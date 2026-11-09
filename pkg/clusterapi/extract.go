package clusterapi

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/sirupsen/logrus"

	"github.com/openshift/installer/data"
	"github.com/openshift/installer/pkg/infrastructure/external/payload"
)

// InfrastructureProviderNames returns the extractable provider names, sorted.
//
// The core Cluster API controller and the envtest binaries are deliberately
// absent: the installer unpacks those itself on every run, whatever the
// platform, so they are never something a user needs to supply. So are the
// companion components, which cannot provision a platform alone.
func InfrastructureProviderNames() []string {
	names := make([]string, 0, len(infrastructureProviders))
	for name := range infrastructureProviders {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// ExtractedProvider describes the artifacts written by ExtractProvider.
type ExtractedProvider struct {
	// Name is the provider name, as it should appear in the install-config.
	Name string
	// BinaryPath is the absolute path to the extracted controller binary.
	BinaryPath string
	// ComponentsPath is the absolute path to the extracted components.
	ComponentsPath string
	// BinarySHA256 is the digest of the extracted binary, so that a run can
	// be traced back to an exact build.
	BinarySHA256 string
}

// ExtractProvider writes an infrastructure provider's controller binary and
// its components out of the installer into destDir.
//
// It exists so that the External platform can be exercised with a provider
// that this installer itself builds, ships and tests. Bringing up a
// user-supplied provider mixes two unknowns: whether the External path works
// at all, and whether a particular provider build behaves. Extracting a known
// provider removes the second, which makes a failure attributable.
//
// The artifacts written are exactly what platform.external.clusterAPI expects,
// and pointing that field at them makes the installer take the External path
// using the same binary it would otherwise have used directly. That is the
// sharpest available test of the External path: anything that then differs
// from an integrated install is the External path's doing, not the provider's.
//
// Only the artifact paths and the binary digest are logged. Contents are not.
func ExtractProvider(name, destDir string) (*ExtractedProvider, error) {
	provider, ok := infrastructureProviders[name]
	if !ok {
		// A companion component is a name a user can reasonably arrive at --
		// it appears in build output and in the installer's own logs -- so
		// say what it is rather than calling it unknown.
		if _, isCompanion := companionComponents[name]; isCompanion {
			return nil, fmt.Errorf("%q is a companion component, not a standalone Cluster API "+
				"infrastructure provider: it runs alongside one and cannot provision a platform "+
				"by itself. Infrastructure providers are %v", name, InfrastructureProviderNames())
		}
		return nil, fmt.Errorf("unknown Cluster API infrastructure provider %q: known providers are %v",
			name, InfrastructureProviderNames())
	}

	dir, err := resolveDestDir(destDir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("failed to create %s: %w", dir, err)
	}

	// The components come first even though the binary is the headline
	// artifact. They are a thousandth of the size, and in a development build
	// they are the half that fails -- data.Assets is bound relative to the
	// working directory, not to the binary. Writing 150MB and then failing on
	// a 1MB file leaves the user with output they must not use.
	componentsName := fmt.Sprintf("%s-infrastructure-components.yaml", name)
	componentsPath := filepath.Join(dir, componentsName)
	// The components are embedded assets, not zip entries, and their URI is a
	// slash path regardless of host.
	if err := data.Unpack(componentsPath, "/cluster-api/"+componentsName); err != nil {
		return nil, fmt.Errorf("failed to extract the %s provider components: %w "+
			"(in a development build the installer's data assets are resolved relative to the "+
			"working directory; set OPENSHIFT_INSTALL_DATA to the data/data directory of a checkout)",
			name, err)
	}
	if err := validateComponents(componentsPath); err != nil {
		return nil, fmt.Errorf("the extracted %s provider components are unusable: %w", name, err)
	}

	binaryPath := filepath.Join(dir, fmt.Sprintf("cluster-api-provider-%s", name))

	// Remove any previous extraction first. Extract skips archive entries it
	// does not match and returns nil when it matched none (providers.go:116-120),
	// so without this an installer built with an unpopulated mirror would
	// "succeed" against a leftover file and report a digest for a build it did
	// not just extract -- defeating the one guarantee the digest offers.
	if err := os.Remove(binaryPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("failed to remove the previously extracted %s: %w", binaryPath, err)
	}

	if err := provider.Extract(dir); err != nil {
		return nil, fmt.Errorf("failed to extract the %s provider binary: %w", name, err)
	}
	if _, err := os.Stat(binaryPath); errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("the %s provider was not found in this installer's embedded artifacts: "+
			"this binary was built without Cluster API providers", name)
	}

	// unpackFile passes 0o777 to os.OpenFile (providers.go:142), which applies
	// only when the file is created. The mode is set explicitly so that the
	// result does not depend on what was in the directory beforehand.
	if err := os.Chmod(binaryPath, 0o755); err != nil { //nolint:gosec // must be executable to be of any use
		return nil, fmt.Errorf("failed to make %s executable: %w", binaryPath, err)
	}
	// The same check the install-config path applies (external.go), so the
	// command cannot hand back something the installer will later refuse.
	if err := validateExecutable(binaryPath); err != nil {
		return nil, fmt.Errorf("the extracted %s provider binary is unusable: %w", name, err)
	}

	digest, err := fileSHA256(binaryPath)
	if err != nil {
		return nil, fmt.Errorf("failed to digest %s: %w", binaryPath, err)
	}

	logrus.Infof("Extracted the %s Cluster API infrastructure provider: binary %s (sha256:%s), components %s",
		name, binaryPath, digest, componentsPath)

	return &ExtractedProvider{
		Name:           name,
		BinaryPath:     binaryPath,
		ComponentsPath: componentsPath,
		BinarySHA256:   digest,
	}, nil
}

// resolveDestDir turns the requested destination into an absolute path,
// rejecting the two inputs that would otherwise write a large binary somewhere
// the user did not mean.
func resolveDestDir(destDir string) (string, error) {
	trimmed := strings.TrimSpace(destDir)
	if trimmed == "" {
		// filepath.Abs("") is the working directory, so without this an empty
		// value silently fills whatever directory the user is standing in.
		return "", fmt.Errorf("no destination directory given")
	}
	if strings.HasPrefix(trimmed, "~") {
		// Nothing in the installer expands ~, so this would create a literal
		// directory named "~" under the working directory.
		return "", fmt.Errorf("%q starts with ~, which is not expanded: give an absolute path", destDir)
	}
	dir, err := filepath.Abs(trimmed)
	if err != nil {
		return "", fmt.Errorf("failed to resolve %q: %w", destDir, err)
	}
	return dir, nil
}

// PayloadDescriptor renders an ExternalProviderPayload descriptor naming the
// extracted artifacts, as a starting point for a payload repository.
//
// This used to print an install-config snippet, because
// platform.external.clusterAPI accepted the two paths directly. It no longer
// does: a provider is supplied as one payload image, so extracted artifacts
// are not something to point at any more, they are something to build from.
// The paths below are on this host and a real descriptor's are relative to the
// payload root, which is why they appear in a comment rather than in the
// fields -- pasting this file unedited must not look like it could work.
//
// spec.capi.controllerArgs is the part a copy cannot guess. The list here is
// what this installer passes its own integrated providers; that is right for a
// provider extracted from this installer, and for anything else it is exactly
// the thing to check against the provider's own flags.
func (e *ExtractedProvider) PayloadDescriptor() string {
	return fmt.Sprintf(`apiVersion: %s
kind: %s
metadata:
  # Must equal platform.external.platformName in install-config.yaml.
  name: %s
spec:
  compatibility:
    kubernetes:
      min: "CHANGE-ME"
      max: "CHANGE-ME"
  capi:
    # Relative to the payload root. Extracted on this host to:
    #   controller: %s
    #   components: %s
    controller: bin/controller
    controllerArgs:
    - -v=2
    - --health-addr={{suggestHealthHostPort}}
    - --webhook-port={{.WebhookPort}}
    - --webhook-cert-dir={{.WebhookCertDir}}
    infrastructureComponents: infrastructure-components.yaml
`, payload.APIVersion, payload.Kind, e.Name, e.BinaryPath, e.ComponentsPath)
}
