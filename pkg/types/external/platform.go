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
// provisions the cluster's infrastructure. The installer starts the named
// controller against its temporary, local Cluster API control plane and
// applies the manifests the user supplies in the install directory; it has no
// knowledge of the provider's API and does not generate manifests for it.
type ClusterAPIProvider struct {
	// Name identifies the provider, for example "oci". It names the controller
	// in installer logs and in the developer-only artifact override
	// environment variables.
	Name string `json:"name"`

	// BinaryPath is a path on the machine running the installer to the provider
	// controller executable.
	//
	// Required when the provider is supplied as local paths, which is the
	// default. It must not be set when Controller.SourceType is "image": the
	// payload supplies the controller, and mixing the two would make it
	// ambiguous which binary runs.
	// +optional
	BinaryPath string `json:"binaryPath,omitempty"`

	// ComponentsPath is a path on the machine running the installer to the
	// provider's CRDs and component manifests, either a single YAML file or a
	// directory of them.
	//
	// Required when the provider is supplied as local paths, and must not be
	// set when Controller.SourceType is "image", as for BinaryPath.
	// +optional
	ComponentsPath string `json:"componentsPath,omitempty"`

	// Args are appended to the provider controller's command line, after the
	// arguments the installer supplies itself. Use it for flags a particular
	// provider requires and the installer does not know about.
	//
	// This is the original spelling and continues to work. Controller.Args is
	// the same setting in the newer, grouped form; setting both is an error
	// rather than a merge, because two places to write one list is how the two
	// silently drift apart.
	// +optional
	Args []string `json:"args,omitempty"`

	// Hooks names programs the installer runs at fixed points in the install.
	// +optional
	Hooks *Hooks `json:"hooks,omitempty"`

	// Controller selects where the provider controller comes from.
	//
	// When it is unset, or its SourceType is "local", the installer reads
	// BinaryPath and ComponentsPath and behaves exactly as it did before this
	// field existed. When SourceType is "image", every artifact instead comes
	// out of one OCI image published by the provider, and no local path is
	// consulted.
	//
	// The two are modes of one field set rather than two field sets, so a user
	// moving between them changes SourceType and the artifacts, not the shape
	// of their configuration.
	// +optional
	Controller *Controller `json:"controller,omitempty"`

	// CapiInstallerPath is the directory the provider payload is extracted
	// into, and the root every other path below is resolved against.
	//
	// Defaults to the "capi-installer" directory of the install directory.
	// Only meaningful when Controller.SourceType is "image".
	// +optional
	CapiInstallerPath string `json:"capiInstallerPath,omitempty"`

	// CustomResource is the single bundle manifest the local Cluster API
	// control plane loads -- the provider's CRDs and webhook configurations.
	//
	// A single file rather than a directory, because every clusterctl provider
	// already publishes exactly one infrastructure-components.yaml. Defaults
	// to what the payload's own layout declares, so a conforming payload needs
	// no path configuration at all.
	// +optional
	CustomResource string `json:"customResource,omitempty"`

	// ManifestPath is the directory of day-0 manifest templates the installer
	// renders and folds into the install directory's manifests, to be applied
	// during bootstrap.
	//
	// Defaults to what the payload's layout declares. These reach the
	// installed cluster, unlike the Cluster API templates alongside them,
	// which are applied only to the installer's local, ephemeral control
	// plane.
	// +optional
	ManifestPath string `json:"manifestPath,omitempty"`

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
	// Do not put a credential in one. These are recorded in the install log.
	// +optional
	Variables map[string]string `json:"variables,omitempty"`
}

// ControllerSourceType selects where the provider's artifacts come from.
type ControllerSourceType string

const (
	// ControllerSourceTypeLocal reads the controller and its components from
	// paths on the machine running the installer. This is the default and is
	// the behaviour that existed before payload images were supported.
	ControllerSourceTypeLocal ControllerSourceType = "local"

	// ControllerSourceTypeImage reads every artifact from an OCI image the
	// provider publishes through their own release process.
	ControllerSourceTypeImage ControllerSourceType = "image"
)

