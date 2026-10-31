// Package external extracts External platform metadata from install
// configurations.
package external

import (
	"github.com/openshift/installer/pkg/infrastructure/external/payload"
	"github.com/openshift/installer/pkg/types"
	"github.com/openshift/installer/pkg/types/external"
)

// Metadata converts an install configuration to External metadata.
//
// The Cluster API provider block is recorded only when the platform asked for
// one. Without it `destroy cluster` has no way to start the provider that
// created the infrastructure, and the installer has no SDK of its own to fall
// back on.
func Metadata(config *types.InstallConfig) *external.Metadata {
	metadata := &external.Metadata{
		PlatformName: config.Platform.External.PlatformName,
		BaseDomain:   config.BaseDomain,
	}

	if capi := config.Platform.External.ClusterAPI; capi != nil {
		metadata.ClusterAPI = &external.ClusterAPIMetadata{
			Name:           capi.Name,
			BinaryPath:     capi.BinaryPath,
			ComponentsPath: capi.ComponentsPath,
			Args:           append([]string(nil), capi.Args...),
		}
		// Copied rather than shared: metadata is marshalled well after this
		// returns, and the install-config is not this function's to alias.
		// Deeply, because each hook is itself a pointer to a struct holding a
		// slice, and a shallow copy would alias both.
		metadata.ClusterAPI.Hooks = capi.Hooks.DeepCopy()

		recordPayload(capi, metadata.ClusterAPI)
	}

	return metadata
}

// recordPayload overwrites the artifact locations with the ones a provider
// payload actually resolved to.
//
// In image mode the install-config names none of them -- validation rejects a
// local path there -- so what is copied above is empty, and an empty
// BinaryPath in metadata.json is a cluster `destroy cluster` cannot remove.
//
// The resolution is read from the payload package rather than recomputed
// because it must be the same one the install used: the openshift manifests
// asset resolves the payload earlier in this same process, and a second
// resolution of a floating tag could legitimately produce a different image.
// If there is none, something resolved the platform without resolving its
// payload, and recording the empty paths would hide that until destroy.
func recordPayload(capi *external.ClusterAPIProvider, md *external.ClusterAPIMetadata) {
	if !capi.UsesImagePayload() {
		return
	}
	res := payload.Resolved()
	if res == nil {
		return
	}
	md.BinaryPath = res.ControllerPath
	md.ComponentsPath = res.ComponentsPath
	md.Args = append([]string(nil), res.Args...)
	md.Image = res.Image
	md.ImageDigest = res.Digest
	// Only the pre-destroy root is recorded, because pre-destroy is the only
	// hook `destroy cluster` runs. The other two have already run by the time
	// this metadata is written and nothing reads their root again.
	md.HookRoot = res.HookRoots.PreDestroy
	md.Hooks = res.Hooks.DeepCopy()
}
