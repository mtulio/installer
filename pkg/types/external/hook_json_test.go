package external

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestHooksFromLegacyMetadata is a regression test for a real failure: a
// cluster created before Hook became an object records its hooks in
// metadata.json as plain strings, and `destroy cluster` built after the change
// could not read its own install directory --
//
//	json: cannot unmarshal string into Go struct field
//	Hooks.clusterAPI.hooks.infraReady of type external.Hook
//
// which is the one file that says how to remove the cluster's DNS. The
// mixed case is the one to pin: an install directory can be re-run with a
// newer install-config, so both forms can appear side by side.
func TestHooksFromLegacyMetadata(t *testing.T) {
	for name, tc := range map[string]struct {
		json string
		want Hooks
	}{
		"legacy strings": {
			json: `{"infraReady":"hooks/infra-hook.sh","preDestroy":"hooks/infra-hook.sh"}`,
			want: Hooks{
				InfraReady: &Hook{Program: "hooks/infra-hook.sh"},
				PreDestroy: &Hook{Program: "hooks/infra-hook.sh"},
			},
		},
		"objects": {
			json: `{"postProvision":{"program":"hooks/dns.sh"}}`,
			want: Hooks{
				PostProvision: &Hook{Program: "hooks/dns.sh"},
			},
		},
		// bootstrapOutputPath is the one field besides program, and it is
		// carried only by ignition. It reaches metadata.json like the
		// rest, so destroy must be able to read a directory that has one.
		"ignition carries its output path": {
			json: `{"ignition":{"program":"hooks/stage.sh","bootstrapOutputPath":"pointer.ign"}}`,
			want: Hooks{
				Ignition: &Hook{Program: "hooks/stage.sh", BootstrapOutputPath: "pointer.ign"},
			},
		},
		"mixed": {
			json: `{"infraReady":"hooks/dns.sh","postProvision":{"program":"hooks/dns.sh"}}`,
			want: Hooks{
				InfraReady:    &Hook{Program: "hooks/dns.sh"},
				PostProvision: &Hook{Program: "hooks/dns.sh"},
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			var got Hooks
			require.NoError(t, json.Unmarshal([]byte(tc.json), &got))
			assert.Equal(t, tc.want, got)
		})
	}

	t.Run("neither form is an error naming both", func(t *testing.T) {
		var got Hook
		err := json.Unmarshal([]byte(`123`), &got)
		require.Error(t, err)
	})

	// Everything written from here on is the object form, so an install
	// directory does not silently keep the legacy shape alive.
	t.Run("always written as an object", func(t *testing.T) {
		data, err := json.Marshal(&Hook{Program: "hooks/dns.sh"})
		require.NoError(t, err)
		assert.JSONEq(t, `{"program":"hooks/dns.sh"}`, string(data))
	})
}
