package external

// CloudControllerManager describes the type of cloud controller manager to be enabled.
type CloudControllerManager string

const (
	// CloudControllerManagerTypeExternal specifies that an external cloud provider is to be configured.
	CloudControllerManagerTypeExternal = "External"

	// CloudControllerManagerTypeNone specifies that no cloud provider is to be configured.
	CloudControllerManagerTypeNone = ""
)

// Platform stores configuration related to external cloud providers.
type Platform struct {
	// PlatformName holds the arbitrary string representing the infrastructure provider name, expected to be set at the installation time.
	// This field is solely for informational and reporting purposes and is not expected to be used for decision-making.
	// +kubebuilder:default:="Unknown"
	// +default="Unknown"
	// +kubebuilder:validation:XValidation:rule="oldSelf == 'Unknown' || self == oldSelf",message="platform name cannot be changed once set"
	// +optional
	PlatformName string `json:"platformName,omitempty"`

	// CloudControllerManager when set to external, this property will enable an external cloud provider.
	// +kubebuilder:default:=""
	// +default=""
	// +kubebuilder:validation:Enum="";External
	// +optional
	CloudControllerManager CloudControllerManager `json:"cloudControllerManager,omitempty"`

	// ClusterAPI configures a user-supplied Cluster API infrastructure provider
	// to provision the cluster's infrastructure. When it is unset, the installer
	// provisions nothing and the infrastructure is expected to exist already,
	// which is the behaviour of this platform before this field was introduced.
	//
	// This is a provisional interface for the External enablement pilot and is
	// not a supported installation API.
	// +optional
	ClusterAPI *ClusterAPIProvider `json:"clusterAPI,omitempty"`
}

// ProvisionsWithClusterAPI reports whether this platform asks the installer to
// provision infrastructure with a user-supplied Cluster API provider, rather
// than leaving it to be provisioned out of band.
//
// This is the one predicate that separates the two kinds of External install,
// and every place that has to tell them apart must call it: the platform's
// long-standing user-provisioned behaviour has to survive unchanged, and it
// only does so if the condition cannot drift between call sites.
//
// A nil receiver reports false, so callers holding an InstallConfig whose
// platform may be something else entirely can call it without a guard.
func (p *Platform) ProvisionsWithClusterAPI() bool {
	return p != nil && p.ClusterAPI != nil
}

