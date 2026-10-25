package service

// GitHub #508 F3 (design 22, "Amendment 2026-10-24 (#508)"): a provenance
// clone uses only the requesting project's own GitHub App block, for its own
// repo owner on its own GitHub host; and the daemon applies
// memory.provenance to its automatic clone manager.

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/github"
	"vornik.io/vornik/internal/registry"
)

func TestProvenanceAppTokenFor508(t *testing.T) {
	src := &github.InstallationTokenSource{Mint: func(_ context.Context, api string, _, _ int64, _ string) (string, time.Time, error) {
		return "tok-" + api, time.Now().Add(time.Hour), nil
	}}
	p := &registry.Project{ID: "a"}
	p.GitHub = registry.ProjectGitHub{AppID: 1, InstallationID: 77, PrivateKeyPath: "/k", Repo: "Acme/widgets"}
	ctx := context.Background()
	if tok, id := provenanceAppTokenFor(ctx, src, p, "github.com", "acme"); tok == "" || id != "api.github.com/1/77" {
		t.Errorf("own owner on github.com: %q %q", tok, id)
	}
	for name, tc := range map[string]struct{ host, owner string }{
		"another owner": {"github.com", "other"},
		"another host":  {"ghe.corp", "acme"},
	} {
		if tok, _ := provenanceAppTokenFor(ctx, src, p, tc.host, tc.owner); tok != "" {
			t.Errorf("%s: minted a token", name)
		}
	}
	ghe := &registry.Project{ID: "b"}
	ghe.GitHub = registry.ProjectGitHub{AppID: 1, InstallationID: 9, PrivateKeyPath: "/k", Repo: "acme/x", APIBaseURL: "https://ghe.corp/api/v3"}
	if tok, _ := provenanceAppTokenFor(ctx, src, ghe, "ghe.corp", "acme"); tok == "" {
		t.Error("a GitHub Enterprise project's own host was refused")
	}
	if tok, _ := provenanceAppTokenFor(ctx, src, nil, "github.com", "acme"); tok != "" {
		t.Error("no project minted a token")
	}
	none := &registry.Project{ID: "c"}
	if tok, _ := provenanceAppTokenFor(ctx, src, none, "github.com", "acme"); tok != "" {
		t.Error("a project with no App block minted a token")
	}
}

func TestService508_AutoClonesApplyMemoryProvenance(t *testing.T) {
	ws := filepath.Join(t.TempDir(), "workspaces")
	if err := os.MkdirAll(ws, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{}
	cfg.Memory.Provenance = config.MemoryProvenanceConfig{Forges: []config.ProvenanceForgeConfig{{
		Name: "corp", Kind: "gitlab", ScopePrefix: "gitlab.corp/8443", BaseURL: "https://gitlab.corp:8443", Token: "t",
	}}}
	c := &Container{Logger: zerolog.Nop(), Config: cfg}
	a := c.provenanceAutoClones(ws)
	if a == nil || len(a.Forges) != 1 || a.AppToken == nil || a.AppTokenByIdentity == nil {
		t.Fatalf("auto clones = %+v", a)
	}
	if got := a.ExpectedOrigin("gitlab.corp/8443/g/r"); got != "gitlab.corp/8443/g/r" {
		t.Errorf("forge not applied: %q", got)
	}
	if c.provenanceAutoFetch() == nil {
		t.Error("the sweeper gets no AutoFetch")
	}
}
