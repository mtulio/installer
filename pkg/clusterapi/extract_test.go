package clusterapi

import (
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"

	"github.com/openshift/installer/pkg/infrastructure/external/payload"
)

func TestInfrastructureProviderNames(t *testing.T) {
	names := InfrastructureProviderNames()
	require.NotEmpty(t, names)
	assert.True(t, sort.StringsAreSorted(names), "names must be sorted: %v", names)
	assert.Contains(t, names, AWS.Name)

	// The core controller and envtest are unpacked by the installer on every
	// run, whatever the platform. Offering them here would suggest a user has
	// to supply them, which would be wrong.
	assert.NotContains(t, names, ClusterAPI.Name)
	assert.NotContains(t, names, EnvTest.Name)

	// The companion components run alongside an infrastructure provider and
	// cannot provision a platform alone (system.go:277, :403). Offering one as
	// platform.external.clusterAPI would produce a configuration that starts
	// a controller and then never reports infrastructure ready.
	assert.NotContains(t, names, AzureASO.Name)
	assert.NotContains(t, names, OpenStackORC.Name)

	// Every name offered must have components to go with it, or extraction
	// yields a binary whose CRDs never reach the local control plane.
	for _, name := range names {
		assert.FileExists(t,
			filepath.Join("..", "..", "data", "data", "cluster-api", name+"-infrastructure-components.yaml"),
			"provider %q is offered for extraction but has no components", name)
	}
}

func TestExtractProviderUnknownProvider(t *testing.T) {
	_, err := ExtractProvider("no-such-provider", t.TempDir())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no-such-provider")
	// The message has to list what is available, or the user's only recourse
	// is to read the source.
	assert.Contains(t, err.Error(), AWS.Name)
}

// TestExtractProviderCompanionComponent covers a name a user can plausibly
// reach for -- it appears in build output and in the installer's own logs --
// but that cannot serve as an infrastructure provider.
func TestExtractProviderCompanionComponent(t *testing.T) {
	for _, name := range []string{AzureASO.Name, OpenStackORC.Name} {
		t.Run(name, func(t *testing.T) {
			_, err := ExtractProvider(name, t.TempDir())
			require.Error(t, err)
			assert.Contains(t, err.Error(), "companion component")
			// "Unknown" would be a lie and would send the user looking for a
			// typo, so the message must not claim it.
			assert.NotContains(t, err.Error(), "unknown")
		})
	}
}

func TestResolveDestDir(t *testing.T) {
	t.Run("an empty destination is rejected", func(t *testing.T) {
		// filepath.Abs("") is the working directory, so accepting this would
		// write 150MB wherever the user happened to be standing.
		for _, in := range []string{"", "   "} {
			_, err := resolveDestDir(in)
			assert.Error(t, err, "input %q", in)
		}
	})

	t.Run("a tilde is rejected rather than taken literally", func(t *testing.T) {
		// Nothing in the installer expands ~; silently creating a directory
		// named "~" is the worst of the available behaviours.
		_, err := resolveDestDir("~/artifacts")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "~")
	})

	t.Run("a relative path is made absolute", func(t *testing.T) {
		dir, err := resolveDestDir("artifacts")
		require.NoError(t, err)
		assert.True(t, filepath.IsAbs(dir), "want an absolute path, got %q", dir)
	})
}

// TestPayloadDescriptor checks the descriptor the way a payload author will
// use it: parsed by the same loader the installer uses.
//
// A skeleton that merely looks right is worthless -- the author only finds out
// when the installer refuses their image.
func TestPayloadDescriptor(t *testing.T) {
	extracted := &ExtractedProvider{
		Name:           "aws",
		BinaryPath:     filepath.Join("/artifacts", "cluster-api-provider-aws"),
		ComponentsPath: filepath.Join("/artifacts", "aws-infrastructure-components.yaml"),
	}

	var d payload.Descriptor
	// UnmarshalStrict with DisallowUnknownFields is what the descriptor
	// loader uses. Lax parsing here would accept a misspelled field.
	require.NoError(t, yaml.UnmarshalStrict([]byte(extracted.PayloadDescriptor()), &d, yaml.DisallowUnknownFields))

	assert.Equal(t, payload.APIVersion, d.APIVersion)
	assert.Equal(t, payload.Kind, d.Kind)
	// metadata.name must equal platform.external.platformName, and the
	// install is refused if they differ -- so the name of the provider that
	// was extracted is the only defensible value to pre-fill.
	assert.Equal(t, "aws", d.Metadata.Name)
	assert.Equal(t, "bin/controller", d.Spec.CAPI.Controller)
	assert.Equal(t, "infrastructure-components.yaml", d.Spec.CAPI.InfrastructureComponents)

	// The host paths are a comment, not a field. A descriptor that named them
	// would look like it could work unedited, and it cannot: a payload's paths
	// are relative to the payload root.
	for _, path := range []string{extracted.BinaryPath, extracted.ComponentsPath} {
		assert.Contains(t, extracted.PayloadDescriptor(), "#   ")
		assert.Contains(t, extracted.PayloadDescriptor(), path)
	}
	assert.NotContains(t, d.Spec.CAPI.Controller, "/artifacts")
	assert.NotContains(t, d.Spec.CAPI.InfrastructureComponents, "/artifacts")
}

// TestPayloadDescriptorArgsCarryTheRenderTokens pins the part of the skeleton
// a copy cannot guess.
//
// The payload owns the controller's whole command line now, so a descriptor
// that omitted a token would start a controller whose webhook the installer
// never reaches -- a late, obscure failure. The installer requires
// {{.WebhookPort}} and {{.WebhookCertDir}} exactly once each and appends
// --kubeconfig itself, so a skeleton that does not satisfy its own validator
// is worse than no skeleton.
func TestPayloadDescriptorArgsCarryTheRenderTokens(t *testing.T) {
	var d payload.Descriptor
	require.NoError(t, yaml.Unmarshal(
		[]byte((&ExtractedProvider{Name: "aws"}).PayloadDescriptor()), &d))

	args := d.Spec.CAPI.ControllerArgs
	require.NotEmpty(t, args)
	for _, token := range []string{payload.TokenWebhookPort, payload.TokenWebhookCertDir} {
		n := 0
		for _, a := range args {
			n += strings.Count(a, token)
		}
		assert.Equal(t, 1, n, "token %s must appear exactly once in %v", token, args)
	}
	for _, a := range args {
		assert.NotEqual(t, "--kubeconfig", strings.SplitN(a, "=", 2)[0],
			"the installer appends --kubeconfig; a payload must not declare it")
	}
}