// ClusterAPIProvider describes the Cluster API infrastructure provider that
// provisions the cluster's infrastructure. Every artifact comes from one OCI
// image the provider publishes -- the controller, its components bundle, both
// template trees and the install hooks -- and the installer starts that
// controller against its temporary, local Cluster API control plane. The
// installer has no knowledge of the provider's API and generates no manifests
// for it.
//
// The stanza is deliberately small. A conforming payload needs PayloadImage
// and whatever Variables its templates declare, and nothing else: the three
// path fields select between things the payload already shipped,
// ControllerExtraArgs is for debugging, and BootstrapIgnitionStaging is
// consent a payload has to ask for.
type ClusterAPIProvider struct {
	// PayloadImage is a fully qualified reference to the provider payload
	// image. A digest is required for a mirrored or disconnected install --
	// imageDigestSources redirects by digest -- and accepted everywhere; a tag
	// is accepted for a connected install, because the install log records the
	// digest it resolved to.
	//
	// One field rather than a struct: the image carries the controller binary,
	// the hooks, both template trees and the descriptor, so naming it after
	// the controller would name it after one of its parts.
	//
	// There is no provider-name field. The payload's metadata.name is required
	// to equal PlatformName above, which cluster metadata already records, so
	// `destroy cluster` reaches the same provider without a third spelling of
	// the same string.
	//
	// Authentication reuses pullSecret and redirection reuses
	// imageDigestSources. Neither needs a new field here.
	PayloadImage string `json:"payloadImage"`

	// ControllerExtraArgs are appended to the controller's command line after
	// the payload's own spec.capi.controllerArgs, with the last occurrence of
	// a flag winning -- so "-v=5" here raises verbosity for a support case
	// without repackaging the payload image.
	//
	// Genuinely extra: the payload, not this field, owns the baseline. A
	// provider's flag names are the provider's knowledge and belong in the
	// payload; this exists for the operator's debugging, not for making a
	// payload work. An entry containing a {{...}} token is rejected, because
	// the installer's render inputs are not a user interface.
	// +optional
	ControllerExtraArgs []string `json:"controllerExtraArgs,omitempty"`

	// BootstrapIgnitionStaging is the cluster creator's consent to the
	// payload's ignition hook -- the only hook handed the cluster's
	// bootstrap ignition, and therefore its day-0 secret material. A payload
	// declaring that hook while this is unset or "Disabled" is refused.
	//
	// The refusal cannot happen at install-config validation, because the hook
	// is declared by the payload and the payload is not available until it has
	// been pulled. It happens at the first point after the payload is read --
	// PreProvision, before the local control plane starts and before any cloud
	// resource is created -- and again at the handover itself. A payload that
	// does not declare the hook is unaffected either way, so the field is inert
	// for the common case and nothing an ordinary partner's users have to set.
	//
	// An enum rather than a bool so that a narrower permission -- allowed only
	// from a signed payload, say -- is a third value rather than a second
	// field. Unset is treated as "Disabled".
	// +kubebuilder:validation:Enum=Disabled;Allowed
	// +optional
	BootstrapIgnitionStaging BootstrapIgnitionStaging `json:"bootstrapIgnitionStaging,omitempty"`

	// CapiInstallerPath is the directory the provider payload is extracted
	// into, and the root the two paths below are resolved against.
	//
	// Relative to the install directory and required to stay inside it, so
	// that the artifacts `destroy cluster` depends on travel with the
	// directory the user keeps. Defaults to "capi-installer".
	// +optional
	CapiInstallerPath string `json:"capiInstallerPath,omitempty"`

	// InfrastructureComponents is the single bundle manifest the local Cluster
	// API control plane loads -- the provider's CRDs and webhook
	// configurations.
	//
	// A single file rather than a directory, because every clusterctl provider
	// already publishes exactly one infrastructure-components.yaml. Defaults
	// to what the payload's own descriptor declares, so a conforming payload
	// needs no path configuration at all.
	// +optional
	InfrastructureComponents string `json:"infrastructureComponents,omitempty"`

	// ManifestTemplates is the directory of day-0 manifest templates the
	// installer renders and folds into the install directory's manifests, to
	// be applied during bootstrap.
	//
	// Defaults to what the payload's descriptor declares. These reach the
	// installed cluster, unlike the Cluster API templates alongside them,
	// which are applied only to the installer's local, ephemeral control
	// plane.
	// +optional
	ManifestTemplates string `json:"manifestTemplates,omitempty"`

	// Variables supply values for the substitution tokens in the payload's
	// template trees. The installer uses clusterctl's syntax -- ${VAR},
	// ${VAR=default} and ${VAR:="default"} -- so that a provider maintains one
	// template dialect across their upstream and their OpenShift artifacts.
	//
	// Names beginning with "INSTALLER_" are reserved: the installer supplies
	// that set itself, and setting one here is an error. The prefix is what
	// lets the installer's namespace and the provider's grow independently
	// without colliding inside the same file.
	//
	// A token the templates use, that has no default and that nothing here
	// resolves, fails validation before any cloud call -- consistent with
	// clusterctl's own behaviour on an unset required variable.
	//
	// Do not put a credential in one. These names are recorded in the install
	// log.
	// +optional
	Variables map[string]string `json:"variables,omitempty"`
}

// BootstrapIgnitionStaging is the cluster creator's answer to a payload that
// asks to handle the bootstrap ignition itself.
type BootstrapIgnitionStaging string

const (
	// BootstrapIgnitionStagingDisabled refuses a payload that declares an
	// ignition hook. It is the behaviour of an unset field.
	BootstrapIgnitionStagingDisabled BootstrapIgnitionStaging = "Disabled"

	// BootstrapIgnitionStagingAllowed permits the payload's ignition hook
	// to be handed the cluster's bootstrap ignition.
	BootstrapIgnitionStagingAllowed BootstrapIgnitionStaging = "Allowed"
)

// AllowsBootstrapIgnitionStaging reports whether the cluster creator consented
// to the ignition hook. A nil receiver and an unset field both report
// false, so the absence of an answer is never read as a yes.
func (p *ClusterAPIProvider) AllowsBootstrapIgnitionStaging() bool {
	return p != nil && p.BootstrapIgnitionStaging == BootstrapIgnitionStagingAllowed
}

// UsesImagePayload reports whether a payload image is configured. A nil
// receiver reports false.
//
// PayloadImage is required whenever ClusterAPI is set, so in a valid
// install-config this agrees with ProvisionsWithClusterAPI. It stays a
// separate predicate because the callers that use it run before validation or
// on a configuration they did not validate themselves, and an empty reference
// there means "nothing to pull", not "pull nothing".
func (p *ClusterAPIProvider) UsesImagePayload() bool {
	return p != nil && p.PayloadImage != ""
}

