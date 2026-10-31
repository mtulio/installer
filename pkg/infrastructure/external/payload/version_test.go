package payload

import (
	"os"
	"regexp"
	"testing"
)

// kubeAPIRequire matches the k8s.io/api line in the module's require blocks.
var kubeAPIRequire = regexp.MustCompile(`(?m)^\s*k8s\.io/api v0\.(\d+)\.`)

// TestKubernetesVersion keeps the constant a payload's compatibility range is
// checked against in step with the Kubernetes libraries this installer is
// actually built with.
//
// The constant cannot be derived at run time -- see version.go -- so the only
// thing standing between it and silent drift is this test. Drift here is not
// a cosmetic problem: a payload that correctly declares support for the new
// minor would be refused, and one that correctly declares it does not support
// it would be accepted.
func TestKubernetesVersion(t *testing.T) {
	data, err := os.ReadFile("../../../../go.mod")
	if err != nil {
		t.Fatalf("failed to read go.mod: %v", err)
	}
	m := kubeAPIRequire.FindSubmatch(data)
	if m == nil {
		t.Fatal("no k8s.io/api requirement found in go.mod; update this test if the dependency was renamed")
	}
	// k8s.io/api v0.N.x is Kubernetes 1.N.
	want := "1." + string(m[1])
	if KubernetesVersion != want {
		t.Errorf("KubernetesVersion is %q but go.mod requires k8s.io/api v0.%s, which is Kubernetes %s; "+
			"update the constant in version.go", KubernetesVersion, m[1], want)
	}
}
