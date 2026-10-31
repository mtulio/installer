package payload

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"testing"
)

// payloadImageEnv names a real payload image to pull. A registry is needed,
// so this cannot run unattended; the development loop is a local one:
//
//	podman build -t localhost:5000/openshift-install-external-capi-aws:dev \
//	  -f installer-labs/external-capi/examples/aws-capa/Containerfile
//	podman push --tls-verify=false localhost:5000/openshift-install-external-capi-aws:dev
//	PAYLOAD_TEST_IMAGE=localhost:5000/openshift-install-external-capi-aws:dev \
//	  go test ./pkg/infrastructure/external/payload/ -run Integration -v
const payloadImageEnv = "PAYLOAD_TEST_IMAGE"

var digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// TestFetchIntegration exercises the whole subprocess path against a real
// registry: oc discovery, digest resolution, extraction, and the executable
// modes that extraction does not preserve.
//
// The unit tests cover the arguments this builds; only a real oc against a
// real registry covers whether oc accepts them. That distinction has already
// cost two rounds -- `oc image info` rejects --idms-file, and it rejects an
// empty --registry-config -- and neither was visible without this.
func TestFetchIntegration(t *testing.T) {
	ref := os.Getenv(payloadImageEnv)
	if ref == "" {
		t.Skipf("skipping integration test: set %s to a pullable provider payload image", payloadImageEnv)
	}
	if _, err := exec.LookPath("oc"); err != nil {
		t.Skipf("skipping integration test: %v", err)
	}

	dest := filepath.Join(t.TempDir(), "capi-installer")

	// A mirror that matches nothing. It makes the run exercise both mirror
	// files -- oc wants a different flag and a different kind for `image
	// info` than for `image extract` -- without redirecting the pull.
	mirrors := map[string][]string{"quay.io/does-not-exist": {"mirror.invalid/nope"}}

	digest, err := Fetch(t.Context(), ref, dest, os.Getenv("PAYLOAD_TEST_PULL_SECRET"), mirrors)
	if err != nil {
		t.Fatalf("Fetch(%s): %v", ref, err)
	}
	if !digestPattern.MatchString(digest) {
		t.Errorf("got digest %q, want a sha256 digest", digest)
	}

	d, err := Load(dest)
	if err != nil {
		t.Fatalf("the extracted payload has no usable descriptor: %v", err)
	}

	// Nothing may be executable yet. If something is, oc has started
	// preserving modes and the guarantee in ApplyExecutableModes -- that the
	// descriptor is the only source of the executable bit -- needs revisiting
	// rather than silently widening.
	assertNoExecutables(t, dest)

	if err := d.ApplyExecutableModes(dest); err != nil {
		t.Fatalf("ApplyExecutableModes: %v", err)
	}
	if err := d.Validate(dest); err != nil {
		t.Fatalf("Validate rejected a freshly extracted payload: %v", err)
	}

	for rel := range d.executables() {
		info, err := os.Stat(filepath.Join(dest, rel))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm()&0o111 == 0 {
			t.Errorf("declared program %s is not executable after extraction (%s)", rel, info.Mode().Perm())
		}
	}

	// Re-extracting over the same directory is what a second `create
	// manifests` in one install directory does.
	if _, err := Fetch(t.Context(), ref, dest, os.Getenv("PAYLOAD_TEST_PULL_SECRET"), mirrors); err != nil {
		t.Errorf("second Fetch into a populated directory: %v", err)
	}
}

func assertNoExecutables(t *testing.T, root string) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode().Perm()&0o111 != 0 {
			rel, err := filepath.Rel(root, path)
			if err != nil {
				rel = path
			}
			t.Errorf("%s is executable straight out of extraction (%s); "+
				"oc used to normalise every file to 0640, and ApplyExecutableModes assumes it still does",
				rel, info.Mode().Perm())
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
