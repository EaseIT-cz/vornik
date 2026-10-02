package service

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/mcpauth"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/secretstore"
	"vornik.io/vornik/internal/storage"
)

// countingSecrets is an AgentSecretRepository that only answers CountAll and
// stores rows in memory for the post-boot store test.
type countingSecrets struct {
	n    int
	rows map[string]persistence.AgentSecretRow
}

func (r *countingSecrets) Upsert(_ context.Context, row persistence.AgentSecretRow) error {
	if r.rows == nil {
		r.rows = map[string]persistence.AgentSecretRow{}
	}
	r.rows[row.Namespace+"/"+row.Name] = row
	return nil
}
func (r *countingSecrets) Get(_ context.Context, ns, name string) (*persistence.AgentSecretRow, error) {
	row, ok := r.rows[ns+"/"+name]
	if !ok {
		return nil, persistence.ErrNotFound
	}
	return &row, nil
}
func (r *countingSecrets) List(context.Context, string) ([]persistence.AgentSecretRow, error) {
	return nil, nil
}
func (r *countingSecrets) ListNamespaces(context.Context) ([]string, error) { return nil, nil }
func (r *countingSecrets) Delete(context.Context, string, string) error     { return nil }
func (r *countingSecrets) CountAll(context.Context) (int, error)            { return r.n, nil }

func TestMCPGrants_CarryTheProjectNamespace(t *testing.T) {
	g := mcpGrantsFor("hermes--finance", []string{"hermes/FIO"})
	if g.Namespace != "hermes" {
		t.Fatalf("agent project namespace = %q", g.Namespace)
	}
	if g := mcpGrantsFor("assistant", []string{"GITHUB_TOKEN"}); g.Namespace != "" {
		t.Fatalf("operator project got namespace %q", g.Namespace)
	}
	// End to end through mcpauth: an agent project cannot reach the flat env.
	c := &Container{ConfigPath: filepath.Join(t.TempDir(), "config.yaml")}
	t.Setenv("GITHUB_TOKEN", "env-value")
	_, err := mcpauth.Resolve(mcpauth.Auth{Mode: mcpauth.ModeStatic, ValueFrom: "secret://GITHUB_TOKEN"},
		"streamable-http", c.secretSource(), mcpGrantsFor("hermes--finance", []string{"GITHUB_TOKEN"}))
	if err == nil {
		t.Fatal("agent project resolved a flat environment secret")
	}
}

// Review 20261002-8932 finding 4: the operator path keeps resolving flat
// names from the environment through the new source, including names with
// a dot or a dash.
func TestSecretSource_OperatorProjectStillResolvesFlatNames(t *testing.T) {
	c := &Container{ConfigPath: filepath.Join(t.TempDir(), "config.yaml")}
	c.repos = &storage.Repositories{AgentSecrets: &countingSecrets{}}
	t.Setenv("GITHUB_TOKEN", "gh-value")
	t.Setenv("my.token-v2", "dotted-value")
	for name, want := range map[string]string{"GITHUB_TOKEN": "gh-value", "my.token-v2": "dotted-value"} {
		inj, err := mcpauth.Resolve(mcpauth.Auth{Mode: mcpauth.ModeStatic, ValueFrom: "secret://" + name},
			"streamable-http", c.secretSource(), mcpGrantsFor("assistant", []string{name}))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		found := false
		for _, v := range inj.Headers {
			found = found || strings.Contains(v, want)
		}
		if !found {
			t.Fatalf("%s: injection %v does not carry the env value", name, inj.Headers)
		}
	}
}

// Plan review f329 finding 8 (Review Focus 2): rows exist, key missing. The
// lookup logs a key error naming the key path, never a plain "missing".
func TestSecretSource_KeyMissingWithRowsReportsTheKey(t *testing.T) {
	var buf bytes.Buffer
	c := &Container{ConfigPath: filepath.Join(t.TempDir(), "config.yaml"), Logger: zerolog.New(&buf)}
	c.repos = &storage.Repositories{AgentSecrets: &countingSecrets{n: 2}}
	if _, ok := c.secretSource().Get("hermes/FIO"); ok {
		t.Fatal("resolved without a key")
	}
	out := buf.String()
	if !strings.Contains(out, "store.key") || !strings.Contains(out, "unavailable") {
		t.Fatalf("log does not name the key problem: %s", out)
	}
}

// No rows and no key: a namespaced lookup is a quiet miss, not an error.
func TestSecretSource_NoRowsNoKeyIsQuiet(t *testing.T) {
	var buf bytes.Buffer
	c := &Container{ConfigPath: filepath.Join(t.TempDir(), "config.yaml"), Logger: zerolog.New(&buf)}
	c.repos = &storage.Repositories{AgentSecrets: &countingSecrets{}}
	if _, ok := c.secretSource().Get("hermes/FIO"); ok {
		t.Fatal("resolved without a key")
	}
	if buf.Len() != 0 {
		t.Fatalf("logged on a plain miss: %s", buf.String())
	}
}

// A source built before the key exists sees a store installed later (the
// first credential entry creates the key after boot).
func TestSecretSource_StoreInstalledAfterBootIsSeen(t *testing.T) {
	repo := &countingSecrets{}
	c := &Container{ConfigPath: filepath.Join(t.TempDir(), "config.yaml")}
	c.repos = &storage.Repositories{AgentSecrets: repo}
	src := c.secretSource()
	if _, ok := src.Get("hermes/FIO"); ok {
		t.Fatal("resolved before any store")
	}
	st, err := secretstore.New(repo, bytes.Repeat([]byte{3}, 32))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Put(context.Background(), "hermes", "FIO", "secret", []byte("v"), "dev"); err != nil {
		t.Fatal(err)
	}
	c.setSecretStore(st)
	if v, ok := src.Get("hermes/FIO"); !ok || v != "v" {
		t.Fatalf("after install = (%q, %v)", v, ok)
	}
}
