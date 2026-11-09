// Package payload reads and validates a provider payload: the OCI image a
// partner publishes containing their Cluster API controller, its components
// bundle, the manifest templates and the install hooks.
//
// The installer validates the payload's structure, not its content. It checks
// that every path the payload declares is present, that the controller is
// executable, that the payload carries no executable it did not declare, and
// that the declared compatibility range admits the release being installed.
// It does not inspect, scan or attest the controller, the manifests or the
// hooks: what is inside is established by the provider's own release process,
// and that is the trust boundary.
package payload

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"sigs.k8s.io/yaml"

	"github.com/openshift/installer/pkg/types/external"
)

const (
	// APIVersion is the only descriptor API version this installer accepts.
	APIVersion = "external.openshift.io/v1alpha1"
	// Kind is the descriptor's kind.
	Kind = "ExternalProviderPayload"
)

// The tokens the installer renders into the controller's command line before
// it execs it (pkg/clusterapi/system.go:695-710). They are how a payload
// declares a flag whose value only the installer knows, and they are the only
// templating the descriptor supports.
const (
	// TokenWebhookPort and TokenWebhookCertDir are each required exactly once.
	// A controller whose webhook disagrees with the one the installer
	// installed fails late and obscurely, so the requirement is on the token
	// rather than on any particular flag name -- a provider spells the flag
	// however their binary spells it.
	TokenWebhookPort    = "{{.WebhookPort}}"    // #nosec G101 // a template token for a directory path, not a credential
	TokenWebhookCertDir = "{{.WebhookCertDir}}" // #nosec G101 // a template token for a directory path, not a credential

	// TokenHealthHostPort is optional. Omitting it means the payload's
	// controller publishes no health endpoint the installer knows the address
	// of, and the installer does not poll /healthz.
	TokenHealthHostPort = "{{suggestHealthHostPort}}"
)

