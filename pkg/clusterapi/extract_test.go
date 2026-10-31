package clusterapi

import (
	"path/filepath"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/yaml"

	"github.com/openshift/installer/pkg/types"
	externalvalidation "github.com/openshift/installer/pkg/types/external/validation"
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

// TestInstallConfigSnippet checks the snippet the way the installer will read
// it: pasted into an install-config and parsed strictly. A snippet that merely
// looks right is worthless -- the user only finds out at install time.
func TestInstallConfigSnippet(t *testing.T) {
	cases := []struct {
		name string
		dir  string
	}{
		{name: "an ordinary path", dir: "/artifacts"},
		// Both of these are legal directory names and both defeat a snippet
		// built with fmt.Sprintf: "#" truncates the value at the comment, and
		// ": " stops the document parsing at all.
		{name: "a path containing a hash", dir: "/tmp/a #b"},
		{name: "a path containing a colon and space", dir: "/tmp/a: b"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			extracted := &ExtractedProvider{
				Name:           "aws",
				BinaryPath:     filepath.Join(tc.dir, "cluster-api-provider-aws"),
				ComponentsPath: filepath.Join(tc.dir, "aws-infrastructure-components.yaml"),
			}

			// The snippet is a fragment, so it is pasted where the command
			// tells the user to paste it before being parsed.
			doc := "platform:\n  external:\n" + extracted.InstallConfigSnippet()

			var ic types.InstallConfig
			// UnmarshalStrict with DisallowUnknownFields is what
			// installconfigbase.go:48 uses. Lax parsing here would accept a
			// misspelled field and a duplicate key.
			require.NoError(t, yaml.UnmarshalStrict([]byte(doc), &ic, yaml.DisallowUnknownFields))

			require.NotNil(t, ic.Platform.External)
			require.NotNil(t, ic.Platform.External.ClusterAPI)
			assert.Equal(t, "aws", ic.Platform.External.ClusterAPI.Name)
			assert.Equal(t, extracted.BinaryPath, ic.Platform.External.ClusterAPI.BinaryPath)
			assert.Equal(t, extracted.ComponentsPath, ic.Platform.External.ClusterAPI.ComponentsPath)

			// Shape validation must pass too, or the user gets to the next
			// error rather than to an install.
			assert.Empty(t, externalvalidation.ValidatePlatform(ic.Platform.External, field.NewPath("platform", "external"), &ic))
		})
	}
}

// TestInstallConfigSnippetIsAFragment pins the reason the snippet is not a
// whole platform block: an install-config already has one, and the installer's
// strict parser rejects a duplicate key outright rather than merging.
func TestInstallConfigSnippetIsAFragment(t *testing.T) {
	snippet := (&ExtractedProvider{Name: "aws", BinaryPath: "/a", ComponentsPath: "/b"}).InstallConfigSnippet()
	assert.NotContains(t, snippet, "platform:")

	duplicated := "platform:\n  external:\n    platformName: aws\nplatform:\n  external: {}\n"
	var ic types.InstallConfig
	require.Error(t, yaml.UnmarshalStrict([]byte(duplicated), &ic),
		"a duplicate platform key must be an error, or this snippet need not be a fragment")
}
