package service

// GitHub #820: a provenance clone's token is scoped to the project's outbound
// repository and carries the block's App-backed flag to the key guard.

import (
	"testing"

	"vornik.io/vornik/internal/registry"
)

func TestProvenanceMint820(t *testing.T) {
	p := &registry.Project{ID: "a"}
	p.GitHub = registry.ProjectGitHub{AppID: 1, InstallationID: 77, PrivateKeyPath: "/k", Repo: "Acme/widgets"}
	m := provenanceMint(p, "https://api.github.com")
	if len(m.Repositories) != 1 || m.Repositories[0] != "Acme/widgets" || m.AppBacked || m.KeyPath != "/k" {
		t.Fatalf("hand-made block: %+v", m)
	}
	p.GitHub.Repo = ""
	if m := provenanceMint(p, "x"); len(m.Repositories) != 0 {
		t.Fatalf("no repo must mint unscoped: %+v", m)
	}
}
