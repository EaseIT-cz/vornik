package api

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/secretstore"
)

type memAgentSecrets struct {
	rows map[string]persistence.AgentSecretRow
}

func (m *memAgentSecrets) Upsert(_ context.Context, r persistence.AgentSecretRow) error {
	if m.rows == nil {
		m.rows = map[string]persistence.AgentSecretRow{}
	}
	m.rows[r.Namespace+"/"+r.Name] = r
	return nil
}
func (m *memAgentSecrets) Get(_ context.Context, ns, n string) (*persistence.AgentSecretRow, error) {
	r, ok := m.rows[ns+"/"+n]
	if !ok {
		return nil, persistence.ErrNotFound
	}
	return &r, nil
}
func (m *memAgentSecrets) List(_ context.Context, ns string) ([]persistence.AgentSecretRow, error) {
	var out []persistence.AgentSecretRow
	for _, r := range m.rows {
		if r.Namespace == ns {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}
func (m *memAgentSecrets) ListNamespaces(context.Context) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	for _, r := range m.rows {
		if !seen[r.Namespace] {
			seen[r.Namespace] = true
			out = append(out, r.Namespace)
		}
	}
	sort.Strings(out)
	return out, nil
}
func (m *memAgentSecrets) Delete(context.Context, string, string) error { return nil }
func (m *memAgentSecrets) CountAll(context.Context) (int, error)        { return len(m.rows), nil }

// countsButListsNothing reports rows from CountAll while listing none, as when
// rows vanish between the two calls or two drivers disagree.
type countsButListsNothing struct{ memAgentSecrets }

func (countsButListsNothing) CountAll(context.Context) (int, error) { return 3, nil }

// Review 20261002-8932 finding 1: when rows are counted but none can be
// sampled, nothing was opened, so the check must not certify the key.
func TestCheckSecretStoreKey_NothingSampledIsNotOK(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.key")
	if err := os.WriteFile(path, bytes.Repeat([]byte{1}, 32), 0o600); err != nil {
		t.Fatal(err)
	}
	h := &DoctorHandlers{}
	h.SetAgentSecretStore(&countsButListsNothing{}, path)
	got := h.checkSecretStoreKey(context.Background(), false)
	if got.Status != "WARNING" {
		t.Fatalf("got %s %q, want WARNING: nothing was opened", got.Status, got.Message)
	}
}

func TestCheckSecretStoreKey(t *testing.T) {
	keyA := bytes.Repeat([]byte{1}, 32)
	keyB := bytes.Repeat([]byte{2}, 32)
	sealed := func(t *testing.T) *memAgentSecrets {
		repo := &memAgentSecrets{}
		st, _ := secretstore.New(repo, keyA)
		if err := st.Put(context.Background(), "hermes", "FIO", "secret", []byte("VALUE-CANARY"), "dev"); err != nil {
			t.Fatal(err)
		}
		return repo
	}
	cases := []struct {
		name    string
		repo    func(t *testing.T) *memAgentSecrets
		key     []byte // nil = no key file
		status  string
		mention string
	}{
		{"no rows, no key", func(*testing.T) *memAgentSecrets { return &memAgentSecrets{} }, nil, "OK", "no agent credentials"},
		{"no rows, key present", func(*testing.T) *memAgentSecrets { return &memAgentSecrets{} }, keyA, "OK", "key present"},
		{"rows, key opens them", sealed, keyA, "OK", "key opens them"},
		{"rows, key missing", sealed, nil, "ERROR", "store.key"},
		{"rows, wrong key", sealed, keyB, "ERROR", "does not open"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "store.key")
			if c.key != nil {
				if err := os.WriteFile(path, c.key, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			h := &DoctorHandlers{}
			h.SetAgentSecretStore(c.repo(t), path)
			got := h.checkSecretStoreKey(context.Background(), false)
			if got.Status != c.status || !strings.Contains(got.Message, c.mention) {
				t.Fatalf("got %s %q, want %s mentioning %q", got.Status, got.Message, c.status, c.mention)
			}
			if strings.Contains(got.Message, "VALUE-CANARY") {
				t.Fatal("the check leaked a value")
			}
		})
	}
}
