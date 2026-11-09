package payload

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeExtracted builds a directory shaped like one `oc image extract`
// produces: every regular file 0640, no executable bit anywhere, whatever the
// image declared.
func writeExtracted(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, content := range files {
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o640); err != nil { //nolint:gosec // matches what oc writes.
			t.Fatal(err)
		}
	}
	return root
}

func descriptorFor(controller string, hooks *InstallerHooks) *Descriptor {
	return &Descriptor{
		APIVersion: APIVersion,
		Kind:       Kind,
		Metadata:   Metadata{Name: "somecloud"},
		Spec: Spec{
			CAPI: CAPI{
				Controller:               controller,
				InfrastructureComponents: "infrastructure-components.yaml",
			},
			OpenShift: OpenShift{InstallerHooks: hooks},
		},
	}
}

func mode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}

// TestApplyExecutableModesIsTheOnlySourceOfTheExecutableBit is the core of the
// move to `oc image extract`.
//
// oc writes every file 0640 regardless of its mode in the image, so after
// extraction nothing is executable. What makes the controller and the hooks
// runnable is the descriptor, and only the descriptor. The assertion that
// matters is the second one: a program the payload ships but does not declare
// stays unexecutable, which is a stronger property than the previous
// extractor had -- it used to detect an undeclared executable after the fact,
// and now there cannot be one.
func TestApplyExecutableModesIsTheOnlySourceOfTheExecutableBit(t *testing.T) {
	root := writeExtracted(t, map[string]string{
		"bin/controller":                  "ELF",
		"hooks/infra-hook.sh":             "#!/bin/sh\n",
		"infrastructure-components.yaml":  "kind: CustomResourceDefinition\n",
		"manifests/99_external-ccm.yaml":  "kind: Deployment\n",
		"bin/undeclared-helper":           "ELF",
		"manifests/hidden-in-plain-sight": "#!/bin/sh\n",
	})
	d := descriptorFor("bin/controller", &InstallerHooks{
		InfraReady: &HookDeclaration{Program: "hooks/infra-hook.sh"},
	})

	if m := mode(t, filepath.Join(root, "bin/controller")); m&0o111 != 0 {
		t.Fatalf("precondition: controller was already executable (%s); the test is not reproducing oc", m)
	}

	if err := d.ApplyExecutableModes(root); err != nil {
		t.Fatal(err)
	}

	for _, rel := range []string{"bin/controller", "hooks/infra-hook.sh"} {
		if m := mode(t, filepath.Join(root, rel)); m&0o111 == 0 {
			t.Errorf("declared program %s is not executable (%s)", rel, m)
		}
	}
	for _, rel := range []string{
		"bin/undeclared-helper",
		"manifests/hidden-in-plain-sight",
		"infrastructure-components.yaml",
		"manifests/99_external-ccm.yaml",
	} {
		if m := mode(t, filepath.Join(root, rel)); m&0o111 != 0 {
			t.Errorf("undeclared file %s was made executable (%s)", rel, m)
		}
	}

	// And the pair is what a real resolve does, in this order.
	if err := d.Validate(root); err != nil {
		t.Errorf("Validate rejected a payload it had just been prepared for: %v", err)
	}
}

// TestValidateWithoutApplyExecutableModesFails is the negative half.
//
// If this ever passes, ApplyExecutableModes has stopped being load-bearing
// and the executable bit is coming from somewhere else -- which would mean
// the undeclared-executable guarantee is resting on the image again without
// anyone deciding that.
func TestValidateWithoutApplyExecutableModesFails(t *testing.T) {
	root := writeExtracted(t, map[string]string{
		"bin/controller":                 "ELF",
		"infrastructure-components.yaml": "kind: CustomResourceDefinition\n",
	})
	d := descriptorFor("bin/controller", nil)

	err := d.Validate(root)
	if err == nil {
		t.Fatal("Validate accepted a payload whose controller is not executable")
	}
	if !strings.Contains(err.Error(), "not executable") {
		t.Errorf("got %v, want a complaint about executability", err)
	}
}

