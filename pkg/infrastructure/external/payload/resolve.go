package payload

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/sirupsen/logrus"

	"github.com/openshift/installer/pkg/types"
	"github.com/openshift/installer/pkg/types/external"
)

// Resolution is everything the installer needs from a provider payload, with
// every path absolute and already checked.
//
// It is the only thing the rest of the installer sees of a payload. Nothing
// outside this package reads the descriptor, resolves a layout path or renders
// a template, so the payload contract has exactly one implementation.
type Resolution struct {
	// Image is the reference from the install-config, and Digest is what it
	// resolved to.
	//
	// Digest is set whether this process pulled the payload or reused an
	// extraction a previous command made, because it is provenance and it
	// reaches metadata.json, which travels with a support bundle. It used to
	// be empty on the reuse path, which meant the recorded digest went missing
	// in exactly the normal case: `create manifests` pulls, `create cluster`
	// reuses, and the metadata the second one writes is the metadata that
	// survives. Reused tells the two apart where that matters, which is the
	// install log and nothing else.
	Image  string
	Digest string

	// Reused reports that the payload was already extracted when this process
	// started, so no registry round trip was made.
	Reused bool

	// Root is the directory the payload was extracted into.
	Root string

	// ControllerPath and ComponentsPath are the provider controller and its
	// components bundle, ready to hand to clusterapi.SetExternalProvider.
	ControllerPath string
	ComponentsPath string

	// Args are the controller's extra command-line arguments, taken from
	// whichever of the two install-config spellings was used.
	Args []string

	// Hooks are the hook programs that will run, whether the payload declared
	// them or the install-config named them. Nil when there are none.
	Hooks *external.Hooks

	// HookRoots is the directory each hook's Program is resolved against,
	// relative to the install directory, phase by phase. Empty for a phase
	// with no hook.
	HookRoots HookRoots
}

// HookRoots is the directory each hook's Program is resolved against.
//
// There is one per phase rather than one for the set because a hook's root
// follows its origin, and the two origins are different directories: a program
// the payload declared lives in the extraction directory, one the
// install-config named lives in the External manifest directory. A single root
// can only be right for a payload whose hooks are all overridden or all not.
type HookRoots struct {
	InfraReady    string
	PostProvision string
	PreDestroy    string
}

// ResolveOptions is the input to Resolve.
type ResolveOptions struct {
	// Provider is the install-config's platform.external.clusterAPI. It must
	// be in image mode; Resolve refuses a local-mode provider rather than
	// quietly doing nothing, because a caller that reached here believing
	// otherwise has a bug.
	Provider *external.ClusterAPIProvider

	// InstallDir is the install directory. It need not be absolute.
	InstallDir string

	// The cluster's identity, published to the templates as the INSTALLER_
	// variables. None of these is a credential.
	InfraID     string
	ClusterName string
	BaseDomain  string
	Publish     string

	// PullSecret authenticates the payload pull. It is never logged and never
	// written to the install directory.
	PullSecret string

	// Mirrors is imageDigestSources, flattened to source -> mirrors.
	Mirrors map[string][]string
}

// resolved caches the result for the process.
//
// Four call sites need the payload and they run at different stages of one
// install: the openshift manifests asset renders the day-0 manifests, the
// Cluster API manifest provider reads the rendered cluster templates,
// PreProvision starts the controller, and the metadata asset records where
// everything came from. Pulling four times would be four registry round trips
// for one answer, and -- worse -- four chances for a floating tag to resolve
// to a different image partway through a single install.
var resolved struct {
	sync.Mutex
	res *Resolution
}

// Resolved returns the resolution made earlier in this process, or nil.
//
// It exists for callers that must not trigger a pull of their own: the
// metadata asset records what was resolved, and recording it is not a reason
// to go to a registry.
func Resolved() *Resolution {
	resolved.Lock()
	defer resolved.Unlock()
	return resolved.res
}

