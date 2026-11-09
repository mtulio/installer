package external

// Metadata contains External metadata (e.g. for uninstalling the cluster).
//
// Its presence is what identifies the platform: ClusterPlatformMetadata.Platform
// reports "external" only when this member is set, and pkg/clusterapi's
// System.Run refuses to start without a platform name. So the External CAPI
// path needs this written even though the installer has nothing
// platform-specific to record.
type Metadata struct {
	// PlatformName holds the arbitrary string representing the infrastructure
	// provider name that was set at installation time. It is informational,
	// mirroring the install-config field of the same name.
	PlatformName string `json:"platformName,omitempty"`

	// ClusterAPI describes the user-supplied Cluster API infrastructure
	// provider that provisioned this cluster. It is nil for an External
	// install that provisioned nothing.
	// +optional
	ClusterAPI *ClusterAPIMetadata `json:"clusterAPI,omitempty"`

	// BaseDomain is the cluster's base domain, recorded because the teardown
	// hook is handed the same cluster identity the provisioning hook was and
	// the install-config that carried it has been consumed by the time
	// `destroy cluster` runs. It is not a credential and is already present
	// throughout the cluster's DNS.
	// +optional
	BaseDomain string `json:"baseDomain,omitempty"`
}

// ClusterAPIMetadata records where the user-supplied Cluster API
// infrastructure provider came from, so that `destroy cluster` can start the
// same provider again.
//
// Destroy cannot work without this. The installer has no SDK for the
// partner's cloud, so it cannot sweep resources by tag the way the integrated
// destroyers do; the only way to remove the infrastructure is to hand the
// objects back to the provider that created them and let it delete them. That
// requires running the provider, and by the time `destroy cluster` runs the
// install-config that named it has been consumed.
//
// Only the artifact paths and a digest are recorded. metadata.json travels
// with support bundles, so this deliberately holds nothing but locations:
// never a credential, and never the contents of a file. The paths are
// machine-local, which means destroy has to run somewhere the artifacts are
// still reachable -- a real limitation, stated here rather than discovered.
type ClusterAPIMetadata struct {
	// There is no provider-name field. PlatformName above is the name, and
	// the payload's metadata.name is required to equal it, so a second
	// spelling here could only ever disagree with the first.

	// BinaryPath is the absolute path to the provider controller executable.
	BinaryPath string `json:"binaryPath"`

	// No digest is recorded. metadata.json is written before the provider
	// artifacts are resolved and hashed, so any digest here would be either
	// absent or a second, differently-timed answer to a question the install
	// log already answers: both SetExternalProvider and the destroyer log the
	// binary's SHA-256, and comparing those two lines is what identifies a
	// destroy run against a different build than the install.

	// ComponentsPath is the absolute path to the provider's CRDs and
	// component manifests, either a file or a directory.
	ComponentsPath string `json:"componentsPath"`

	// Args is the controller's whole command line as the install resolved it
	// -- the payload's spec.capi.controllerArgs followed by the
	// install-config's controllerExtraArgs -- recorded so that destroy starts
	// the provider the same way the install did. The installer's render
	// tokens are still in it unrendered, because the ports and the certificate
	// directory belong to whichever local control plane is running, and
	// destroy's is not the install's.
	// +optional
	Args []string `json:"args,omitempty"`

	// Image is the provider payload image the artifacts above were extracted
	// from, and ImageDigest is what that reference resolved to at install
	// time. Both are empty for an install from local paths.
	//
	// Neither is used to pull anything during destroy. BinaryPath and
	// ComponentsPath point inside the install directory, so the payload is
	// already there; these are recorded so that a support case can tell which
	// build of a partner's provider a cluster was created by, which a tagged
	// reference alone cannot answer.
	// +optional
	Image string `json:"image,omitempty"`
	// +optional
	ImageDigest string `json:"imageDigest,omitempty"`

	// HookRoot is the directory, relative to the install directory, that the
	// pre-destroy hook's Program is resolved against. Empty means the External
	// manifest directory, which is where a hook named in the install-config
	// lives.
	//
	// It is recorded because a payload's hooks live in the extracted payload
	// instead, and `destroy cluster` has no install-config and no payload
	// descriptor to work that out from. It covers pre-destroy only -- each
	// hook's root follows the origin of that hook, and pre-destroy is the one
	// hook that still has to run after this file is written.
	// +optional
	HookRoot string `json:"hookRoot,omitempty"`

	// Hooks are the programs the install was configured to run, repeated here
	// for the same reason as Args: `destroy cluster` has to run the teardown
	// counterpart of whatever provisioning hook created resources, and the
	// install-config that named it is gone by then.
	//
	// The paths are relative to the External manifest directory in the
	// install directory, so unlike the provider artifact paths above they
	// travel with the install directory rather than being machine-local.
	// +optional
	Hooks *Hooks `json:"hooks,omitempty"`
}