// Descriptor is the content of openshift.yaml at the payload root.
//
// It is shaped like a Kubernetes object -- apiVersion, kind, metadata, spec --
// because that is the shape every reader of this file already knows, and
// because it leaves room for a status or a second version without the document
// changing shape. Nothing serves it from an API server and nothing reconciles
// it; the resemblance is to the reader's expectations, not to a controller.
type Descriptor struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`

	Metadata Metadata `json:"metadata"`
	Spec     Spec     `json:"spec"`
}

// Metadata identifies the payload.
type Metadata struct {
	// Name must equal platform.external.platformName in the install-config.
	// Binding the two is what makes an install attributable: platformName is
	// the value that reaches Infrastructure.status.platformStatus.external,
	// so the cluster carries a record of which provider installed it, and the
	// installer needs no third spelling of the same string.
	Name string `json:"name"`

	// Labels are informational and never read to decide behaviour. They are
	// written to the install log, so they must carry no credential.
	// +optional
	Labels map[string]string `json:"labels,omitempty"`
}

// Spec is everything the payload declares about itself.
type Spec struct {
	// Compatibility is the range of releases the provider claims this payload
	// supports. The Cluster API contract is not repeated here: it comes from
	// the adjacent clusterctl metadata.yaml, whose semantics are defined
	// upstream rather than by us.
	Compatibility Compatibility `json:"compatibility"`

	// CAPI is everything Cluster API needs to run this provider.
	CAPI CAPI `json:"capi"`

	// OpenShift is what the installer does beyond Cluster API. A consumer that
	// is not the installer reads CAPI and ignores this, which is why the two
	// are separate rather than one flat layout.
	OpenShift OpenShift `json:"openshift"`
}

// Compatibility declares what the payload supports.
type Compatibility struct {
	Kubernetes VersionRange `json:"kubernetes"`
}

// VersionRange is an inclusive "major.minor" range.
type VersionRange struct {
	Min string `json:"min"`
	Max string `json:"max"`
}

// CAPI names the Cluster API artifacts, by path relative to the payload root.
type CAPI struct {
	// Controller is the provider controller executable.
	Controller string `json:"controller"`

	// ControllerArgs is the controller's whole command line, not an addition
	// to one.
	//
	// The installer used to build a fixed list and append the provider's,
	// which meant a provider could add a flag and never remove one. CAPOCI
	// declares --health-probe-bind-address where the installer injected
	// --health-addr, pflag exits non-zero on an unknown flag, and the only
	// recourse was a wrapper script in front of the real binary. Every
	// integrated provider already owns its full list (pkg/clusterapi/system.go:193
	// for AWS, :381 for Nutanix), so this aligns the external path with the
	// shape that already exists rather than adding a second mechanism.
	//
	// --kubeconfig must not be declared: the installer appends it.
	// +optional
	ControllerArgs []string `json:"controllerArgs,omitempty"`

	// InfrastructureComponents is the single components bundle -- the
	// provider's CRDs and webhook configurations. Only those two kinds are
	// installed: the installer runs the controller as a local process, so any
	// Namespace, RBAC or Deployment in the bundle is inert.
	InfrastructureComponents string `json:"infrastructureComponents"`

	// ClusterTemplates holds Cluster API objects applied to the installer's
	// local, ephemeral control plane. Nothing in this tree reaches the
	// installed cluster.
	//
	// It is a separate key from OpenShift.ManifestTemplates, rather than one
	// rendered directory, because the two have different destinations and
	// different lifetimes. Rendering is the only thing they have in common.
	// +optional
	ClusterTemplates string `json:"clusterTemplates,omitempty"`

	// E2E is reserved. A payload may declare a test entry point and the
	// installer validates that it exists and is executable, but nothing runs
	// it: there is no conformance suite to run it against yet, and reserving
	// the key now is cheaper than adding it to a shipped contract later.
	//
	// Because every declared path must exist, a payload that ships no such
	// program must omit this rather than declare an empty one.
	// +optional
	E2E *E2E `json:"e2e,omitempty"`
}

// E2E is the payload's reserved test entry point.
type E2E struct {
	Program string `json:"program"`
}

// OpenShift names the artifacts only the OpenShift installer consumes.
type OpenShift struct {
	// ManifestTemplates holds day-0 manifests folded into the install
	// directory and applied during bootstrap. These do reach the cluster.
	// +optional
	ManifestTemplates string `json:"manifestTemplates,omitempty"`

	// InstallerHooks names the program for each install phase. Several phases
	// may name the same program: a single script that dispatches on the phase
	// is a normal shape, and declaring it three times is how it stays a
	// declared executable rather than an undeclared one.
	//
	// The key carries the consumer's name because these are the installer's
	// phases, not Cluster API's -- a reader who is not the installer can skip
	// the whole stanza knowing it is not addressed to them.
	// +optional
	InstallerHooks *InstallerHooks `json:"installerHooks,omitempty"`
}

// InstallerHooks declares the hook for each phase.
//
// Every entry is an object rather than a bare path, so the map has one shape
// whether or not a phase needs a second field. Only Ignition needs one
// today.
type InstallerHooks struct {
	// Ignition runs after the cluster's infrastructure reports ready and
	// before any machine is created, at the installer's IgnitionProvider
	// point of extension. It is conditional, and most payloads will not
	// declare it: it exists for a provider whose controller cannot stage an
	// oversized bootstrap ignition itself.
	//
	// It is the only hook handed the cluster's day-0 secret material, so
	// declaring it requires the cluster creator's consent through
	// platform.external.clusterAPI.bootstrapIgnitionStaging.
	// +optional
	Ignition *HookDeclaration `json:"ignition,omitempty"`

	// +optional
	InfraReady *HookDeclaration `json:"infraReady,omitempty"`
	// +optional
	PostProvision *HookDeclaration `json:"postProvision,omitempty"`
	// +optional
	PreDestroy *HookDeclaration `json:"preDestroy,omitempty"`
}

// HookDeclaration is one phase's program, and whatever else that phase needs.
type HookDeclaration struct {
	// Program is the executable, relative to the payload root.
	Program string `json:"program"`

	// BootstrapOutputPath is the file the installer reads back after the hook
	// returns, relative to the directory the installer creates for the hook
	// and removes afterwards. It is required on Ignition and rejected on
	// every other phase.
	//
	// Declared rather than fixed so that the installer has a name to validate
	// and a name to put in a failure message. It must not be "bootstrap.ign":
	// that name belongs to the installer's own output, which this hook never
	// touches, and two files with one name would leave no way to tell which of
	// them a later step consumed.
	// +optional
	BootstrapOutputPath string `json:"bootstrapOutputPath,omitempty"`
}

// bootstrapIgnitionFile is the installer's own bootstrap ignition filename,
// which a hook's output must not shadow.
const bootstrapIgnitionFile = "bootstrap.ign"

// Hooks returns the declared hooks, or an empty set when none are declared, so
// callers can read a phase without a nil guard.
func (s Spec) Hooks() InstallerHooks {
	if s.OpenShift.InstallerHooks == nil {
		return InstallerHooks{}
	}
	return *s.OpenShift.InstallerHooks
}

// byPhase pairs each declared hook with the phase name used in messages. The
// order is the order the phases run in.
func (h InstallerHooks) byPhase() []struct {
	Phase string
	Hook  *HookDeclaration
} {
	return []struct {
		Phase string
		Hook  *HookDeclaration
	}{
		{"ignition", h.Ignition},
		{"infraReady", h.InfraReady},
		{"postProvision", h.PostProvision},
		{"preDestroy", h.PreDestroy},
	}
}

// Load reads and validates the descriptor at the root of an extracted
// payload. It does not touch the artifacts the descriptor names; call
// Validate for that.
func Load(root string) (*Descriptor, error) {
	path := filepath.Join(root, external.PayloadDescriptorFile)
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("provider payload has no %s at its root: every payload must declare its layout, "+
				"because the installer resolves artifacts from the declaration rather than by guessing at filenames",
				external.PayloadDescriptorFile)
		}
		return nil, fmt.Errorf("failed to read %s: %w", path, err)
	}

	d := &Descriptor{}
	// Strict: an unknown field is a payload written against a different
	// contract, and silently ignoring it would mean honouring a layout the
	// provider did not intend.
	if err := yaml.UnmarshalStrict(data, d); err != nil {
		return nil, fmt.Errorf("failed to parse %s: %w", external.PayloadDescriptorFile, err)
	}

	if d.APIVersion != APIVersion {
		return nil, fmt.Errorf("provider payload declares apiVersion %q, but this installer understands only %q",
			d.APIVersion, APIVersion)
	}
	if d.Kind != Kind {
		return nil, fmt.Errorf("provider payload declares kind %q, expected %q", d.Kind, Kind)
	}
	if d.Metadata.Name == "" {
		return nil, fmt.Errorf("%s declares no metadata.name; it is required and must equal "+
			"platform.external.platformName in the install-config", external.PayloadDescriptorFile)
	}
	if d.Spec.CAPI.Controller == "" {
		return nil, fmt.Errorf("%s declares no spec.capi.controller", external.PayloadDescriptorFile)
	}
	if d.Spec.CAPI.InfrastructureComponents == "" {
		return nil, fmt.Errorf("%s declares no spec.capi.infrastructureComponents", external.PayloadDescriptorFile)
	}
	if err := d.validateControllerArgs(); err != nil {
		return nil, err
	}
	if err := d.validateHookDeclarations(); err != nil {
		return nil, err
	}
	return d, nil
}

// validateControllerArgs checks the command line the payload owns.
//
// Here rather than in Validate because it needs only the document: a
// descriptor whose controller could never be started correctly should be
// refused as soon as it is read, not after an image has been extracted.
func (d *Descriptor) validateControllerArgs() error {
	args := d.Spec.CAPI.ControllerArgs

	for _, token := range []string{TokenWebhookPort, TokenWebhookCertDir} {
		n := 0
		for _, a := range args {
			n += strings.Count(a, token)
		}
		if n != 1 {
			return fmt.Errorf("%s: spec.capi.controllerArgs contains the %s token %d times, expected exactly once. "+
				"The installer renders the webhook server it installed into that token, and a controller serving "+
				"its webhook anywhere else is one the local control plane cannot reach",
				external.PayloadDescriptorFile, token, n)
		}
	}

	for _, a := range args {
		if name, _, _ := strings.Cut(a, "="); name == "--kubeconfig" {
			return fmt.Errorf("%s: spec.capi.controllerArgs declares --kubeconfig, which the installer appends "+
				"itself; the payload cannot know the path of a control plane that does not exist when it is built",
				external.PayloadDescriptorFile)
		}
	}
	return nil
}

// validateHookDeclarations checks each declared phase, and the one field that
// belongs to exactly one of them.
func (d *Descriptor) validateHookDeclarations() error {
	for _, h := range d.Spec.Hooks().byPhase() {
		if h.Hook == nil {
			continue
		}
		key := "spec.openshift.installerHooks." + h.Phase
		if h.Hook.Program == "" {
			return fmt.Errorf("%s: %s declares no program", external.PayloadDescriptorFile, key)
		}
		if h.Phase != "ignition" {
			if h.Hook.BootstrapOutputPath != "" {
				return fmt.Errorf("%s: %s declares bootstrapOutputPath, which only ignition may declare: "+
					"it is the one phase whose output the installer reads back",
					external.PayloadDescriptorFile, key)
			}
			continue
		}
		out := h.Hook.BootstrapOutputPath
		if out == "" {
			return fmt.Errorf("%s: %s declares no bootstrapOutputPath. It is required: the installer reads that "+
				"file back in place of its own bootstrap ignition, so it needs a name to validate and a name to "+
				"put in a failure message", external.PayloadDescriptorFile, key)
		}
		if _, err := filepath.Localize(out); err != nil {
			return fmt.Errorf("%s: %s bootstrapOutputPath %q must be a relative path inside the directory the "+
				"installer creates for the hook", external.PayloadDescriptorFile, key, out)
		}
		if filepath.Clean(out) == bootstrapIgnitionFile {
			return fmt.Errorf("%s: %s bootstrapOutputPath must not be %q, which is the installer's own output: "+
				"two files with one name leave no way to tell which of them a later step consumed",
				external.PayloadDescriptorFile, key, bootstrapIgnitionFile)
		}
	}
	return nil
}

// declaredPaths returns every path the layout names, cleaned and deduplicated.
func (d *Descriptor) declaredPaths() []string {
	seen := map[string]struct{}{}
	add := func(p string) {
		if p == "" {
			return
		}
		seen[filepath.Clean(p)] = struct{}{}
	}
	add(d.Spec.CAPI.Controller)
	add(d.Spec.CAPI.InfrastructureComponents)
	add(d.Spec.CAPI.ClusterTemplates)
	add(d.Spec.OpenShift.ManifestTemplates)
	if e2e := d.Spec.CAPI.E2E; e2e != nil {
		add(e2e.Program)
	}
	for _, h := range d.Spec.Hooks().byPhase() {
		if h.Hook != nil {
			add(h.Hook.Program)
		}
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// executables returns the cleaned set of paths the layout declares as
// programs: the controller, and each hook that is named.
func (d *Descriptor) executables() map[string]struct{} {
	out := map[string]struct{}{filepath.Clean(d.Spec.CAPI.Controller): {}}
	// The e2e program is never run by the installer, but it is declared, and
	// the declaration is what decides what may be executable. Leaving it out
	// would ship it unexecutable and make the reservation useless to whatever
	// eventually runs it.
	if e2e := d.Spec.CAPI.E2E; e2e != nil && e2e.Program != "" {
		out[filepath.Clean(e2e.Program)] = struct{}{}
	}
	for _, h := range d.Spec.Hooks().byPhase() {
		if h.Hook != nil && h.Hook.Program != "" {
			out[filepath.Clean(h.Hook.Program)] = struct{}{}
		}
	}
	return out
}

// ApplyExecutableModes makes the declared programs executable, and nothing
// else.
//
// This exists because `oc image extract` writes every regular file 0640
// whatever its mode in the image. Its --preserve-ownership flag would keep
// the mode, but it also calls lchown and therefore fails for any user who is
// not root, which the installer is not.
//
// Restoring the bit from the declaration rather than from the image is the
// part worth being deliberate about. It means the installer decides what is
// executable, and it decides it from the one document the payload publishes
// for that purpose. A program the payload ships without declaring stays
// unexecutable on disk, so the descriptor is not merely a description of what
// can run -- it is what makes it able to run.
//
// Containment was already established by Validate's filepath.Localize check
// on every declared path, and no symlink is extracted, so each join below
// stays inside root.
func (d *Descriptor) ApplyExecutableModes(root string) error {
	for rel := range d.executables() {
		path := filepath.Join(root, rel)
		info, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("provider payload declares executable %q, which is not present in the image", rel)
		}
		if info.IsDir() {
			return fmt.Errorf("provider payload declares executable %q, which is a directory", rel)
		}
		if err := os.Chmod(path, info.Mode().Perm()|0o100); err != nil {
			return fmt.Errorf("failed to make the provider payload's %q executable: %w", rel, err)
		}
	}
	return nil
}

// Validate checks the extracted payload against its own declaration.
//
// Exhaustive by design, and decided before publication rather than after:
// tightening this rule later would break every partner already shipping. Three
// properties are enforced.
//
//   - Every declared path exists, and is a file or a directory as its role
//     requires.
//   - The controller and every declared hook are executable.
//   - Nothing executable is present that the payload did not declare. This is
//     what makes "the installer validates structure, not content" a guarantee
//     rather than a slogan: a payload cannot carry unannounced executables.
func (d *Descriptor) Validate(root string) error {
	// Containment first. A declared path that escapes the payload root would
	// have the installer run or read something that did not come out of the
	// image, which defeats the point of a payload being one unit of trust.
	declared := d.declaredPaths()
	for _, rel := range declared {
		if _, err := filepath.Localize(rel); err != nil {
			return fmt.Errorf("%s declares path %q, which is not a relative path inside the payload",
				external.PayloadDescriptorFile, rel)
		}
	}

	mustExist := func(rel, role string, wantDir bool) error {
		if rel == "" {
			return nil
		}
		info, err := os.Stat(filepath.Join(root, rel))
		if err != nil {
			return fmt.Errorf("provider payload declares %s at %q, which is not present in the image", role, rel)
		}
		if wantDir && !info.IsDir() {
			return fmt.Errorf("provider payload declares %s at %q, which must be a directory", role, rel)
		}
		if !wantDir && info.IsDir() {
			return fmt.Errorf("provider payload declares %s at %q, which must be a file", role, rel)
		}
		return nil
	}

	for _, f := range []struct {
		rel     string
		role    string
		wantDir bool
	}{
		{d.Spec.CAPI.Controller, "spec.capi.controller", false},
		{d.Spec.CAPI.InfrastructureComponents, "spec.capi.infrastructureComponents", false},
		{d.Spec.CAPI.ClusterTemplates, "spec.capi.clusterTemplates", true},
		{d.Spec.OpenShift.ManifestTemplates, "spec.openshift.manifestTemplates", true},
	} {
		if err := mustExist(f.rel, f.role, f.wantDir); err != nil {
			return err
		}
	}
	if e2e := d.Spec.CAPI.E2E; e2e != nil {
		if err := mustExist(e2e.Program, "spec.capi.e2e.program", false); err != nil {
			return err
		}
	}
	for _, h := range d.Spec.Hooks().byPhase() {
		if h.Hook == nil {
			continue
		}
		if err := mustExist(h.Hook.Program, "spec.openshift.installerHooks."+h.Phase+".program", false); err != nil {
			return err
		}
	}

	executables := d.executables()
	for rel := range executables {
		info, err := os.Stat(filepath.Join(root, rel))
		if err != nil {
			return fmt.Errorf("provider payload declares executable %q, which is not present in the image", rel)
		}
		if info.Mode().Perm()&0o111 == 0 {
			return fmt.Errorf("provider payload declares %q as an executable, but it is not executable (mode %s)",
				rel, info.Mode().Perm())
		}
	}

	// The undeclared-executable sweep. Walking the whole payload is the point:
	// an executable in a template directory is exactly the case this rule
	// exists to reject.
	//
	// What this rule rests on changed when extraction moved to
	// `oc image extract`, and the change is worth stating because the check
	// reads the same and no longer means the same. `oc` writes every regular
	// file 0640 regardless of its mode in the image, so the payload's own
	// permission bits never reach the disk and this walk can no longer
	// observe them. The only files with an executable bit are the ones
	// ApplyExecutableModes just set, from the declaration.
	//
	// So the guarantee inverted, and strengthened. It used to be "nothing
	// undeclared was found to be executable", a detection that depended on
	// the image's bits surviving. It is now "nothing undeclared is
	// executable", which holds by construction: an undeclared program in the
	// payload lands unexecutable and a hook would have to chmod it on purpose
	// to run it. The walk stays because it is what catches this package
	// getting the declared set wrong -- a bug that would otherwise leave a
	// file executable with nothing to notice.
	var undeclared []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&fs.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return nil
		}
		if info.Mode().Perm()&0o111 == 0 {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if _, ok := executables[filepath.Clean(rel)]; !ok {
			undeclared = append(undeclared, rel)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("failed to inspect the provider payload at %s: %w", root, err)
	}
	if len(undeclared) > 0 {
		sort.Strings(undeclared)
		return fmt.Errorf("provider payload carries executables it does not declare: %s. "+
			"Declare them in %s or remove them; the installer rejects an undeclared executable so that the "+
			"payload's declaration is a complete description of what it can run",
			strings.Join(undeclared, ", "), external.PayloadDescriptorFile)
	}
	return nil
}

// CheckCompatibility reports whether the payload admits the given Kubernetes
// version, which is the version of the release payload being installed.
//
// Comparison is on major.minor only. A patch-level claim would be a promise no
// provider can keep: they cannot test against a z-stream that does not exist
// yet, and refusing an install on one would be a false negative.
func (d *Descriptor) CheckCompatibility(kubeVersion string) error {
	have, err := parseMajorMinor(kubeVersion)
	if err != nil {
		return fmt.Errorf("cannot compare the provider payload's compatibility range against the cluster's "+
			"Kubernetes version %q: %w", kubeVersion, err)
	}
	rng := d.Spec.Compatibility.Kubernetes
	if rng.Min == "" && rng.Max == "" {
		return errors.New("provider payload declares no compatibility.kubernetes range; a payload must state " +
			"which releases it supports, because the installer has no other way to refuse an unsupported pairing")
	}
	if rng.Min != "" {
		lowest, err := parseMajorMinor(rng.Min)
		if err != nil {
			return fmt.Errorf("provider payload declares an unparseable compatibility.kubernetes.min %q: %w", rng.Min, err)
		}
		if compareMajorMinor(have, lowest) < 0 {
			return fmt.Errorf("provider payload supports Kubernetes %s to %s, but this release is %s",
				rng.Min, rng.Max, kubeVersion)
		}
	}
	if rng.Max != "" {
		highest, err := parseMajorMinor(rng.Max)
		if err != nil {
			return fmt.Errorf("provider payload declares an unparseable compatibility.kubernetes.max %q: %w", rng.Max, err)
		}
		if compareMajorMinor(have, highest) > 0 {
			return fmt.Errorf("provider payload supports Kubernetes %s to %s, but this release is %s",
				rng.Min, rng.Max, kubeVersion)
		}
	}
	return nil
}

// parseMajorMinor accepts "1.33" and "1.33.4" and "v1.33", ignoring anything
// below the minor. Tolerating the forms callers actually have avoids making
// every call site normalise first and disagree about how.
func parseMajorMinor(v string) ([2]int, error) {
	s := strings.TrimPrefix(strings.TrimSpace(v), "v")
	if s == "" {
		return [2]int{}, errors.New("version is empty")
	}
	parts := strings.SplitN(s, ".", 3)
	if len(parts) < 2 {
		return [2]int{}, fmt.Errorf("%q is not a major.minor version", v)
	}
	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return [2]int{}, fmt.Errorf("%q has a non-numeric major version", v)
	}
	minor, err := strconv.Atoi(parts[1])
	if err != nil {
		return [2]int{}, fmt.Errorf("%q has a non-numeric minor version", v)
	}
	return [2]int{major, minor}, nil
}

func compareMajorMinor(a, b [2]int) int {
	switch {
	case a[0] != b[0]:
		if a[0] < b[0] {
			return -1
		}
		return 1
	case a[1] != b[1]:
		if a[1] < b[1] {
			return -1
		}
		return 1
	}
	return 0
}