// stateFile records what was extracted into an install directory, so that a
// second command against the same directory reuses it instead of pulling
// again. `create manifests` then `create cluster` is two processes and one
// payload, and `destroy cluster` must work with no registry at all.
const stateFile = ".openshift_install_payload.json"

// payloadState is the content of stateFile. Locations and a digest only --
// never a credential, never the contents of a file -- because the install
// directory travels with support bundles.
type payloadState struct {
	Image  string `json:"image"`
	Digest string `json:"digest"`
	Root   string `json:"root"`
}

// Resolve makes a payload usable: it pulls and extracts the image if it is not
// already present, validates the extracted tree against its own declaration,
// checks the compatibility range, and renders both template trees into the
// install directory.
//
// It is idempotent and safe to call from every stage that needs it. The
// rendering is redone on each call, because it is local and cheap and because
// a half-rendered tree left by an interrupted run should not survive; the pull
// is not, because it is neither.
func Resolve(ctx context.Context, opts ResolveOptions) (*Resolution, error) {
	resolved.Lock()
	defer resolved.Unlock()
	if resolved.res != nil {
		return resolved.res, nil
	}

	p := opts.Provider
	if p == nil {
		return nil, errors.New("no platform.external.clusterAPI is configured")
	}
	if !p.UsesImagePayload() {
		return nil, errors.New("platform.external.clusterAPI does not use a payload image")
	}

	installDir, err := filepath.Abs(opts.InstallDir)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve the install directory %s: %w", opts.InstallDir, err)
	}
	root, err := payloadRoot(installDir, p.CapiInstallerPath)
	if err != nil {
		return nil, err
	}

	digest, reused, err := obtain(ctx, opts, root, installDir)
	if err != nil {
		return nil, err
	}

	descriptor, err := Load(root)
	if err != nil {
		return nil, err
	}
	// Before Validate, not after: extraction does not preserve the executable
	// bit, so the declared programs are not executable until this runs and
	// Validate's check of them would fail on every payload. Running it on a
	// reused extraction too is deliberate -- it is idempotent, and it is the
	// cheapest way to not depend on how the directory got there.
	if err := descriptor.ApplyExecutableModes(root); err != nil {
		return nil, err
	}
	if err := descriptor.Validate(root); err != nil {
		return nil, err
	}
	if err := descriptor.CheckCompatibility(KubernetesVersion); err != nil {
		return nil, err
	}

	res := &Resolution{
		Image:          p.Controller.Image,
		Digest:         digest,
		Reused:         reused,
		Root:           root,
		ControllerPath: filepath.Join(root, descriptor.Layout.Controller),
		Args:           controllerArgs(p),
	}

	// The install-config may name a different components bundle or manifest
	// directory than the payload declares. Both are resolved against the
	// payload root and must stay inside it: an override selects between things
	// the payload shipped, it does not reach outside the unit of trust.
	components, err := overridePath(root, descriptor.Layout.CustomResource, p.CustomResource, "customResource")
	if err != nil {
		return nil, err
	}
	res.ComponentsPath = components

	manifestTemplates, err := overridePath(root, descriptor.Layout.ManifestTemplates, p.ManifestPath, "manifestPath")
	if err != nil {
		return nil, err
	}

	if err := renderTrees(root, installDir, descriptor, manifestTemplates, opts); err != nil {
		return nil, err
	}

	res.Hooks, res.HookRoots = payloadHooks(descriptor, p, installDir, root)

	logrus.Infof("Using the %s Cluster API provider payload from %s: controller %s, components %s",
		p.Name, describeSource(res), res.ControllerPath, res.ComponentsPath)

	resolved.res = res
	return res, nil
}

// describeSource names the payload, and says whether this run resolved the
// digest or read it from the previous command's state file.
//
// Both carry the digest, because it is the same payload either way and a
// support log that omits it on the second of two commands is the one that
// gets read. The wording keeps the distinction that matters: "already
// extracted" is not a claim that the reference still resolves there now.
func describeSource(res *Resolution) string {
	if res.Digest == "" {
		return res.Image
	}
	if res.Reused {
		return fmt.Sprintf("%s (%s, already extracted)", res.Image, res.Digest)
	}
	return fmt.Sprintf("%s (%s)", res.Image, res.Digest)
}

