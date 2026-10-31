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
	Kind = "ProviderPayload"
)

// Descriptor is the content of openshift.yaml at the payload root.
type Descriptor struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`

	// Compatibility is the range of releases the provider claims this payload
	// supports. The Cluster API contract is not repeated here: it comes from
	// the adjacent clusterctl metadata.yaml, whose semantics are defined
	// upstream rather than by us.
	Compatibility Compatibility `json:"compatibility"`

	// Layout maps each artifact to its path, relative to the payload root.
	Layout Layout `json:"layout"`
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

// Layout names each artifact's path relative to the payload root.
type Layout struct {
	// Controller is the provider controller executable.
	Controller string `json:"controller"`

	// CustomResource is the single components bundle -- the provider's CRDs
	// and webhook configurations. Only those two kinds are installed: the
	// installer runs the controller as a local process, so any Namespace,
	// RBAC or Deployment in the bundle is inert.
	CustomResource string `json:"customResource"`

	// ClusterTemplates holds Cluster API objects applied to the installer's
	// local, ephemeral control plane. Nothing in this tree reaches the
	// installed cluster.
	//
	// It is a separate key from ManifestTemplates, rather than one rendered
	// directory, because the two have different destinations and different
	// lifetimes. Rendering is the only thing they have in common.
	// +optional
	ClusterTemplates string `json:"clusterTemplates,omitempty"`

	// ManifestTemplates holds day-0 manifests folded into the install
	// directory and applied during bootstrap. These do reach the cluster.
	// +optional
	ManifestTemplates string `json:"manifestTemplates,omitempty"`

	// Hooks names the program for each install phase. Several phases may name
	// the same program: a single script that dispatches on the phase is a
	// normal shape, and declaring it three times is how it stays a declared
	// executable rather than an undeclared one.
	// +optional
	Hooks *LayoutHooks `json:"hooks,omitempty"`
}

// LayoutHooks names the hook program for each phase.
type LayoutHooks struct {
	InfraReady    string `json:"infraReady,omitempty"`
	PostProvision string `json:"postProvision,omitempty"`
	PreDestroy    string `json:"preDestroy,omitempty"`
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
	if d.Layout.Controller == "" {
		return nil, fmt.Errorf("%s declares no layout.controller", external.PayloadDescriptorFile)
	}
	if d.Layout.CustomResource == "" {
		return nil, fmt.Errorf("%s declares no layout.customResource", external.PayloadDescriptorFile)
	}
	return d, nil
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
	add(d.Layout.Controller)
	add(d.Layout.CustomResource)
	add(d.Layout.ClusterTemplates)
	add(d.Layout.ManifestTemplates)
	if d.Layout.Hooks != nil {
		add(d.Layout.Hooks.InfraReady)
		add(d.Layout.Hooks.PostProvision)
		add(d.Layout.Hooks.PreDestroy)
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
	out := map[string]struct{}{filepath.Clean(d.Layout.Controller): {}}
	if d.Layout.Hooks != nil {
		for _, rel := range []string{
			d.Layout.Hooks.InfraReady,
			d.Layout.Hooks.PostProvision,
			d.Layout.Hooks.PreDestroy,
		} {
			if rel != "" {
				out[filepath.Clean(rel)] = struct{}{}
			}
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

	if err := mustExist(d.Layout.Controller, "layout.controller", false); err != nil {
		return err
	}
	if err := mustExist(d.Layout.CustomResource, "layout.customResource", false); err != nil {
		return err
	}
	if err := mustExist(d.Layout.ClusterTemplates, "layout.clusterTemplates", true); err != nil {
		return err
	}
	if err := mustExist(d.Layout.ManifestTemplates, "layout.manifestTemplates", true); err != nil {
		return err
	}

	if d.Layout.Hooks != nil {
		for role, rel := range map[string]string{
			"hooks.infraReady":    d.Layout.Hooks.InfraReady,
			"hooks.postProvision": d.Layout.Hooks.PostProvision,
			"hooks.preDestroy":    d.Layout.Hooks.PreDestroy,
		} {
			if err := mustExist(rel, "layout."+role, false); err != nil {
				return err
			}
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
	rng := d.Compatibility.Kubernetes
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
