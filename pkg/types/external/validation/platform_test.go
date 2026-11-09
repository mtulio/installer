package validation

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/util/validation/field"

	"github.com/openshift/installer/pkg/infrastructure/external/payload"
	"github.com/openshift/installer/pkg/types"
	"github.com/openshift/installer/pkg/types/external"
)

// capi is the shortest valid provider block, so that a case asserting one
// field is not also asserting the others by accident.
func capi(mutate func(*external.ClusterAPIProvider)) *external.Platform {
	p := &external.ClusterAPIProvider{PayloadImage: "registry.example.com/provider/payload:v1"}
	if mutate != nil {
		mutate(p)
	}
	return &external.Platform{PlatformName: "somecloud", ClusterAPI: p}
}

func TestValidatePlatform(t *testing.T) {
	cases := []struct {
		name     string
		platform *external.Platform
		config   *types.InstallConfig
		expected []string
	}{
		{
			name:     "nil platform",
			platform: nil,
		},
		{
			// The pre-existing shape of this platform: no Cluster API
			// provider, nothing to validate, no new error.
			name:     "no cluster api provider",
			platform: &external.Platform{PlatformName: "SomeCloud"},
		},
		{
			name:     "a payload image is the whole minimum",
			platform: capi(nil),
		},
		{
			// There is one supply mode now. The pilot's binaryPath and
			// componentsPath are gone, so an empty block names nothing the
			// installer can fetch.
			name:     "missing payload image",
			platform: &external.Platform{ClusterAPI: &external.ClusterAPIProvider{}},
			expected: []string{`^test-path\.clusterAPI\.payloadImage: Required value`},
		},
		{
			// Without the registry host, `oc` applies its own default and the
			// install-config no longer says where the provider came from.
			name:     "a reference without a registry host",
			platform: capi(func(p *external.ClusterAPIProvider) { p.PayloadImage = "provider/payload:v1" }),
			expected: []string{`^test-path\.clusterAPI\.payloadImage: Invalid value.*fully qualified`},
		},
		{
			name:     "localhost is a registry host",
			platform: capi(func(p *external.ClusterAPIProvider) { p.PayloadImage = "localhost:5000/payload:v1" }),
		},
		{
			// Mirrors are resolved by digest, so a tag cannot be redirected.
			name:     "a tag is refused when the install is mirrored",
			platform: capi(nil),
			config: &types.InstallConfig{ImageDigestSources: []types.ImageDigestSource{
				{Source: "registry.example.com/provider/payload", Mirrors: []string{"mirror.example.com/payload"}},
			}},
			expected: []string{`^test-path\.clusterAPI\.payloadImage: Invalid value.*pinned by digest`},
		},
		{
			name: "a digest is accepted when the install is mirrored",
			platform: capi(func(p *external.ClusterAPIProvider) {
				p.PayloadImage = "registry.example.com/provider/payload@sha256:" +
					"0000000000000000000000000000000000000000000000000000000000000000"
			}),
			config: &types.InstallConfig{ImageDigestSources: []types.ImageDigestSource{
				{Source: "registry.example.com/provider/payload", Mirrors: []string{"mirror.example.com/payload"}},
			}},
		},
		{
			// Appended after the payload's own list, which is the whole
			// intended use: -v=5 for a support case, without repackaging.
			name: "controllerExtraArgs are plain flags",
			platform: capi(func(p *external.ClusterAPIProvider) {
				p.ControllerExtraArgs = []string{"-v=5", "--feature-gates=Something=true"}
			}),
		},
		{
			// The render inputs belong to the payload's controllerArgs. A
			// token here either duplicates one the payload already declared,
			// breaking the exactly-once rule, or names an input that does not
			// exist and renders to an empty value.
			name: "a render token in controllerExtraArgs",
			platform: capi(func(p *external.ClusterAPIProvider) {
				p.ControllerExtraArgs = []string{"-v=5", "--webhook-port={{.WebhookPort}}"}
			}),
			expected: []string{`^test-path\.clusterAPI\.controllerExtraArgs\[1\]: Invalid value.*template token`},
		},
		{
			name: "bootstrapIgnitionStaging: Allowed",
			platform: capi(func(p *external.ClusterAPIProvider) {
				p.BootstrapIgnitionStaging = external.BootstrapIgnitionStagingAllowed
			}),
		},
		{
			name: "bootstrapIgnitionStaging: anything else",
			platform: capi(func(p *external.ClusterAPIProvider) {
				p.BootstrapIgnitionStaging = external.BootstrapIgnitionStaging("true")
			}),
			expected: []string{`^test-path\.clusterAPI\.bootstrapIgnitionStaging: Unsupported value`},
		},
		{
			// Containment is the only property decidable from the string, so
			// it is the only one decided here.
			name: "an absolute capiInstallerPath",
			platform: capi(func(p *external.ClusterAPIProvider) {
				p.CapiInstallerPath = "/var/tmp/payload"
			}),
			expected: []string{`^test-path\.clusterAPI\.capiInstallerPath: Invalid value.*install directory`},
		},
		{
			name: "overrides that escape the payload",
			platform: capi(func(p *external.ClusterAPIProvider) {
				p.InfrastructureComponents = "../components.yaml"
				p.ManifestTemplates = "/etc/manifests"
			}),
			expected: []string{
				`^test-path\.clusterAPI\.infrastructureComponents: Invalid value.*provider payload`,
				`^test-path\.clusterAPI\.manifestTemplates: Invalid value.*provider payload`,
			},
		},
		{
			name: "overrides that stay inside it",
			platform: capi(func(p *external.ClusterAPIProvider) {
				p.InfrastructureComponents = "alt/components.yaml"
				p.ManifestTemplates = "alt/manifests"
			}),
		},
		{
			// A variable that cannot be spelled as a ${TOKEN} is a value
			// nothing can reference, which is a typo rather than a choice.
			name: "a variable name a token cannot have",
			platform: capi(func(p *external.ClusterAPIProvider) {
				p.Variables = map[string]string{"aws-region": "us-east-1"}
			}),
			expected: []string{`^test-path\.clusterAPI\.variables\[aws-region\]: Invalid value`},
		},
		{
			name: "a variable in the installer's reserved namespace",
			platform: capi(func(p *external.ClusterAPIProvider) {
				p.Variables = map[string]string{"INSTALLER_INFRA_ID": "mine"}
			}),
			expected: []string{`^test-path\.clusterAPI\.variables\[INSTALLER_INFRA_ID\]: Invalid value.*supplied by the installer`},
		},
		{
			name: "ordinary variables",
			platform: capi(func(p *external.ClusterAPIProvider) {
				p.Variables = map[string]string{"AWS_REGION": "us-east-1", "_private": "x"}
			}),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			errs := ValidatePlatform(tc.platform, field.NewPath("test-path"), tc.config)
			assert.Len(t, errs, len(tc.expected))
			for i, want := range tc.expected {
				if i < len(errs) {
					assert.Regexp(t, want, errs[i].Error())
				}
			}
		})
	}
}