// controllerArgs returns the extra controller arguments from whichever
// spelling the install-config used. Validation has already refused both at
// once, so there is nothing to merge.
func controllerArgs(p *external.ClusterAPIProvider) []string {
	if len(p.Args) > 0 {
		return append([]string(nil), p.Args...)
	}
	if p.Controller != nil {
		return append([]string(nil), p.Controller.Args...)
	}
	return nil
}

// payloadRoot resolves the extraction directory, defaulting to the
// "capi-installer" directory of the install directory.
//
// A configured path is taken relative to the install directory and must stay
// inside it. Allowing an absolute path elsewhere on the host would put the
// artifacts a `destroy cluster` depends on outside the directory the user
// keeps, which is how an undeletable cluster is made.
func payloadRoot(installDir, configured string) (string, error) {
	if configured == "" {
		return filepath.Join(installDir, external.CapiInstallerDir), nil
	}
	local, err := filepath.Localize(configured)
	if err != nil {
		return "", fmt.Errorf("platform.external.clusterAPI.capiInstallerPath %q must be a relative path "+
			"inside the install directory, so that the extracted payload travels with it: %w", configured, err)
	}
	return filepath.Join(installDir, local), nil
}

// overridePath picks the install-config's path over the payload's declaration,
// and resolves whichever won against the payload root.
func overridePath(root, declared, configured, field string) (string, error) {
	rel := declared
	if configured != "" {
		local, err := filepath.Localize(configured)
		if err != nil {
			return "", fmt.Errorf("platform.external.clusterAPI.%s %q must be a relative path inside the "+
				"provider payload: %w", field, configured, err)
		}
		rel = local
	}
	if rel == "" {
		return "", nil
	}
	path := filepath.Join(root, rel)
	if _, err := os.Stat(path); err != nil {
		return "", fmt.Errorf("the provider payload has no %s at %q", field, rel)
	}
	return path, nil
}

// obtain makes sure the payload is extracted at root, pulling it only when it
// is not already there for this image reference. It returns the digest and
// whether the extraction was reused rather than made.
//
// The digest comes from the state file on the reuse path. That is a weaker
// claim than a digest this process resolved -- it says what was pulled into
// this directory earlier, not what the reference resolves to now -- but it is
// the right one to record, because what ran is what was extracted, not what
// the tag points at today.
func obtain(ctx context.Context, opts ResolveOptions, root, installDir string) (string, bool, error) {
	ref := opts.Provider.Controller.Image
	statePath := filepath.Join(installDir, stateFile)

	if state, err := readState(statePath); err == nil && state.Image == ref && state.Root == root {
		if _, err := os.Stat(filepath.Join(root, external.PayloadDescriptorFile)); err == nil {
			logrus.Infof("Reusing the provider payload already extracted into %s (%s)", root, state.Digest)
			return state.Digest, true, nil
		}
	}

	// A stale extraction is removed rather than overlaid. Extraction writes
	// files without removing ones the new image does not have, so a payload
	// swapped for another would leave the first one's hooks and templates in
	// place -- and the undeclared-executable check would then reject the
	// result, naming files the user never put there.
	if err := os.RemoveAll(root); err != nil {
		return "", false, fmt.Errorf("failed to clear the provider payload directory %s: %w", root, err)
	}

	digest, err := Fetch(ctx, ref, root, opts.PullSecret, opts.Mirrors)
	if err != nil {
		return "", false, err
	}
	if err := writeState(statePath, payloadState{Image: ref, Digest: digest, Root: root}); err != nil {
		return "", false, err
	}
	return digest, false, nil
}

