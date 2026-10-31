package validation

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/util/validation/field"

	"github.com/openshift/installer/pkg/infrastructure/external/payload"
	"github.com/openshift/installer/pkg/types/external"
)

func TestValidatePlatform(t *testing.T) {
	cases := []struct {
		name     string
		platform *external.Platform
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
			name: "complete cluster api provider",
			platform: &external.Platform{
				ClusterAPI: &external.ClusterAPIProvider{
					Name:           "somecloud",
					BinaryPath:     "/artifacts/controller",
					ComponentsPath: "/artifacts/components.yaml",
				},
			},
		},
		{
			name: "args are optional",
			platform: &external.Platform{
				ClusterAPI: &external.ClusterAPIProvider{
					Name:           "somecloud",
					BinaryPath:     "/artifacts/controller",
					ComponentsPath: "/artifacts/components",
					Args:           []string{"--extra-flag=1"},
				},
			},
		},
		{
			name: "missing name",
			platform: &external.Platform{
				ClusterAPI: &external.ClusterAPIProvider{
					BinaryPath:     "/artifacts/controller",
					ComponentsPath: "/artifacts/components.yaml",
				},
			},
			expected: []string{`^test-path\.clusterAPI\.name: Required value`},
		},
		{
			name: "missing binary path",
			platform: &external.Platform{
				ClusterAPI: &external.ClusterAPIProvider{
					Name:           "somecloud",
					ComponentsPath: "/artifacts/components.yaml",
				},
			},
			expected: []string{`^test-path\.clusterAPI\.binaryPath: Required value`},
		},
		{
			name: "missing components path",
			platform: &external.Platform{
				ClusterAPI: &external.ClusterAPIProvider{
					Name:       "somecloud",
					BinaryPath: "/artifacts/controller",
				},
			},
			expected: []string{`^test-path\.clusterAPI\.componentsPath: Required value`},
		},
		{
			// Every missing field is reported at once: the point of
			// field.ErrorList is that a user fixes one install-config rather
			// than rerunning to discover the next omission.
			name: "empty cluster api provider reports every field",
			platform: &external.Platform{
				ClusterAPI: &external.ClusterAPIProvider{},
			},
			expected: []string{
				`^test-path\.clusterAPI\.name: Required value`,
				`^test-path\.clusterAPI\.binaryPath: Required value`,
				`^test-path\.clusterAPI\.componentsPath: Required value`,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			errs := ValidatePlatform(tc.platform, field.NewPath("test-path"), nil)
			assert.Len(t, errs, len(tc.expected))
			for i, want := range tc.expected {
				assert.Regexp(t, want, errs[i].Error())
			}
		})
	}
}

// TestValidateHooks covers the one property that can be decided from the
// install-config alone: a hook path stays inside the install directory.
//
// Existence and executability are deliberately not checked here. They need
// the filesystem, which this package does not have, and they are enforced
// where the hook is about to run -- one implementation rather than two that
// can disagree about what is valid.
func TestValidateHooks(t *testing.T) {
	provider := func(h *external.Hooks) *external.Platform {
		return &external.Platform{ClusterAPI: &external.ClusterAPIProvider{
			Name:           "reference",
			BinaryPath:     "/tmp/provider",
			ComponentsPath: "/tmp/components.yaml",
			Hooks:          h,
		}}
	}

	cases := []struct {
		name     string
		hooks    *external.Hooks
		expected []string
	}{{
		name:  "no hooks at all is valid",
		hooks: nil,
	}, {
		// Configuring neither half is supported: DNS may be created out of
		// band, which is what the non-Cluster-API External CI does.
		name:  "an empty hooks block is valid",
		hooks: &external.Hooks{},
	}, {
		name:  "relative paths in a subdirectory",
		hooks: &external.Hooks{InfraReady: &external.Hook{Program: "hooks/infra-hook.sh"}, PreDestroy: &external.Hook{Program: "hooks/infra-hook.sh"}},
	}, {
		name:  "a bare filename",
		hooks: &external.Hooks{InfraReady: &external.Hook{Program: "infra-hook.sh"}},
	}, {
		name:  "absolute paths escape the install directory",
		hooks: &external.Hooks{InfraReady: &external.Hook{Program: "/usr/local/bin/dns.sh"}},
		expected: []string{
			`^test-path\.clusterAPI\.hooks\.infraReady\.program: Invalid value.*relative path inside`,
		},
	}, {
		name:  "parent traversal escapes the install directory",
		hooks: &external.Hooks{PreDestroy: &external.Hook{Program: "../../../bin/sh"}},
		expected: []string{
			`^test-path\.clusterAPI\.hooks\.preDestroy\.program: Invalid value.*relative path inside`,
		},
	}, {
		// Reported together, like every other field in this package.
		name:  "both halves are reported at once",
		hooks: &external.Hooks{InfraReady: &external.Hook{Program: "/abs/one.sh"}, PreDestroy: &external.Hook{Program: "../two.sh"}},
		expected: []string{
			`^test-path\.clusterAPI\.hooks\.infraReady\.program: Invalid value`,
			`^test-path\.clusterAPI\.hooks\.preDestroy\.program: Invalid value`,
		},
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			errs := ValidatePlatform(provider(tc.hooks), field.NewPath("test-path"), nil)
			assert.Len(t, errs, len(tc.expected))
			for i, want := range tc.expected {
				assert.Regexp(t, want, errs[i].Error())
			}
		})
	}
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
