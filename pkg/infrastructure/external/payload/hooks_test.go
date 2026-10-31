package payload

import (
	"path/filepath"
	"testing"

	"github.com/openshift/installer/pkg/types/external"
)

// TestPayloadHooksRootFollowsOrigin covers the one thing about hook resolution
// that is easy to get wrong and expensive when it is: each phase's root has to
// follow where that phase's program came from.
//
// The mixed case is the regression. An override on one phase used to move every
// phase's root to the External manifest directory, so a payload that declared
// infraReady and a user who overrode postProvision produced an install that
// looked for the payload's own program under external-install/ and failed on a
// hook neither of them had got wrong.
func TestPayloadHooksRootFollowsOrigin(t *testing.T) {
	const installDir = "/tmp/install-dir"
	root := filepath.Join(installDir, external.CapiInstallerDir)

	hook := func(program string) *external.Hook { return &external.Hook{Program: program} }

	for _, tc := range []struct {
		name       string
		declared   *LayoutHooks
		configured *external.Hooks

		wantPrograms map[string]string
		wantRoots    HookRoots
	}{{
		name:     "payload only",
		declared: &LayoutHooks{InfraReady: "hooks/all.sh", PostProvision: "hooks/all.sh", PreDestroy: "hooks/all.sh"},
		wantPrograms: map[string]string{
			"infraReady": "hooks/all.sh", "postProvision": "hooks/all.sh", "preDestroy": "hooks/all.sh",
		},
		wantRoots: HookRoots{
			InfraReady:    external.CapiInstallerDir,
			PostProvision: external.CapiInstallerDir,
			PreDestroy:    external.CapiInstallerDir,
		},
	}, {
		name:         "override one, payload keeps the rest",
		declared:     &LayoutHooks{InfraReady: "hooks/all.sh", PostProvision: "hooks/all.sh", PreDestroy: "hooks/all.sh"},
		configured:   &external.Hooks{PostProvision: hook("hooks/mine.sh")},
		wantPrograms: map[string]string{"infraReady": "hooks/all.sh", "postProvision": "hooks/mine.sh", "preDestroy": "hooks/all.sh"},
		wantRoots: HookRoots{
			InfraReady:    external.CapiInstallerDir,
			PostProvision: external.ManifestDir,
			PreDestroy:    external.CapiInstallerDir,
		},
	}, {
		name:         "a phase the payload does not declare is skipped",
		declared:     &LayoutHooks{InfraReady: "hooks/dns.sh"},
		wantPrograms: map[string]string{"infraReady": "hooks/dns.sh"},
		wantRoots:    HookRoots{InfraReady: external.CapiInstallerDir},
	}, {
		name:         "install-config supplies a phase the payload does not",
		declared:     &LayoutHooks{InfraReady: "hooks/dns.sh"},
		configured:   &external.Hooks{PreDestroy: hook("hooks/teardown.sh")},
		wantPrograms: map[string]string{"infraReady": "hooks/dns.sh", "preDestroy": "hooks/teardown.sh"},
		wantRoots:    HookRoots{InfraReady: external.CapiInstallerDir, PreDestroy: external.ManifestDir},
	}, {
		name: "no hooks anywhere",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			d := &Descriptor{Layout: Layout{Hooks: tc.declared}}
			p := &external.ClusterAPIProvider{Hooks: tc.configured}

			hooks, roots := payloadHooks(d, p, installDir, root)

			if len(tc.wantPrograms) == 0 {
				if hooks != nil {
					t.Fatalf("expected no hooks, got %+v", hooks)
				}
				return
			}
			if hooks == nil {
				t.Fatal("expected hooks, got nil")
			}
			got := map[string]string{}
			for phase, h := range map[string]*external.Hook{
				"infraReady": hooks.InfraReady, "postProvision": hooks.PostProvision, "preDestroy": hooks.PreDestroy,
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
