package registry

// GitHub #820: ResolveForge hands the forge provider its block's repository
// as the token scope, and an App-backed block's flag.

import "testing"

func TestResolveForge820_CarriesTheScope(t *testing.T) {
	p := &Project{ID: "a"}
	p.GitHub = ProjectGitHub{AppID: 1, InstallationID: 2, PrivateKeyPath: "/k", Repo: "acme/api"}
	cfg, ok := p.ResolveForge()
	if !ok || len(cfg.GitHub.Repositories) != 1 || cfg.GitHub.Repositories[0] != "acme/api" || cfg.GitHub.AppBacked {
		t.Fatalf("hand-made block: %+v", cfg.GitHub)
	}
	p.GitHub.Repo = ""
	if cfg, _ := p.ResolveForge(); len(cfg.GitHub.Repositories) != 0 {
		t.Fatalf("no repo must stay unscoped: %+v", cfg.GitHub)
	}
}
