// Package clusterapi provisions the infrastructure for the External platform
// with a Cluster API infrastructure provider supplied by the user.
//
// The installer has no knowledge of the provider: it does not import the
// provider's Go types, does not generate its manifests, and does not talk to
// its cloud. It starts the provider's controller against its temporary, local
// Cluster API control plane, applies the Cluster and infrastructure objects
// the user placed in the install directory, and waits for the Cluster to
// report its infrastructure ready. Everything platform-specific belongs to the
// provider.
//
// Nothing here is specific to any one cloud. Where a provider is named in
// tests or documentation it is a reference provider, used to exercise this
// path; it is not an integration with that cloud.
package clusterapi

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/sets"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta1" //nolint:staticcheck //CORS-3563
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openshift/installer/cmd/openshift-install/command"
	"github.com/openshift/installer/pkg/clusterapi"
	infracapi "github.com/openshift/installer/pkg/infrastructure/clusterapi"
	"github.com/openshift/installer/pkg/infrastructure/external/payload"
	externaltypes "github.com/openshift/installer/pkg/types/external"
)

// Provider implements the External platform's Cluster API infrastructure
// provider.
//
// The remaining installer hooks are optional interfaces reached by type
// assertion, so the ones it does not implement are simply not called.
//
// InfraReady and PostProvision are implemented, but note what they do and do
// not do. Each runs a program the user supplies rather than doing any cloud
// work itself -- see infraready.go and postprovision.go -- because the things
// that have to happen there, creating the cluster's API and ingress DNS, are
// not expressible in the Cluster API contract and cannot be done by an
// installer that has no knowledge of the provider's cloud. The two differ
// only in when they run, and that difference is the whole reason there are
// two: InfraReady can name what the provider built, PostProvision can name
// what the cluster built for itself.
type Provider struct{}

var (
	_ infracapi.PreProvider                   = Provider{}
	_ infracapi.InfraReadyProvider            = Provider{}
	_ infracapi.PostProvider                  = Provider{}
	_ infracapi.IgnitionProvider              = Provider{}
	_ infracapi.ManifestProvider              = Provider{}
	_ infracapi.ManifestValidator             = Provider{}
	_ infracapi.UnstructuredManifestTolerator = Provider{}
)

// manifestExtensions are the file extensions read from the manifest
// directory. It matches what the installer accepts for the platforms it
// generates manifests for, so a user moving between them is not surprised.
var manifestExtensions = sets.New(".yaml", ".yml", ".json")

// ProvideManifests reads the Cluster API objects the user placed in the
// install directory.
//
// The installer generates nothing for this platform: it does not know the
// provider's API, so the Cluster, the infrastructure object and the Machines
// are all written by the user. They are read from
// <install-dir>/external-install, with Machines in its "machines"
// subdirectory, and returned for the installer to validate and create through
// the same path every other platform uses.
//
// A missing directory is an error rather than an empty result. Reaching this
// point means the install-config asked for Cluster API provisioning, so an
// absent directory is a mistake the user needs told about, not an instruction
// to provision nothing.
func (p Provider) ProvideManifests(_ context.Context, installDir string) (infra, machines []client.Object, err error) {
	root := filepath.Join(installDir, externaltypes.ManifestDir)

	infra, err = readManifestDir(root)
	if err != nil {
		return nil, nil, err
	}

	machineDir := filepath.Join(root, externaltypes.MachineManifestDir)
	if _, statErr := os.Stat(machineDir); statErr == nil {
		machines, err = readManifestDir(machineDir)
		if err != nil {
			return nil, nil, err
		}
	} else if !os.IsNotExist(statErr) {
		return nil, nil, fmt.Errorf("failed to read %s: %w", machineDir, statErr)
	}

	logrus.Debugf("Loaded %d infrastructure and %d machine objects from %s",
		len(infra), len(machines), root)
	return infra, machines, nil
}

// readManifestDir decodes every manifest directly inside dir. It does not
// recurse: the "machines" subdirectory is read separately, and anything else
// found there is the user's own business.
func readManifestDir(dir string) ([]client.Object, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("no %q directory in the install directory: the %s platform does not "+
				"generate Cluster API manifests, so the %s and the object its provider reconciles must be "+
				"supplied there",
				externaltypes.ManifestDir, externaltypes.Name, clusterv1.ClusterKind)
		}
		return nil, fmt.Errorf("failed to read %s: %w", dir, err)
	}

	var objects []client.Object
	for _, entry := range entries {
		if entry.IsDir() || !manifestExtensions.Has(strings.ToLower(filepath.Ext(entry.Name()))) {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("failed to read %s: %w", path, err)
		}
		decoded, err := clusterapi.ObjectsFromManifest(path, data)
		if err != nil {
			return nil, err
		}
		for _, d := range decoded {
			objects = append(objects, d.Object)
		}
	}
	return objects, nil
}

