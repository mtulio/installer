package payload

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// TestInsecurePull pins which references are allowed to be pulled over plain
// HTTP. The rule is narrow on purpose, so the cases that must stay false
// matter more than the ones that must be true.
func TestInsecurePull(t *testing.T) {
	for _, tc := range []struct {
		ref  string
		want bool
	}{
		{"localhost:5000/payload:dev", true},
		{"localhost/payload:dev", true},
		{"127.0.0.1:5000/payload:dev", true},
		{"[::1]:5000/payload:dev", true},
		{"quay.io/example/payload:v1", false},
		{"registry.example.com:5000/payload:v1", false},
		// The giveaway cases: "localhost" appearing anywhere but the host.
		{"example.com/localhost/payload:v1", false},
		{"example.com/localhost:5000/payload:v1", false},
		{"localhost.example.com/payload:v1", false},
		{"notlocalhost:5000/payload:v1", false},
		// No registry host at all is not a reference we pull insecurely.
		{"payload", false},
	} {
		if got := insecurePull(tc.ref); got != tc.want {
			t.Errorf("insecurePull(%q) = %v, want %v", tc.ref, got, tc.want)
		}
	}
}

// TestIDMSArg checks that what is handed to `oc --idms-file` is a well-formed
// ImageDigestMirrorSet.
//
// Worth a test because a malformed document fails in the one configuration
// nobody runs locally: a mirrored, disconnected install. Parsing it back is
// the check, rather than comparing strings, so that quoting a reference with
// a colon and a digest in it cannot silently produce invalid YAML.
func TestIDMSArg(t *testing.T) {
	t.Run("no mirrors yields no flag", func(t *testing.T) {
		arg, cleanup, err := mirrorArg(nil, idmsFormat)
		if err != nil {
			t.Fatal(err)
		}
		defer cleanup()
		if arg != "" {
			t.Errorf("got %q, want no flag", arg)
		}
	})

	t.Run("mirrors round-trip", func(t *testing.T) {
		arg, cleanup, err := mirrorArg(map[string][]string{
			"quay.io/example":        {"mirror.internal:5000/example", "backup.internal:5000/example"},
			"registry.redhat.io/rhe": {"mirror.internal:5000/rhe"},
		}, idmsFormat)
		if err != nil {
			t.Fatal(err)
		}
		defer cleanup()

		path := strings.TrimPrefix(arg, "--idms-file=")
		if path == arg {
			t.Fatalf("got %q, want an --idms-file flag", arg)
		}
		data, err := os.ReadFile(path) //nolint:gosec // path is this test's own temp file.
		if err != nil {
			t.Fatal(err)
		}

		var idms struct {
			APIVersion string `json:"apiVersion"`
			Kind       string `json:"kind"`
			Metadata   struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Spec struct {
				ImageDigestMirrors []struct {
					Source  string   `json:"source"`
					Mirrors []string `json:"mirrors"`
				} `json:"imageDigestMirrors"`
			} `json:"spec"`
		}
		if err := yaml.UnmarshalStrict(data, &idms); err != nil {
			t.Fatalf("not a parseable ImageDigestMirrorSet: %v\n%s", err, data)
		}
		if idms.APIVersion != "config.openshift.io/v1" || idms.Kind != "ImageDigestMirrorSet" {
			t.Errorf("got %s/%s, want config.openshift.io/v1/ImageDigestMirrorSet", idms.APIVersion, idms.Kind)
		}
		if len(idms.Spec.ImageDigestMirrors) != 2 {
			t.Fatalf("got %d mirror entries, want 2", len(idms.Spec.ImageDigestMirrors))
		}
		// Sorted, so the order is pinned rather than whatever the map gave.
		first := idms.Spec.ImageDigestMirrors[0]
		if first.Source != "quay.io/example" {
			t.Errorf("got first source %q, want quay.io/example", first.Source)
		}
		if len(first.Mirrors) != 2 || first.Mirrors[0] != "mirror.internal:5000/example" {
			t.Errorf("got mirrors %v, want the two internal mirrors in order", first.Mirrors)
		}

		cleanup()
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("cleanup left %s behind", path)
		}
	})

	// The two formats are not interchangeable and the flag has to match the
	// kind, because oc 5.0 accepts --idms-file only on `image extract` and
	// --icsp-file only on `image info`. Sending the wrong one is a failure
	// that appears only in a mirrored install.
	t.Run("icsp format for oc image info", func(t *testing.T) {
		arg, cleanup, err := mirrorArg(map[string][]string{"quay.io/example": {"mirror.internal:5000/example"}}, icspFormat)
		if err != nil {
			t.Fatal(err)
		}
		defer cleanup()

		path, ok := strings.CutPrefix(arg, "--icsp-file=")
		if !ok {
			t.Fatalf("got %q, want an --icsp-file flag", arg)
		}
		data, err := os.ReadFile(path) //nolint:gosec // path is this test's own temp file.
		if err != nil {
			t.Fatal(err)
		}
		var icsp struct {
			APIVersion string `json:"apiVersion"`
			Kind       string `json:"kind"`
			Metadata   struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Spec struct {
				RepositoryDigestMirrors []struct {
					Source  string   `json:"source"`
					Mirrors []string `json:"mirrors"`
				} `json:"repositoryDigestMirrors"`
			} `json:"spec"`
		}
		if err := yaml.UnmarshalStrict(data, &icsp); err != nil {
			t.Fatalf("not a parseable ImageContentSourcePolicy: %v\n%s", err, data)
		}
		if icsp.Kind != "ImageContentSourcePolicy" || icsp.APIVersion != "operator.openshift.io/v1alpha1" {
			t.Errorf("got %s/%s, want operator.openshift.io/v1alpha1/ImageContentSourcePolicy", icsp.APIVersion, icsp.Kind)
		}
		if len(icsp.Spec.RepositoryDigestMirrors) != 1 {
			t.Fatalf("got %d entries under repositoryDigestMirrors, want 1", len(icsp.Spec.RepositoryDigestMirrors))
		}
	})
}

// TestCheckSize bounds the extracted tree.
func TestCheckSize(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "small"), make([]byte, 1024), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := checkSize(dir); err != nil {
		t.Errorf("a 1 KiB payload was rejected: %v", err)
	}
}

// TestRequireOCMessageNamesTheAlternative checks the error a user without oc
// will see.
//
// The message is the whole value of the check: failing with "exec: oc: not
// found" part-way through an install would leave someone guessing. It has to
// say what is missing and what the other option is.
func TestRequireOCMessageNamesTheAlternative(t *testing.T) {
	t.Setenv("PATH", t.TempDir())

	err := requireOC()
	if err == nil {
		t.Fatal("requireOC passed with an empty PATH")
	}
	for _, want := range []string{"oc", "oc image extract", "binaryPath"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q: %v", want, err)
		}
	}
}

// TestFetchWithoutOCFailsBeforeTouchingTheDisk makes sure the precondition is
// checked first, so a missing oc does not leave a half-made directory behind
// for the next run's reuse check to find.
func TestFetchWithoutOCFailsBeforeTouchingTheDisk(t *testing.T) {
	t.Setenv("PATH", t.TempDir())

	dest := filepath.Join(t.TempDir(), "capi-installer")
	if _, err := Fetch(t.Context(), "quay.io/example/payload:v1", dest, "", nil); err == nil {
		t.Fatal("Fetch succeeded with no oc on PATH")
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Errorf("Fetch created %s before failing", dest)
	}
}
