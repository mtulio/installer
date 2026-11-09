package clusterapi

import (
	"fmt"
	"sync"

	"github.com/sirupsen/logrus"

	"github.com/openshift/installer/pkg/types/external"
)

// ExternalProviderSpec describes a Cluster API infrastructure provider that the
// installer was not compiled against, supplied by the user for the External
// platform. Its paths are absolute and have been validated.
type ExternalProviderSpec struct {
	// Name identifies the provider. It names the controller in logs and
	// selects the developer-only artifact override environment variables.
	Name string
	// BinaryPath is the absolute path to the provider controller executable.
	BinaryPath string
	// ComponentsPath is the absolute path to the provider's CRDs and component
	// manifests, either a file or a directory.
	ComponentsPath string
	// BinarySHA256 is the hex-encoded digest of BinaryPath, recorded so that a
	// run can be traced back to an exact build.
	BinarySHA256 string
	// Args is the controller's whole command line, less the --kubeconfig the
	// local control plane appends. The installer contributes no flags of its
	// own: see externalInfrastructureController.
	Args []string
}

// externalProvider carries the spec from the External infrastructure
// provider's PreProvision hook to System.Run.
//
// It is process state rather than an argument because System.Run reads
// metadata.json, not the install-config, and the two are reached through
// different call paths. PreProvision runs strictly before System.Run
// (pkg/infrastructure/clusterapi/clusterapi.go:138 and :172), so the spec is
// always set by the time it is read.
var externalProvider struct {
	sync.Mutex
	spec *ExternalProviderSpec
}

// SetExternalProvider validates the user-supplied provider artifacts and
// records them for System.Run.
//
// Validation is the same as for the developer-only environment-variable
// overrides -- the paths are made absolute, the binary must be a non-empty
// executable built for this host architecture, and the components must exist
// -- so that the two ways of supplying artifacts cannot disagree about what is
// acceptable. Only the artifact path, its source and its digest are logged;
// the contents never are.
func SetExternalProvider(spec *ExternalProviderSpec) error {
	if spec == nil {
		return fmt.Errorf("no Cluster API provider configured")
	}
	if spec.Name == "" {
		return fmt.Errorf("the Cluster API provider name is empty")
	}

	resolved := &ExternalProviderSpec{Name: spec.Name, Args: spec.Args}

	binary, err := absolutePath(spec.BinaryPath)
	if err != nil {
		return fmt.Errorf("invalid provider binary: %w", err)
	}
	if err := validateExecutable(binary); err != nil {
		return fmt.Errorf("invalid provider binary: %w", err)
	}
	digest, err := fileSHA256(binary)
	if err != nil {
		return fmt.Errorf("invalid provider binary: %w", err)
	}
	resolved.BinaryPath, resolved.BinarySHA256 = binary, digest

	components, err := absolutePath(spec.ComponentsPath)
	if err != nil {
		return fmt.Errorf("invalid provider components: %w", err)
	}
	if err := validateComponents(components); err != nil {
		return fmt.Errorf("invalid provider components: %w", err)
	}
	resolved.ComponentsPath = components

	// Warn, not Info, and say so plainly: this is a provisional interface and
	// a support case needs the signal in the log bundle. Matches the tone of
	// applyArtifactOverride for the developer-only env-var path.
	logrus.Warnf("Using user-supplied Cluster API infrastructure provider %q: binary %s (sha256:%s), components %s; "+
		"this is not a supported configuration",
		resolved.Name, resolved.BinaryPath, resolved.BinarySHA256, resolved.ComponentsPath)

	externalProvider.Lock()
	defer externalProvider.Unlock()
	externalProvider.spec = resolved
	return nil
}

// SetExternalProviderFromMetadata recovers the provider spec from
// metadata.json for a command that never ran PreProvision.
//
// PreProvision is the only caller of SetExternalProvider, and it runs on the
// provisioning path alone. Both destroy paths -- `destroy bootstrap`, which is
// also a normal step of `create cluster`, and `destroy cluster` -- reach
// System.Run without it, so the in-process spec is nil and the local control
// plane has no infrastructure provider to start. Recording the artifact
// locations in metadata.json is what makes them recoverable.
//
// Validation is deliberately not relaxed for this path. A binary that has been
// moved, replaced with a different architecture, or emptied since install
// should fail here, naming the path, rather than start a provider that cannot
// act and let destroy report success having deleted nothing.
func SetExternalProviderFromMetadata(md *external.Metadata) error {
	if md == nil || md.ClusterAPI == nil {
		return fmt.Errorf("metadata.json records no Cluster API infrastructure provider for the %s platform; "+
			"it was written by an installer that did not provision infrastructure, or by one too old to record the provider",
			external.Name)
	}

	// The name is the platform's, not a second field of its own. A provider
	// payload's metadata.name is required to equal platformName, so recording
	// it again under ClusterAPI could only ever be a spelling that disagrees
	// with this one -- and this is the one `destroy cluster` already reads to
	// pick the platform.
	return SetExternalProvider(&ExternalProviderSpec{
		Name:           md.PlatformName,
		BinaryPath:     md.ClusterAPI.BinaryPath,
		ComponentsPath: md.ClusterAPI.ComponentsPath,
		Args:           md.ClusterAPI.Args,
	})
}

// getExternalProvider returns the spec recorded by SetExternalProvider, or nil
// when none was configured.
func getExternalProvider() *ExternalProviderSpec {
	externalProvider.Lock()
	defer externalProvider.Unlock()
	return externalProvider.spec
}

// externalInfrastructureController builds the controller for a user-supplied
// infrastructure provider.
//
// Unlike the integrated providers it takes nothing from the embedded mirror:
// Path is the user's binary and Components is the user's manifest, so
// skipExtract is set and Provider.Sources is left empty. Provider itself stays
// non-nil so that the developer-only environment-variable overrides still
// apply -- an override for this provider name takes precedence over the
// install-config, which is what makes it useful for swapping a build without
// editing the install-config.
//
// The argument list is taken whole from spec.Args and nothing is injected.
// This used to prepend the four flags the installer considered universal and
// append the provider's, which was wrong in a way that only a non-integrated
// provider could show: append cannot remove. CAPOCI declares
// --health-probe-bind-address rather than --health-addr and exits non-zero on
// an unknown flag, so the injected spelling made the controller unstartable
// and no configuration could take it back out.
//
// Every integrated provider already owns its full list -- compare
// system.go:345 for AWS with :381 for Nutanix, which disagree on exactly that
// flag -- so this makes the external path the same shape as the rest of this
// file rather than a second mechanism.
//
// runController still appends --kubeconfig (system.go:703-705), and the
// payload is validated not to declare it.
func (c *system) externalInfrastructureController(spec *ExternalProviderSpec) *controller {
	args := append([]string(nil), spec.Args...)

	return &controller{
		Provider:    &Provider{Name: spec.Name},
		Name:        fmt.Sprintf("%s infrastructure provider", spec.Name),
		Path:        spec.BinaryPath,
		Components:  []string{spec.ComponentsPath},
		Args:        args,
		Env:         map[string]string{},
		skipExtract: true,
	}
}