func readState(path string) (payloadState, error) {
	state := payloadState{}
	data, err := os.ReadFile(path)
	if err != nil {
		return state, err
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return state, err
	}
	return state, nil
}

func writeState(path string, state payloadState) error {
	data, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("failed to record the provider payload source: %w", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil { //nolint:gosec // locations only, no secret.
		return fmt.Errorf("failed to write %s: %w", path, err)
	}
	return nil
}

// renderTrees writes the payload's rendered templates where the installer
// already looks for user-supplied ones.
//
// Rendering into the existing directories rather than teaching the readers
// about payloads is deliberate. The Cluster API manifest provider and the
// openshift extra-manifest reader are the same code for both modes, so a
// payload cannot produce an install the installer treats differently from one
// a user hand-wrote -- which is the property that makes the two CI arms
// comparable.
func renderTrees(root, installDir string, d *Descriptor, manifestTemplates string, opts ResolveOptions) error {
	vars := variables(opts)

	if d.Layout.ClusterTemplates != "" {
		src := filepath.Join(root, d.Layout.ClusterTemplates)
		dst := filepath.Join(installDir, external.ManifestDir)
		if err := Render(src, dst, vars); err != nil {
			return err
		}
		logrus.Infof("Rendered the payload's Cluster API templates into %s", dst)
	}

	if manifestTemplates != "" {
		dst := filepath.Join(installDir, external.ManifestDir, external.ExtraManifestDir)
		if err := Render(manifestTemplates, dst, vars); err != nil {
			return err
		}
		logrus.Infof("Rendered the payload's day-0 manifest templates into %s", dst)
	}
	return nil
}

// variables is the substitution environment: the installer's own set, then the
// user's, which cannot collide because validation reserves the INSTALLER_
// prefix.
//
// The installer's set is deliberately small and is only what it knows without
// knowing the provider's cloud. Adding a variable is a compatible change;
// renaming one is not.
func variables(opts ResolveOptions) map[string]string {
	vars := map[string]string{
		InstallerVariablePrefix + "INFRA_ID":       opts.InfraID,
		InstallerVariablePrefix + "CLUSTER_NAME":   opts.ClusterName,
		InstallerVariablePrefix + "BASE_DOMAIN":    opts.BaseDomain,
		InstallerVariablePrefix + "CLUSTER_DOMAIN": clusterDomain(opts),
		InstallerVariablePrefix + "PUBLISH":        opts.Publish,
	}
	for name, value := range opts.Provider.Variables {
		vars[name] = value
	}
	// The names, never the values. A variable is not supposed to carry a
	// credential -- the field documentation says so -- but a log line is the
	// wrong place to find out that someone did it anyway.
	logrus.Debugf("Rendering the provider payload with variables: %s", strings.Join(sortedNames(vars), ", "))
	return vars
}

func clusterDomain(opts ResolveOptions) string {
	if opts.ClusterName == "" || opts.BaseDomain == "" {
		return ""
	}
	return opts.ClusterName + "." + opts.BaseDomain
}

func sortedNames(vars map[string]string) []string {
	names := make([]string, 0, len(vars))
	for name := range vars {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// payloadHooks decides, phase by phase, which program runs and which directory
// it is resolved against.
//
// Phase by phase is the point. The install-config's own hook wins when it names
// one, so a user can point a phase at their own script without rebuilding the
// payload -- and the root follows the winner, because a program the payload
// declared lives in the extraction directory and one the user named lives in
// the External manifest directory.
//
// An earlier version returned a single root for the whole set, which made
// overriding one hook silently move the others: a user who replaced
// postProvision alone left the payload's own infraReady program being looked
// for under the External manifest directory, where it does not exist, and the
// install failed on a hook that nothing was wrong with. Nothing in the
// install-config said that would happen, and nothing in the error said why.
//
// A phase with neither a declaration nor an override is left nil, which is how
// every caller already spells "skip". That is the ordinary case for a payload
// that ships, say, only an infraReady hook, and it is not worth a warning here
// -- the warning belongs where the consequence is known, next to the phase that
// will not run.
func payloadHooks(d *Descriptor, p *external.ClusterAPIProvider, installDir, root string) (*external.Hooks, HookRoots) {
	payloadRoot, err := filepath.Rel(installDir, root)
	if err != nil {
		// root was built by joining installDir, so this cannot happen; falling
		// back to the default keeps a would-be panic out of the install path.
		payloadRoot = external.CapiInstallerDir
	}

	var declared LayoutHooks
	if d.Layout.Hooks != nil {
		declared = *d.Layout.Hooks
	}
	configured := p.Hooks

	hooks := &external.Hooks{}
	roots := HookRoots{}
	for _, h := range []struct {
		phase    string
		declared string
		override *external.Hook
		dst      **external.Hook
		dstRoot  *string
	}{
		{"infraReady", declared.InfraReady,
			hookOrNil(configured, func(h *external.Hooks) *external.Hook { return h.InfraReady }),
			&hooks.InfraReady, &roots.InfraReady},
		{"postProvision", declared.PostProvision,
			hookOrNil(configured, func(h *external.Hooks) *external.Hook { return h.PostProvision }),
			&hooks.PostProvision, &roots.PostProvision},
		{"preDestroy", declared.PreDestroy,
			hookOrNil(configured, func(h *external.Hooks) *external.Hook { return h.PreDestroy }),
			&hooks.PreDestroy, &roots.PreDestroy},
	} {
		switch {
		case h.override != nil:
			*h.dst = h.override.DeepCopy()
			*h.dstRoot = external.ManifestDir
			if h.declared != "" {
				logrus.Warnf("platform.external.clusterAPI.hooks.%s replaces the %s hook the provider "+
					"payload declares (%s). The payload's other hooks are unaffected and still run from %q; "+
					"yours runs from %q.",
					h.phase, h.phase, h.declared, payloadRoot, external.ManifestDir)
			}
		case h.declared != "":
			*h.dst = &external.Hook{Program: h.declared}
			*h.dstRoot = payloadRoot
		}
	}

	if !anyHook(hooks) {
		return nil, HookRoots{}
	}
	return hooks, roots
}

func hookOrNil(h *external.Hooks, pick func(*external.Hooks) *external.Hook) *external.Hook {
	if h == nil {
		return nil
	}
	return pick(h)
}

func anyHook(h *external.Hooks) bool {
	return h != nil && (h.InfraReady != nil || h.PostProvision != nil || h.PreDestroy != nil)
}

// ResolveForInstallConfig is the entry point every stage of the installer
// uses, so that none of them has its own idea of what a payload resolves to.
//
// It returns (nil, nil) when the platform is not an External install with a
// payload image, which lets a caller ask unconditionally rather than
// repeating the mode test at four call sites and eventually disagreeing about
// it.
func ResolveForInstallConfig(ctx context.Context, ic *types.InstallConfig, installDir, infraID string) (*Resolution, error) {
	if ic == nil || ic.Platform.External == nil {
		return nil, nil
	}
	capi := ic.Platform.External.ClusterAPI
	if !capi.UsesImagePayload() {
		return nil, nil
	}

	mirrors := map[string][]string{}
	for _, source := range ic.ImageDigestSources {
		mirrors[source.Source] = append(mirrors[source.Source], source.Mirrors...)
	}

	return Resolve(ctx, ResolveOptions{
		Provider:    capi,
		InstallDir:  installDir,
		InfraID:     infraID,
		ClusterName: ic.ObjectMeta.Name,
		BaseDomain:  ic.BaseDomain,
		Publish:     string(ic.Publish),
		PullSecret:  ic.PullSecret,
		Mirrors:     mirrors,
	})
}

// Discard forgets the cached resolution. It exists for tests; the installer
// resolves a payload once per process and never needs to.
func Discard() {
	resolved.Lock()
	defer resolved.Unlock()
	resolved.res = nil
}