// TestUndeclaredExecutableIsStillRejected keeps the sweep honest.
//
// The sweep can no longer be triggered by the image, so the case it now
// guards is a bug in this package: the declared set and the chmodded set
// disagreeing. Simulating that by chmodding a file ApplyExecutableModes did
// not touch is the closest a unit test gets, and it fails if someone deletes
// the walk on the grounds that it can never fire.
func TestUndeclaredExecutableIsStillRejected(t *testing.T) {
	root := writeExtracted(t, map[string]string{
		"bin/controller":                 "ELF",
		"infrastructure-components.yaml": "kind: CustomResourceDefinition\n",
		"bin/extra":                      "ELF",
	})
	d := descriptorFor("bin/controller", nil)

	if err := d.ApplyExecutableModes(root); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(root, "bin/extra"), 0o755); err != nil {
		t.Fatal(err)
	}

	err := d.Validate(root)
	if err == nil {
		t.Fatal("Validate accepted an undeclared executable")
	}
	if !strings.Contains(err.Error(), "bin/extra") {
		t.Errorf("got %v, want the offending path named", err)
	}
}

// TestApplyExecutableModesReportsAMissingProgram covers the declaration
// pointing at something the image does not contain.
func TestApplyExecutableModesReportsAMissingProgram(t *testing.T) {
	root := writeExtracted(t, map[string]string{
		"infrastructure-components.yaml": "kind: CustomResourceDefinition\n",
	})
	d := descriptorFor("bin/controller", nil)

	err := d.ApplyExecutableModes(root)
	if err == nil {
		t.Fatal("ApplyExecutableModes passed with no controller present")
	}
	if !strings.Contains(err.Error(), "bin/controller") {
		t.Errorf("got %v, want the missing path named", err)
	}
}

// TestApplyExecutableModesIsIdempotent matters because a reused extraction
// runs it a second time against a tree it already prepared.
func TestApplyExecutableModesIsIdempotent(t *testing.T) {
	root := writeExtracted(t, map[string]string{
		"bin/controller":                 "ELF",
		"infrastructure-components.yaml": "kind: CustomResourceDefinition\n",
	})
	d := descriptorFor("bin/controller", nil)

	if err := d.ApplyExecutableModes(root); err != nil {
		t.Fatal(err)
	}
	first := mode(t, filepath.Join(root, "bin/controller"))
	if err := d.ApplyExecutableModes(root); err != nil {
		t.Fatal(err)
	}
	if second := mode(t, filepath.Join(root, "bin/controller")); second != first {
		t.Errorf("mode changed on the second run: %s then %s", first, second)
	}
}

// TestReservedE2EIsStillMadeExecutable covers the one declared program the
// installer never runs.
//
// spec.capi.e2e is reserved: nothing in an install executes it. It is in the
// executable set anyway, because the alternative is a payload that ships a
// test entry point which validation then rejects as an undeclared executable
// -- the declaration would make the payload worse. The consequence is that
// declaring it obliges the payload to ship it, which is why a payload with no
// bin/e2e must omit the key rather than name a path it does not have.
func TestReservedE2EIsStillMadeExecutable(t *testing.T) {
	root := writeExtracted(t, map[string]string{
		"bin/controller":                 "ELF",
		"bin/e2e":                        "ELF",
		"infrastructure-components.yaml": "kind: CustomResourceDefinition\n",
	})
	d := descriptorFor("bin/controller", nil)
	d.Spec.CAPI.E2E = &E2E{Program: "bin/e2e"}

	if err := d.ApplyExecutableModes(root); err != nil {
		t.Fatal(err)
	}
	if m := mode(t, filepath.Join(root, "bin/e2e")); m&0o111 == 0 {
		t.Errorf("declared e2e program is not executable (%s)", m)
	}
	if err := d.Validate(root); err != nil {
		t.Errorf("Validate rejected a payload declaring e2e: %v", err)
	}
}

// TestDeclaredE2EMustExist is the other side of the same trade: every
// declared path must be present, so a payload that names a test entry point
// it does not ship fails at layout validation rather than at whatever later
// point someone tries to use it.
func TestDeclaredE2EMustExist(t *testing.T) {
	root := writeExtracted(t, map[string]string{
		"bin/controller":                 "ELF",
		"infrastructure-components.yaml": "kind: CustomResourceDefinition\n",
	})
	d := descriptorFor("bin/controller", nil)
	d.Spec.CAPI.E2E = &E2E{Program: "bin/e2e"}

	err := d.ApplyExecutableModes(root)
	if err == nil {
		t.Fatal("ApplyExecutableModes passed with a declared e2e program absent")
	}
	if !strings.Contains(err.Error(), "bin/e2e") {
		t.Errorf("got %v, want the missing path named", err)
	}
}