// Hooks names programs the installer runs at points in the flow where an
// integrated platform would run its own code.
//
// This is the seam that lets a partner automate the parts of provisioning
// that Cluster API does not cover, without the installer gaining any
// knowledge of their cloud. The motivating case is DNS. An integrated
// platform creates the cluster's `api` and `api-int` records itself once the
// load balancers exist -- for AWS that is InfraReady in
// pkg/infrastructure/aws/clusterapi/aws.go:112 -- but Cluster API has no
// contract for it: the core Cluster carries a single
// spec.controlPlaneEndpoint and there is no field for an internal endpoint at
// all. Without `api-int` the bootstrap node fails at its resolve-api-int-url
// stage and no control-plane machine can fetch its ignition, so something has
// to create those records, and on this platform it cannot be the installer.
//
// Every hook is optional. Leaving one unset is a supported configuration --
// the records may be created out of band, which is what the existing
// non-Cluster-API External CI does with CloudFormation -- so an absent hook
// is a warning naming the consequence, never an error.
type Hooks struct {
	// Ignition is run after the cluster's infrastructure reports ready and
	// before any machine is created. The network and the load balancers exist
	// by then; no compute does. It is named for the installer's
	// IgnitionProvider point of extension, which is where the integrated
	// providers stage an oversized bootstrap ignition too.
	//
	// It is the one hook whose output the installer reads back. A provider
	// whose controller cannot stage an oversized bootstrap ignition itself
	// uses it to write a replacement -- a pointer config over an object the
	// hook staged -- which the installer then publishes in place of its own.
	//
	// It is also the only hook handed the cluster's day-0 secret material,
	// which is why a payload declaring it is refused unless the cluster
	// creator set clusterAPI.bootstrapIgnitionStaging to "Allowed".
	// +optional
	Ignition *Hook `json:"ignition,omitempty"`

	// InfraReady is run once the Cluster reports
	// status.infrastructureReady, after the provider has created the network
	// and load balancers and before any machine is created. It is the point
	// at which the load balancer addresses first exist and the last point at
	// which DNS can be created in time for the bootstrap node to use it.
	// +optional
	InfraReady *Hook `json:"infraReady,omitempty"`

	// PostProvision is run once the control-plane machines have been
	// created, which is after the cluster's own API has begun serving and
	// before the installer waits for the bootstrap to complete.
	//
	// It exists for the things that can only be named after the cluster has
	// started building them. The motivating case is the ingress wildcard:
	// `*.apps` has to point at a load balancer that does not exist when
	// InfraReady runs, because on this platform nothing but the cluster's own
	// cloud controller manager creates it, and it does so in response to a
	// Service the cluster applies to itself. A hook here can wait for that
	// address and publish it, which is the last thing missing before
	// `wait-for install-complete` can succeed.
	// +optional
	PostProvision *Hook `json:"postProvision,omitempty"`

	// PreDestroy is run by `destroy cluster` after the local control plane
	// has been restored and before the Cluster is deleted.
	//
	// It is the counterpart of the two hooks above and exists because their
	// resources are invisible to Cluster API: deleting the Cluster removes
	// what the provider built and nothing else. Running before the delete
	// rather than after is deliberate -- a DNS alias record is described in
	// terms of the load balancer it points at, so it is cheaper to remove
	// while that load balancer still exists.
	// +optional
	PreDestroy *Hook `json:"preDestroy,omitempty"`
}

// Hook is one program the installer execs at a phase of the install.
//
// The installer runs it directly, without a shell, and passes it no
// arguments. Everything a hook is told is environment: the cluster's
// identity, the install directory, the Cluster API objects as files. That is
// the whole contract, and it is the same for every phase.
//
// It used to take an argv, from a time when the install-config named hooks
// directly and a user needed somewhere to put their own flags. A payload
// declares its hooks instead, and a payload's hooks and its programs are
// written by the same person, so a second channel for them to talk to each
// other was a field that could only drift from what the scripts expected.
// Its removal also closes an exposure: the installer logged a hook's
// arguments, and there are none left to log.
type Hook struct {
	// Program is a path to an executable, relative to the directory the hook
	// is rooted at -- the extraction directory for a payload's own hooks. It
	// must stay inside that directory: the install directory is the unit a
	// user copies, archives and hands to someone else, and a hook reaching
	// outside it would run something that did not travel with it.
	Program string `json:"program"`

	// BootstrapOutputPath is set on Ignition alone, and is the file the
	// installer reads back after that hook returns -- relative to a directory
	// the installer creates for the hook and removes afterwards.
	//
	// The partner names the file and the installer decides where it lives.
	// Declared rather than fixed so that the installer has a name to validate
	// and a name to put in a failure message.
	// +optional
	BootstrapOutputPath string `json:"bootstrapOutputPath,omitempty"`
}
