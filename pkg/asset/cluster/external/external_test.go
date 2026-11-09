package external

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/openshift/installer/pkg/types"
	"github.com/openshift/installer/pkg/types/external"
)

func config(platform *external.Platform) *types.InstallConfig {
	return &types.InstallConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "example"},
		BaseDomain: "test.example.com",
		Platform:   types.Platform{External: platform},
	}
}

// TestMetadataWithoutClusterAPI covers the long-standing External install,
// which provisions nothing. It must keep recording nothing but the platform
// name: the ClusterAPI block is what tells `destroy cluster` there is
// infrastructure to hand back to a provider, and inventing one for an install
// that never had a provider would send destroy looking for a binary that was
// never configured.
func TestMetadataWithoutClusterAPI(t *testing.T) {
	got := Metadata(config(&external.Platform{PlatformName: "somecloud"}))

	require.NotNil(t, got)
	assert.Equal(t, "somecloud", got.PlatformName)
	assert.Nil(t, got.ClusterAPI)
}

// TestMetadataCopiesNothingFromTheInstallConfig pins the change that collapsed
// the install-config to a payload image.
//
// The install-config used to carry the controller path, the components path,
// the command line and the hooks, and this function copied each one into
// metadata.json. None of them exist there any more: they are inside the
// payload, and what destroy needs is where they were extracted to on this
// host, which only the resolution knows. So a ClusterAPI block is recorded --
// its presence is what tells destroy there is infrastructure to hand back --
// and it is filled in from payload.Resolved(), not from here.
//
// Asserting the empty block matters because the failure it guards is silent:
// a future edit that reads a path back out of the install-config would pass
// every install where the two happen to agree, and write a wrong path into
// metadata.json for every install where the payload was extracted elsewhere.
func TestMetadataCopiesNothingFromTheInstallConfig(t *testing.T) {
	got := Metadata(config(&external.Platform{
		PlatformName: "somecloud",
		ClusterAPI: &external.ClusterAPIProvider{
			PayloadImage: "registry.example.com/provider/payload:v1",
			Variables:    map[string]string{"REGION": "somewhere"},
		},
	}))

	require.NotNil(t, got.ClusterAPI)
	// No resolution was made in this process, so there is nothing to record.
	// recordPayload returns early rather than writing empty strings, which is
	// the same observable result and a deliberately different reason.
	assert.Empty(t, got.ClusterAPI.BinaryPath)
	assert.Empty(t, got.ClusterAPI.ComponentsPath)
	assert.Empty(t, got.ClusterAPI.Args)
	assert.Empty(t, got.ClusterAPI.Image)
	assert.Empty(t, got.ClusterAPI.ImageDigest)
	assert.Nil(t, got.ClusterAPI.Hooks)

	// The teardown hook is handed the same cluster identity the provisioning
	// hook was, and the base domain is the half of it that is not already in
	// ClusterMetadata. This one does come from the install-config.
	assert.Equal(t, "test.example.com", got.BaseDomain)
	assert.Equal(t, "somecloud", got.PlatformName)
}

// TestMetadataWithoutPayloadImage covers a ClusterAPI block that names no
// image. Validation refuses that before this code runs, so it is unreachable
// in an install -- but recordPayload is reached through Metadata by anything
// that builds a config in memory, and a nil-image block must not be treated as
// a resolved payload.
func TestMetadataWithoutPayloadImage(t *testing.T) {
	got := Metadata(config(&external.Platform{
		ClusterAPI: &external.ClusterAPIProvider{},
	}))

	require.NotNil(t, got.ClusterAPI)
	assert.Empty(t, got.ClusterAPI.BinaryPath)
	assert.Nil(t, got.ClusterAPI.Hooks)
}