// Controller describes the provider controller and where it is obtained from.
//
// The installer validates the payload's structure, not its content: it checks
// that the declared layout is present and that the declared compatibility
// range admits this release. It does not inspect, scan or attest the
// controller, the manifests or the hooks. What is inside is established by the
// provider's release process, which is the trust boundary.
type Controller struct {
	// SourceType is "local" or "image", and exactly one set of fields applies
	// to each. It defaults to "local".
	// +kubebuilder:validation:Enum=local;image
	// +kubebuilder:default:="local"
	// +default="local"
	// +optional
	SourceType ControllerSourceType `json:"sourceType,omitempty"`

	// Image is the payload image reference, required when SourceType is
	// "image" and rejected otherwise.
	//
	// The reference must be fully qualified. A tag is accepted for a connected
	// install: the install log records the resolved digest and the artifact
	// checksum, so a tagged install stays traceable after the fact even though
	// it is not reproducible from the install-config alone. A mirrored install
	// must pin a digest, because imageDigestSources redirects by digest.
	//
	// Authentication reuses pullSecret and redirection reuses
	// imageDigestSources. Neither needs a new field here.
	// +optional
	Image string `json:"image,omitempty"`

	// Path is an alternative spelling of BinaryPath for SourceType "local",
	// so that a local configuration can be written in the same grouped form as
	// an image one. Setting both is an error.
	// +optional
	Path string `json:"path,omitempty"`

	// Args are appended to the controller's command line, after the arguments
	// the installer supplies itself. Equivalent to ClusterAPIProvider.Args;
	// setting both is an error.
	// +optional
	Args []string `json:"args,omitempty"`
}

// SourceTypeOrDefault reports the configured source type, treating an unset
// Controller and an unset SourceType as "local".
//
// Every caller that has to tell the two modes apart goes through this, for the
// same reason ProvisionsWithClusterAPI exists: the pre-existing local-path
// behaviour has to survive unchanged, and it only does if the condition cannot
// drift between call sites.
func (c *Controller) SourceTypeOrDefault() ControllerSourceType {
	if c == nil || c.SourceType == "" {
		return ControllerSourceTypeLocal
	}
	return c.SourceType
}

// UsesImagePayload reports whether the provider's artifacts come from an OCI
// image rather than from local paths. A nil receiver reports false.
func (p *ClusterAPIProvider) UsesImagePayload() bool {
	return p != nil && p.Controller.SourceTypeOrDefault() == ControllerSourceTypeImage
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

// Hook is one program and the arguments the installer passes to it.
//
// The installer execs the program directly, without a shell, and hands it
// Args as its argv. It does not parse, expand, template or interpret any of
// them: an argument is a string the user wrote and the program reads.
//
// Arguments rather than more environment variables, and rather than a shell
// command line, is a deliberate choice about who owns the contract. The
// installer's own inputs -- the cluster's identity, the install directory,
// the Cluster API objects -- are environment, because they are the same for
// every hook and the installer knows what they mean. Everything that is
// specific to one partner's automation is argv, because the installer does
// not know what it means and should not have to. A program whose whole input
// is flags is also a program that can be a compiled binary as easily as a
// script, and can be run by hand from a terminal with the same arguments the
// installer used, which is what makes a failed hook debuggable.
type Hook struct {
	// Program is a path to an executable, relative to the External manifest
	// directory in the install directory. It must stay inside that
	// directory: the install directory is the unit a user copies, archives
	// and hands to someone else, and a hook reaching outside it would run
	// something that did not travel with it.
	Program string `json:"program"`

	// Args are passed to Program as its arguments, in order and verbatim.
	//
	// Prefer long flags with an explicit value, `--input-dns-zone=Z123` or
	// `--input-dns-zone Z123`, over positional arguments: the reference hook
	// for this pilot is a script today and may be a binary tomorrow, and a
	// flag survives that change where a position does not.
	//
	// These are written to the installer log and recorded in metadata.json so
	// that `destroy cluster` can run the teardown half of the same
	// automation. Do not put a credential in one.
	// +optional
	Args []string `json:"args,omitempty"`
}