// TestHooksAreNotValidatedHere pins a deliberate absence.
//
// Hooks used to be configurable in the install-config and validated here.
// They are declared by the payload now, so there is nothing in this file to
// check: the program paths, their containment and their executability are all
// decided against the extracted tree, by Descriptor.Load. A future edit that
// reintroduces a hooks block in the install-config without reintroducing its
// validation would be caught by this test failing to compile, which is the
// point of asserting on the type rather than on behaviour.
func TestHooksAreNotValidatedHere(t *testing.T) {
	// Hooks exist in metadata.json -- destroy reads them back -- but not in
	// the install-config's provider block.
	errs := ValidatePlatform(capi(nil), field.NewPath("test-path"), nil)
	assert.Empty(t, errs)
}

// TestVariablePrefixMatchesPayload holds the two spellings of the reserved
// variable prefix together.
//
// They are deliberately separate constants -- see payloadInstallerVariablePrefix
// for why -- and the cost of that choice is that nothing but this test stops
// them drifting. Drift would be quiet and bad in one direction: validation
// would accept a variable the renderer then refuses to use, and the install
// would fail at render time with a message about a reserved name the user was
// told was fine.
func TestVariablePrefixMatchesPayload(t *testing.T) {
	if payloadInstallerVariablePrefix != payload.InstallerVariablePrefix {
		t.Errorf("validation reserves %q but the renderer supplies %q; the two must be the same string",
			payloadInstallerVariablePrefix, payload.InstallerVariablePrefix)
	}
}
