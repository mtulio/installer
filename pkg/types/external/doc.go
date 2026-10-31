// Package external contains generic structures for installer
// configuration and management.
// +k8s:deepcopy-gen=package
package external

// Name is name for the External platform.
const Name string = "external"

// ManifestDir is the directory, relative to the install directory, holding the
// Cluster API manifests the user supplies for the External platform: the
// Cluster and the infrastructure object its provider reconciles, with Machine
// objects in the "machines" subdirectory.
//
// It is deliberately not the "cluster-api" directory the installer uses for
// the platforms it generates manifests for, for two reasons. That directory is
// an asset load path, and the asset store discards assets whose dependencies
// are dirty, which silently throws away files placed there alongside an
// unconsumed install-config.yaml. It is also where the installer unpacks the
// Cluster API and envtest binaries at provisioning time, so it is installer
// scratch space rather than a user interface.
//
// The name is provisional and may change before this lands.
const ManifestDir string = "external-install"

// MachineManifestDir is the subdirectory of ManifestDir holding Machine
// objects. They are separated because the installer creates them in a second
// stage, after the infrastructure reports ready.
const MachineManifestDir string = "machines"

// CapiInstallerDir is the default directory, inside the install directory,
// that a provider payload image is extracted into. Every path the payload
// declares is resolved against it.
//
// It is a sibling of ManifestDir rather than a subdirectory of it because the
// two have different owners and different lifetimes: ManifestDir is written
// by the user and travels with the install directory, while this one is
// unpacked by the installer from an artifact the provider published and can
// be deleted and re-extracted at any time.
const CapiInstallerDir string = "capi-installer"

// PayloadDescriptorFile is the OpenShift-specific descriptor at the root of a
// provider payload. It is the companion of clusterctl's own metadata.yaml:
// that file declares which Cluster API contract the provider implements, and
// this one declares the Kubernetes compatibility range and the payload's
// layout, neither of which has an upstream equivalent.
const PayloadDescriptorFile string = "openshift.yaml"

// ExtraManifestDir is the subdirectory of ManifestDir holding cluster
// manifests the user wants applied during bootstrap, ahead of anything an
// operator does.
//
// It exists because of what `platform: external` means. The installer
// delivers no cloud controller manager -- `cloudControllerManager.state:
// External` is a declaration that the partner supplies one -- and until a CCM
// runs, every node keeps the
// `node.cloudprovider.kubernetes.io/uninitialized` taint the kubelet applies
// under `--cloud-provider=external`, no control-plane operator can schedule,
// and bootstrap times out. So the one thing this platform cannot finish
// without is the one thing it has no way to deliver.
//
// The existing answer is the "openshift" manifest directory, and it does not
// work here. That directory is an asset load path: when the asset store finds
// files in it, the Openshift asset is marked as loaded from disk and its
// Generate is never called (pkg/asset/store/store.go:196-232), so a user who
// drops one file there silently replaces every generated manifest. That is
// why the CI job for this platform has to run `create manifests`, edit the
// tree and then `create ignition-configs` as three separate commands. A
// directory that is read during Generate rather than instead of it is what
// makes a single `create cluster` possible.
//
// Files here are added to the openshift manifest directory, so bootkube moves
// them in with the rest and cluster-bootstrap applies them to the bootstrap
// control plane before it waits for the cluster to come up
// (data/data/bootstrap/files/usr/local/bin/bootkube.sh.template:78). A
// MachineConfig placed here is rendered by the bootstrap machine-config
// server, so it reaches the control-plane machines on their first boot.
//
// The installer does not parse, validate or template anything here. It does
// not know what a partner's CCM needs, and acquiring an opinion about it is
// the coupling this platform exists to avoid.
const ExtraManifestDir string = "extra-manifests"

// HookStateDir is the subdirectory of ManifestDir a provisioning hook may use
// to record what it created, so that the teardown hook can remove exactly
// that.
//
// It exists because a hook creates resources outside Cluster API's ownership,
// and nothing else will ever clean those up: deleting the Cluster deletes what
// the provider built, and a DNS zone the hook created is not that. The
// installer does not read or interpret anything here -- it only guarantees the
// directory exists, is writable, and survives from `create cluster` to
// `destroy cluster` in the same install directory.
const HookStateDir string = "hook-state"
