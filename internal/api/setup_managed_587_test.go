package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/templates"
)

// newSeedRig587 is newSetupRig582 with the shipped template catalogue, a
// configs dir and a counting reload hook (GitHub #587,
// operations-api-contract-design.md §7.2.1).
func newSeedRig587(t *testing.T) (*rigSetup582, string, *int) {
	t.Helper()
	body := strings.Replace(managedYAML582(), "server:\n", "server:\n  public_base_url: https://acme.vornik.cloud\n", 1)
	path := writeConfig582(t, body)
	secretsDir := filepath.Join(t.TempDir(), "secrets")
	configsDir := t.TempDir()
	cfg, err := config.LoadFromPath(path)
	require.NoError(t, err)
	cat, err := templates.Load("../../configs/project-templates")
	require.NoError(t, err)
	reloads := 0
	lister := &gatewayLister582{models: []string{"open-a", "open-b"}}
	srv := NewServer(
		WithLogger(zerolog.Nop()),
		WithConfig(cfg),
		WithSetupConfigPath(path),
		WithSetupSecretsDir(secretsDir),
		WithSetupValidator(lister),
		WithAdminClassKeys(cfg.Admin),
		WithIssuerProbe(func(context.Context, string) error { return nil }),
		WithProjectTemplates(cat),
		WithConfigsDir(configsDir),
		WithConfigReloadHook(func() error { reloads++; return nil }),
	)
	hs := httptest.NewServer(NewRouter(srv, cfg).Handler())
	t.Cleanup(hs.Close)
	return &rigSetup582{path: path, secrets: secretsDir, hs: hs, lister: lister}, configsDir, &reloads
}

func withTemplates587(ts ...string) map[string]any {
	b := managedSetupBody582("owner-sub-1")
	b["templates"] = ts
	return b
}

func seeded587(out map[string]any) []string {
	var got []string
	for _, v := range out["seeded"].([]any) {
		got = append(got, v.(string))
	}
	return got
}

func TestSetupManaged587_SeedsTemplates(t *testing.T) {
	r, dir, _ := newSeedRig587(t)
	code, out := r.post(t, withTemplates587("engineering", "assistant"))
	require.Equal(t, http.StatusOK, code, "%v", out)
	require.Equal(t, true, out["changed"])
	require.ElementsMatch(t, []string{"companion", "assistant"}, seeded587(out))
	for _, f := range []string{"projects/companion.yaml", "swarms/companion-swarm.md", "projects/assistant.yaml", "swarms/assistant-swarm.md"} {
		_, err := os.Stat(filepath.Join(dir, f))
		require.NoError(t, err, "%s not written", f)
	}
	cfg, err := config.LoadFromPath(r.path)
	require.NoError(t, err)
	require.Equal(t, "assistant", cfg.Telegram.DispatcherProjectID, "the assistant template sets the dispatcher project")
}

func TestSetupManaged587_SeedingIsIdempotent(t *testing.T) {
	r, dir, _ := newSeedRig587(t)
	code, _ := r.post(t, withTemplates587("engineering", "assistant"))
	require.Equal(t, http.StatusOK, code)
	edited := []byte("# the customer's own edit\n")
	p := filepath.Join(dir, "projects", "assistant.yaml")
	require.NoError(t, os.WriteFile(p, edited, 0o600))
	before, _ := os.ReadFile(r.path)

	code, out := r.post(t, withTemplates587("engineering", "assistant"))
	require.Equal(t, http.StatusOK, code, "%v", out)
	require.Equal(t, false, out["changed"])
	require.Empty(t, seeded587(out))
	got, _ := os.ReadFile(p)
	require.Equal(t, edited, got, "setup must never overwrite a project")
	after, _ := os.ReadFile(r.path)
	require.Equal(t, string(before), string(after))

	// A template left out of a later call removes nothing.
	code, out = r.post(t, withTemplates587())
	require.Equal(t, http.StatusOK, code, "%v", out)
	_, err := os.Stat(filepath.Join(dir, "projects", "companion.yaml"))
	require.NoError(t, err)
}

func TestSetupManaged587_RefusesUnknownTemplate(t *testing.T) {
	for _, ts := range [][]string{{"devops"}, {"assistant", "assistant"}} {
		r, dir, _ := newSeedRig587(t)
		before, _ := os.ReadFile(r.path)
		code, out := r.post(t, withTemplates587(ts...))
		require.Equal(t, http.StatusBadRequest, code, "%v: %v", ts, out)
		after, _ := os.ReadFile(r.path)
		require.Equal(t, string(before), string(after), "%v: a refused call writes nothing", ts)
		entries, _ := os.ReadDir(dir)
		require.Empty(t, entries, "%v: a refused call seeds nothing", ts)
	}
}

func TestSetupManaged587_SeedsAMissingProjectWithoutConfigChange(t *testing.T) {
	r, dir, reloads := newSeedRig587(t)
	code, _ := r.post(t, withTemplates587("engineering", "assistant"))
	require.Equal(t, http.StatusOK, code)
	// Only the project file goes: its swarm file stays, and is kept as it
	// is (a partial project is completed, never overwritten).
	require.NoError(t, os.Remove(filepath.Join(dir, "projects", "companion.yaml")))
	swarm := filepath.Join(dir, "swarms", "companion-swarm.md")
	require.NoError(t, os.WriteFile(swarm, []byte("# kept\n"), 0o600))
	before, _ := os.ReadFile(r.path)
	n := *reloads

	code, out := r.post(t, withTemplates587("engineering", "assistant"))
	require.Equal(t, http.StatusOK, code, "%v", out)
	require.Equal(t, true, out["changed"])
	require.Equal(t, false, out["restart_required"], "a seeding alone needs no restart")
	require.Equal(t, []string{"companion"}, seeded587(out))
	require.Equal(t, n+1, *reloads, "a seeding alone reloads the registry")
	after, _ := os.ReadFile(r.path)
	require.Equal(t, string(before), string(after))
	got, _ := os.ReadFile(swarm)
	require.Equal(t, "# kept\n", string(got), "an existing file of the template is never overwritten")
	_, err := os.Stat(filepath.Join(dir, "projects", "companion.yaml"))
	require.NoError(t, err)
}

func TestSetupManaged587_TemplatesNeedTheCatalogue(t *testing.T) {
	r := newSetupRig582(t, nil) // no catalogue, no configs dir
	before, _ := os.ReadFile(r.path)
	code, out := r.post(t, withTemplates587("assistant"))
	require.Equal(t, http.StatusServiceUnavailable, code, "%v", out)
	require.Equal(t, "TEMPLATES_NOT_CONFIGURED", out["error"].(map[string]any)["code"])
	after, _ := os.ReadFile(r.path)
	require.Equal(t, string(before), string(after), "a refused call writes nothing")
}

func TestSetupManaged587_EngineeringAloneLeavesTheDispatcher(t *testing.T) {
	r, _, _ := newSeedRig587(t)
	code, out := r.post(t, withTemplates587("engineering"))
	require.Equal(t, http.StatusOK, code, "%v", out)
	require.Equal(t, []string{"companion"}, seeded587(out))
	cfg, err := config.LoadFromPath(r.path)
	require.NoError(t, err)
	require.Empty(t, cfg.Telegram.DispatcherProjectID)
}
