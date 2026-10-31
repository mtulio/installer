package payload

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/openshift/installer/pkg/types/external"
)

// prepareExtracted fakes what a previous command in the same install
// directory leaves behind: an extracted payload and the state file naming it.
func prepareExtracted(t *testing.T, ref, digest string) (installDir, root string) {
	t.Helper()
	installDir = t.TempDir()
	root = filepath.Join(installDir, external.CapiInstallerDir)
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	descriptor := filepath.Join(root, external.PayloadDescriptorFile)
	if err := os.WriteFile(descriptor, []byte("apiVersion: x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	state := payloadState{Image: ref, Digest: digest, Root: root}
	if err := writeState(filepath.Join(installDir, stateFile), state); err != nil {
		t.Fatal(err)
	}
	return installDir, root
}

func optionsFor(ref, installDir string) ResolveOptions {
	return ResolveOptions{
		Provider:   &external.ClusterAPIProvider{Controller: &external.Controller{Image: ref}},
		InstallDir: installDir,
	}
}

// TestObtainReportsTheDigestOfAReusedExtraction is a regression test for a
// defect found by running the three install commands separately.
//
// The digest reaches metadata.json, which travels with a support bundle and
// is the only record of which build of a partner's provider a cluster was
// installed by. obtain used to return "" whenever it reused an extraction,
// which made the field empty in exactly the normal case: `create manifests`
// pulls and `create cluster` reuses, and the metadata the second one writes
// is the metadata that survives. A single-command install recorded it and a
// two-command install did not, which is the wrong way round -- the two-command
// form is what the published example and CI both use.
//
// No oc is needed: the reuse branch returns before any registry call, which
// is itself part of what this asserts.
func TestObtainReportsTheDigestOfAReusedExtraction(t *testing.T) {
	const (
		ref    = "quay.io/example/payload:v1"
		digest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	)
	installDir, root := prepareExtracted(t, ref, digest)

	got, reused, err := obtain(t.Context(), optionsFor(ref, installDir), root, installDir)
	if err != nil {
		t.Fatalf("obtain on a prepared directory: %v", err)
	}
	if !reused {
		t.Error("obtain did not report the extraction as reused")
	}
	if got != digest {
		t.Errorf("got digest %q, want %q carried over from the state file", got, digest)
	}
}

// TestObtainDoesNotReuseAcrossARenamedDirectory pins the other half.
//
// The state file records the root it extracted into, and a copied or renamed
// install directory must not inherit another directory's extraction on the
// strength of a path that no longer describes it. Without oc on PATH the
// re-fetch cannot succeed, which is the assertion: it got as far as trying.
func TestObtainDoesNotReuseAcrossARenamedDirectory(t *testing.T) {
	const ref = "quay.io/example/payload:v1"
	installDir, _ := prepareExtracted(t, ref, "sha256:"+
		"2222222222222222222222222222222222222222222222222222222222222222")

	// Same install directory, different extraction root: the shape a renamed
	// or copied directory produces, without needing to copy one.
	elsewhere := filepath.Join(installDir, "somewhere-else")
	t.Setenv("PATH", t.TempDir())

	if _, _, err := obtain(t.Context(), optionsFor(ref, installDir), elsewhere, installDir); err == nil {
		t.Fatal("obtain reused an extraction recorded against a different root")
	}
}