// TolerateUnstructuredManifests allows manifests whose kind this installer has
// no compiled-in type for.
//
// This is the whole point of the platform: the provider's CRDs are installed
// into the local control plane from its own components, so its objects are
// served by types this binary was never built against. Every compiled-in
// platform keeps the strict check.
func (p Provider) TolerateUnstructuredManifests() bool { return true }

// Name returns the External platform name.
func (p Provider) Name() string {
	return externaltypes.Name
}

// PublicGatherEndpoint indicates that machine ready checks should not wait for
// an ExternalIP before declaring a machine ready.
//
// The installer cannot know whether a given provider records a public address
// in the machine status, and requiring one it never sets would hang the
// install. Waiting for an InternalIP is the weaker assumption, so it is the
// one made here; the cost is that gathering bootstrap logs over a public
// address is not available on this path.
func (p Provider) PublicGatherEndpoint() infracapi.GatherEndpoint {
	return infracapi.InternalIP
}

// ValidateManifests checks that the user supplied the Cluster API objects the
// installer needs but does not generate.
//
// This is the most consequential check on this path. For the External platform
// both manifest generators return nil rather than an error, so without it an
// empty or absent cluster-api directory produces an install that creates no
// infrastructure and reports no failure -- it would hang waiting for a cluster
// that was never created, or complete having done nothing. A silent no-op is
// the worst failure mode available here, so this is an error and not a
// warning.
//
// Only the objects the installer itself relies on are checked. The
// infrastructure object is the provider's own API: the installer does not know
// its schema and does not attempt to validate it.
func (p Provider) ValidateManifests(infra, machines []client.Object) error {
	var clusters, infraObjs int
	for _, o := range infra {
		// A nil object is not hypothetical. On a re-entrant run
		// (OPENSHIFT_INSTALL_REENTRANT=true) the Cluster asset is restored
		// from .openshift_install_state.json, which persists only each
		// RuntimeFile's Filename and Data -- never the decoded Object, which
		// is an interface and is not serialised. The installer's own guests
		// Namespace therefore comes back as a nil client.Object, and reading
		// its kind panicked the installer with a stack trace instead of a
		// message. Skipping it is right on its own terms too: validation
		// counts what the *user* supplied, and a nil is nothing supplied.
		if o == nil {
			continue
		}
		if gvk, skewed := skewedCoreGVK(o); skewed {
			return coreVersionSkewError(gvk)
		}
		switch {
		case isCoreKind(o, clusterv1.ClusterKind):
			clusters++
		case isNamespace(o):
			// Supplied by the installer, not by the user. Counting it would
			// let an install with no infrastructure object pass this check.
		default:
			infraObjs++
		}
	}

	if clusters == 0 {
		return fmt.Errorf("no %s object found in the %q directory of the install directory; "+
			"the %s platform does not generate one, and the installer waits on its status to know "+
			"when the infrastructure is ready",
			clusterv1.ClusterKind, externaltypes.ManifestDir, externaltypes.Name)
	}
	if infraObjs == 0 {
		return fmt.Errorf("no infrastructure object found in the %q directory of the install directory; "+
			"the object the %s provider reconciles must be supplied alongside the %s",
			externaltypes.ManifestDir, externaltypes.Name, clusterv1.ClusterKind)
	}

	// Machines are warned about rather than required. Without them the
	// install cannot complete -- the provisioning wait iterates an empty
	// list, reports the control plane ready and provisions no machines, and
	// the failure surfaces much later in wait-for bootstrap-complete with
	// nothing pointing back here. But infrastructure-only runs are how this
	// path is brought up: creating the network and stopping short of any
	// machine is a deliberate, much cheaper way to exercise the provider.
	// Refusing to start one would make the installer harder to develop
	// against for no safety gained, since the later failure is not
	// destructive.
	var machineObjs int
	for _, o := range machines {
		if gvk, skewed := skewedCoreGVK(o); skewed {
			return coreVersionSkewError(gvk)
		}
		if isCoreKind(o, machineKind) {
			machineObjs++
		}
	}
	if machineObjs == 0 {
		logrus.Warnf("No %s objects found in %q: the %s platform does not generate them, so no machines "+
			"will be created and the install cannot complete. This is expected only for an "+
			"infrastructure-only run.",
			"Machine", filepath.Join(externaltypes.ManifestDir, externaltypes.MachineManifestDir), externaltypes.Name)
	}
	return nil
}

// machineKind is the core Cluster API Machine kind. The Cluster kind has a
// constant upstream; the Machine kind does not.
const machineKind = "Machine"

