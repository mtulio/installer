package payload

import (
	"strings"
	"testing"
)

// descriptorRequiring returns a payload whose declared layout exists on disk,
// so that Validate reaches the environment check rather than failing earlier
// on a missing file.
func descriptorRequiring(t *testing.T, names ...string) (*Descriptor, string) {
	t.Helper()
	root := writeExtracted(t, map[string]string{
		"bin/controller":                 "#!/bin/sh\n",
		"infrastructure-components.yaml": "kind: List\n",
	})
	d := &Descriptor{
		APIVersion: APIVersion,
		Kind:       Kind,
		Metadata:   Metadata{Name: "somecloud"},
		Spec: Spec{
			CAPI: CAPI{
				Controller:               "bin/controller",
				InfrastructureComponents: "infrastructure-components.yaml",
				RequiredEnv:              names,
			},
		},
	}
	if err := d.ApplyExecutableModes(root); err != nil {
		t.Fatal(err)
	}
	return d, root
}

func TestValidateAcceptsADeclaredVariableThatIsSet(t *testing.T) {
	t.Setenv("SOMECLOUD_AUTH_DIR", "/some/path")
	d, root := descriptorRequiring(t, "SOMECLOUD_AUTH_DIR")
	if err := d.Validate(root); err != nil {
		t.Fatalf("expected the payload to validate, got: %v", err)
	}
}

func TestValidateRejectsAnUnsetDeclaredVariable(t *testing.T) {
	d, root := descriptorRequiring(t, "SOMECLOUD_AUTH_DIR_UNSET")
	err := d.Validate(root)
	if err == nil {
		t.Fatal("expected validation to fail on an unset required variable")
	}
	// The name has to be in the message: telling an operator that "a variable"
	// is missing is the failure mode this check exists to replace.
	if !strings.Contains(err.Error(), "SOMECLOUD_AUTH_DIR_UNSET") {
		t.Fatalf("the error must name the missing variable, got: %v", err)
	}
}

// An exported-but-empty variable is the shape a half-written .env file
// produces, and it fails the provider exactly as an unset one does.
func TestValidateRejectsAnEmptyDeclaredVariable(t *testing.T) {
	t.Setenv("SOMECLOUD_AUTH_DIR_EMPTY", "")
	d, root := descriptorRequiring(t, "SOMECLOUD_AUTH_DIR_EMPTY")
	if err := d.Validate(root); err == nil {
		t.Fatal("expected validation to fail on an empty required variable")
	}
}

// Every missing name in one error: a provider needing three variables should
// cost the operator one round trip, not three.
func TestValidateReportsEveryMissingVariableAtOnce(t *testing.T) {
	t.Setenv("SOMECLOUD_PRESENT", "yes")
	d, root := descriptorRequiring(t,
		"SOMECLOUD_MISSING_ONE", "SOMECLOUD_PRESENT", "SOMECLOUD_MISSING_TWO")
	err := d.Validate(root)
	if err == nil {
		t.Fatal("expected validation to fail")
	}
	for _, want := range []string{"SOMECLOUD_MISSING_ONE", "SOMECLOUD_MISSING_TWO"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error must name %s, got: %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "SOMECLOUD_PRESENT") {
		t.Errorf("the error must not name a variable that is set, got: %v", err)
	}
}

func TestValidateRejectsAnEmptyNameInTheDeclaration(t *testing.T) {
	d, root := descriptorRequiring(t, "")
	if err := d.Validate(root); err == nil {
		t.Fatal("expected validation to reject an empty name in spec.capi.requiredEnv")
	}
}

// A payload declaring nothing must behave exactly as it did before the field
// existed -- this is what keeps every existing payload valid.
func TestValidateIsInertWhenNothingIsDeclared(t *testing.T) {
	d, root := descriptorRequiring(t)
	if err := d.Validate(root); err != nil {
		t.Fatalf("a payload declaring no requiredEnv must validate, got: %v", err)
	}
}

// The declaration carries names, never values. A payload that smuggled a value
// in would be publishing it to everyone who can pull the image, so the parser
// must not quietly accept `NAME=value` as if it were a name.
func TestValidateRejectsANameThatLooksLikeAnAssignment(t *testing.T) {
	d, root := descriptorRequiring(t, "SOMECLOUD_TOKEN=s3cr3t")
	err := d.Validate(root)
	if err == nil {
		t.Fatal("expected validation to reject NAME=value in spec.capi.requiredEnv")
	}
	if strings.Contains(err.Error(), "s3cr3t") {
		t.Fatalf("the error must not echo the value, got: %v", err)
	}
}
