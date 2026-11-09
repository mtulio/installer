package payload

import (
	"path/filepath"
	"testing"

	"github.com/openshift/installer/pkg/types/external"
)

// TestPayloadHooksComeOnlyFromThePayload pins the collapse of hook
// configuration.
//
// Through the pilot a hook could be declared by the payload or overridden in
// the install-config, and each phase's root followed where that phase's
// program came from. That produced a real failure: an override on one phase
// moved every phase's root to the External manifest directory, so a payload
// that declared infraReady and a user who overrode postProvision got an
// install that looked for the payload's own program under external-install/
// and failed on a hook neither of them had got wrong.
//
// The override is gone. A payload's hooks and its templates are written
// together and have to agree, so a second place to name one was a way for them
// to disagree. What is left is the invariant worth holding: every hook comes
// from the descriptor, and every root is the payload root.
func TestPayloadHooksComeOnlyFromThePayload(t *testing.T) {
	const installDir = "/tmp/install-dir"
	root := filepath.Join(installDir, external.CapiInstallerDir)

	decl := func(program string) *HookDeclaration { return &HookDeclaration{Program: program} }

	for _, tc := range []struct {
		name     string
		declared *InstallerHooks

		wantPrograms map[string]string
		wantRoots    HookRoots
	}{{
		name: "every phase",
		declared: &InstallerHooks{
			Ignition:      &HookDeclaration{Program: "hooks/all.sh", BootstrapOutputPath: "pointer.ign"},
			InfraReady:    decl("hooks/all.sh"),
			PostProvision: decl("hooks/all.sh"),
			PreDestroy:    decl("hooks/all.sh"),
		},
		wantPrograms: map[string]string{
			"ignition": "hooks/all.sh", "infraReady": "hooks/all.sh",
			"postProvision": "hooks/all.sh", "preDestroy": "hooks/all.sh",
		},
		wantRoots: HookRoots{
			Ignition:      external.CapiInstallerDir,
			InfraReady:    external.CapiInstallerDir,
			PostProvision: external.CapiInstallerDir,
			PreDestroy:    external.CapiInstallerDir,
		},
	}, {
		// The ordinary case. Most payloads declare neither ignition nor
		// postProvision, and an undeclared phase is skipped rather than
		// defaulted to something.
		name:         "a phase the payload does not declare is skipped",
		declared:     &InstallerHooks{InfraReady: decl("hooks/dns.sh"), PreDestroy: decl("hooks/dns.sh")},
		wantPrograms: map[string]string{"infraReady": "hooks/dns.sh", "preDestroy": "hooks/dns.sh"},
		wantRoots: HookRoots{
			InfraReady: external.CapiInstallerDir,
			PreDestroy: external.CapiInstallerDir,
		},
	}, {
		name: "no installerHooks block at all",
	}, {
		name:     "an empty installerHooks block",
		declared: &InstallerHooks{},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			d := &Descriptor{Spec: Spec{OpenShift: OpenShift{InstallerHooks: tc.declared}}}

			hooks, roots := payloadHooks(d, installDir, root)

			if len(tc.wantPrograms) == 0 {
				if hooks != nil {
					t.Fatalf("expected no hooks, got %+v", hooks)
				}
				if roots != (HookRoots{}) {
					t.Errorf("expected no roots, got %+v", roots)
				}
				return
			}
			if hooks == nil {
				t.Fatal("expected hooks, got nil")
			}
			got := map[string]string{}
			for phase, h := range map[string]*external.Hook{
				"ignition": hooks.Ignition, "infraReady": hooks.InfraReady,
				"postProvision": hooks.PostProvision, "preDestroy": hooks.PreDestroy,
			} {
				if h != nil {
					got[phase] = h.Program
				}
			}
			for phase, want := range tc.wantPrograms {
				if got[phase] != want {
					t.Errorf("%s program: got %q, want %q", phase, got[phase], want)
				}
			}
			for phase := range got {
				if _, ok := tc.wantPrograms[phase]; !ok {
					t.Errorf("%s should have been skipped, got program %q", phase, got[phase])
				}
			}
			if roots != tc.wantRoots {
				t.Errorf("roots: got %+v, want %+v", roots, tc.wantRoots)
			}
		})
	}
}

// TestPayloadHooksCarryTheBootstrapOutputPath covers the one hook field
// besides the program.
//
// It has to survive into external.Hooks because that is what reaches
// metadata.json and what the Ignition provider reads back: the installer opens
// exactly the file the payload declared, and a hook that exits 0 without
// writing it fails the install there rather than at the cloud API. Dropping it
// here would be invisible until an ignition install.
func TestPayloadHooksCarryTheBootstrapOutputPath(t *testing.T) {
	const installDir = "/tmp/install-dir"
	root := filepath.Join(installDir, external.CapiInstallerDir)

	d := &Descriptor{Spec: Spec{OpenShift: OpenShift{InstallerHooks: &InstallerHooks{
		Ignition:   &HookDeclaration{Program: "hooks/stage.sh", BootstrapOutputPath: "staged/pointer.ign"},
		InfraReady: &HookDeclaration{Program: "hooks/dns.sh"},
	}}}}

	hooks, _ := payloadHooks(d, installDir, root)
	if hooks == nil || hooks.Ignition == nil {
		t.Fatal("expected an ignition hook")
	}
	if got := hooks.Ignition.BootstrapOutputPath; got != "staged/pointer.ign" {
		t.Errorf("bootstrapOutputPath: got %q, want %q", got, "staged/pointer.ign")
	}
	// And it is carried by that phase only -- there is nothing for any other
	// phase to write it to.
	if got := hooks.InfraReady.BootstrapOutputPath; got != "" {
		t.Errorf("infraReady carried a bootstrapOutputPath: %q", got)
	}
}