// isCoreKind reports whether o is the named core Cluster API object, as
// provisioning will see it.
//
// It matches on the Go type rather than on the object's apiVersion and kind,
// for two reasons that point the same way. Provisioning finds these objects by
// asserting the concrete type (pkg/infrastructure/clusterapi/clusterapi.go:140
// and :387), so matching on anything else would let validation and
// provisioning disagree about what is present. And decoding a manifest does
// not leave a typed object's GroupVersionKind populated: the scheme conversion
// in pkg/clusterapi.ObjectsFromManifest yields a *clusterv1.Cluster whose
// TypeMeta is empty, so a GVK match here rejected every real install directory
// while passing tests that set the GVK by hand.
func isCoreKind(o client.Object, kind string) bool {
	switch o.(type) {
	case *clusterv1.Cluster:
		return kind == clusterv1.ClusterKind
	case *clusterv1.Machine:
		return kind == machineKind
	}
	return false
}

// isNamespace reports whether o is a Namespace, typed or not.
func isNamespace(o client.Object) bool {
	if _, ok := o.(*corev1.Namespace); ok {
		return true
	}
	gvk := o.GetObjectKind().GroupVersionKind()
	return gvk.Group == "" && gvk.Kind == "Namespace"
}

// skewedCoreGVK reports an object that names a core Cluster API kind but was
// not decoded into its Go type.
//
// Only unstructured objects can be skewed: anything the scheme recognised is
// at the one core version this installer registers, by construction. So an
// unstructured object in the core group naming Cluster or Machine is a core
// object at some other version, and is what coreVersionSkewError describes.
func skewedCoreGVK(o client.Object) (schema.GroupVersionKind, bool) {
	u, ok := o.(*unstructured.Unstructured)
	if !ok {
		return schema.GroupVersionKind{}, false
	}
	gvk := u.GroupVersionKind()
	if gvk.Group != clusterv1.GroupVersion.Group {
		return gvk, false
	}
	return gvk, gvk.Kind == clusterv1.ClusterKind || gvk.Kind == machineKind
}

// coreVersionSkewError rejects a core Cluster API object written against a
// version other than the one this installer is built with.
//
// The version matters far more here than it looks. Provisioning finds the
// Cluster and the Machines by asserting the concrete types, and this platform
// tolerates manifests whose kind the installer has no type for, so a Cluster at
// another version is decoded as *unstructured.Unstructured, created
// successfully, and then never recognised. The readiness wait iterates an empty
// list and returns true on its first poll: the install reports the
// infrastructure ready without having waited for anything, and machines are
// created against infrastructure that may not exist.
//
// That is the silent no-op this hook exists to prevent, arriving through a
// version skew rather than a missing file.
func coreVersionSkewError(gvk schema.GroupVersionKind) error {
	return fmt.Errorf("%s in the %q directory uses %s, but this installer is built against %s: "+
		"provisioning identifies core Cluster API objects by their compiled-in type, so one at "+
		"another version would be created and then ignored, and the install would report the "+
		"infrastructure ready without waiting for it. Use apiVersion %s",
		gvk.Kind, externaltypes.ManifestDir, gvk.GroupVersion(), clusterv1.GroupVersion,
		clusterv1.GroupVersion)
}

// PreProvision resolves the user-supplied provider artifacts and hands them to
// the Cluster API system.
//
// This runs before the local control plane is started, so it is where a bad
// path is reported: by the time the controller would be started, the local
// control plane is already running. It creates no cloud resources -- there are
// none the installer could create without knowing the platform.
func (p Provider) PreProvision(ctx context.Context, in infracapi.PreProvisionInput) error {
	if in.InstallConfig == nil || in.InstallConfig.Config == nil || in.InstallConfig.Config.Platform.External == nil {
		return fmt.Errorf("no %s platform configured", externaltypes.Name)
	}
	capi := in.InstallConfig.Config.Platform.External.ClusterAPI
	if capi == nil {
		return fmt.Errorf("platform.external.clusterAPI is required to provision infrastructure for the %s platform",
			externaltypes.Name)
	}

	spec := &clusterapi.ExternalProviderSpec{
		Name:           capi.Name,
		BinaryPath:     capi.BinaryPath,
		ComponentsPath: capi.ComponentsPath,
		Args:           capi.Args,
	}

	// In image mode none of the three paths above is configured -- validation
	// rejects a local path there -- and they come from the payload instead.
	// The payload has already been resolved by this point, by whichever
	// earlier stage needed it first; this call collects that answer rather
	// than producing a second one.
	res, err := payload.ResolveForInstallConfig(ctx, in.InstallConfig.Config, command.RootOpts.Dir, in.InfraID)
	if err != nil {
		return fmt.Errorf("failed to resolve the Cluster API provider payload: %w", err)
	}
	if res != nil {
		spec.BinaryPath = res.ControllerPath
		spec.ComponentsPath = res.ComponentsPath
		spec.Args = res.Args
	}

	if err := clusterapi.SetExternalProvider(spec); err != nil {
		return fmt.Errorf("failed to configure the %s Cluster API infrastructure provider: %w", capi.Name, err)
	}
	return nil
}
