package secretstore

import (
	"bytes"
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

type memRepo struct {
	mu   sync.Mutex
	rows map[string]persistence.AgentSecretRow
}

func newMemRepo() *memRepo                 { return &memRepo{rows: map[string]persistence.AgentSecretRow{}} }
func (m *memRepo) key(ns, n string) string { return ns + "/" + n }
func (m *memRepo) Upsert(_ context.Context, r persistence.AgentSecretRow) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if old, ok := m.rows[m.key(r.Namespace, r.Name)]; ok {
		r.CreatedAt = old.CreatedAt
	}
	m.rows[m.key(r.Namespace, r.Name)] = r
	return nil
}
func (m *memRepo) Get(_ context.Context, ns, n string) (*persistence.AgentSecretRow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rows[m.key(ns, n)]
	if !ok {
		return nil, persistence.ErrNotFound
	}
	return &r, nil
}
func (m *memRepo) List(_ context.Context, ns string) ([]persistence.AgentSecretRow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []persistence.AgentSecretRow
	for _, r := range m.rows {
		if r.Namespace == ns {
			r.Ciphertext, r.Nonce = nil, nil
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}
func (m *memRepo) Delete(_ context.Context, ns, n string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.rows, m.key(ns, n))
	return nil
}
func (m *memRepo) CountAll(context.Context) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.rows), nil
}
func (m *memRepo) ListNamespaces(context.Context) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
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

func newStore(t *testing.T, repo persistence.AgentSecretRepository, master []byte) *Store {
	t.Helper()
	s, err := New(repo, master)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// Review Focus 5: arbitrary bytes round-trip exactly.
func TestStore_RoundTripArbitraryBytes(t *testing.T) {
	s := newStore(t, newMemRepo(), bytes.Repeat([]byte{1}, 32))
	ctx := context.Background()
	for name, v := range map[string][]byte{
		"PEM":   []byte("-----BEGIN KEY-----\nabc\n-----END KEY-----\n"),
		"NUL":   {0, 'a', 0, 'b'},
		"LARGE": bytes.Repeat([]byte("x"), 64<<10),
	} {
		if err := s.Put(ctx, "hermes", name, "secret", v, "dev_1"); err != nil {
			t.Fatal(err)
		}
		got, err := s.Get(ctx, "hermes", name)
		if err != nil || !bytes.Equal(got, v) {
			t.Fatalf("%s: round trip failed (%v)", name, err)
		}
	}
}

func TestStore_CiphertextIsNotThePlaintext(t *testing.T) {
	repo := newMemRepo()
	s := newStore(t, repo, bytes.Repeat([]byte{1}, 32))
	_ = s.Put(context.Background(), "hermes", "FIO", "secret", []byte("hunter2-canary"), "d")
	row, _ := repo.Get(context.Background(), "hermes", "FIO")
	if bytes.Contains(row.Ciphertext, []byte("hunter2-canary")) {
		t.Fatal("plaintext stored")
	}
}

// A row copied into another namespace does not open: the per-namespace key
// and the additional data both bind it.
func TestStore_NamespaceSeparationIsCryptographic(t *testing.T) {
	repo := newMemRepo()
	s := newStore(t, repo, bytes.Repeat([]byte{1}, 32))
	ctx := context.Background()
	_ = s.Put(ctx, "hermes", "FIO", "secret", []byte("v"), "d")
	row, _ := repo.Get(ctx, "hermes", "FIO")
	row.Namespace = "codex"
	_ = repo.Upsert(ctx, *row)
	if _, err := s.Get(ctx, "codex", "FIO"); err == nil {
		t.Fatal("a row moved to another namespace opened")
	}
}

// Review Focus 2: the wrong master key is ErrKeyUnavailable, distinct from ErrNotFound.
func TestStore_WrongKeyIsKeyUnavailable(t *testing.T) {
	repo := newMemRepo()
	_ = newStore(t, repo, bytes.Repeat([]byte{1}, 32)).Put(context.Background(), "hermes", "FIO", "secret", []byte("v"), "d")
	_, err := newStore(t, repo, bytes.Repeat([]byte{2}, 32)).Get(context.Background(), "hermes", "FIO")
	if !errors.Is(err, ErrKeyUnavailable) {
		t.Fatalf("err = %v, want ErrKeyUnavailable", err)
	}
	if _, err := newStore(t, repo, bytes.Repeat([]byte{1}, 32)).Get(context.Background(), "hermes", "NOPE"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: %v, want ErrNotFound", err)
	}
}

func TestStore_RefusesBadNames_AndErrorsCarryNoValue(t *testing.T) {
	s := newStore(t, newMemRepo(), bytes.Repeat([]byte{1}, 32))
	ctx := context.Background()
	for _, c := range []struct{ ns, name string }{{"Hermes", "A"}, {"hermes", "lower"}, {"hermes", "A/B"}, {"", "A"}} {
		err := s.Put(ctx, c.ns, c.name, "secret", []byte("SECRET-CANARY"), "d")
		if err == nil {
			t.Fatalf("Put(%q, %q) accepted", c.ns, c.name)
		}
		if strings.Contains(err.Error(), "SECRET-CANARY") {
			t.Fatal("error carries the value")
		}
	}
	if err := s.Put(ctx, "hermes", "A", "plaintext", []byte("v"), "d"); err == nil {
		t.Fatal("unknown kind accepted")
	}
	// Review 20261002-a6af suggestion 7: Source treats an empty value as
	// unset, so the store refuses to hold one rather than keep a row that
	// reads as missing.
	if err := s.Put(ctx, "hermes", "A", "secret", nil, "d"); err == nil {
		t.Fatal("empty value accepted")
	}
}

func TestStore_ListIsMetadata(t *testing.T) {
	s := newStore(t, newMemRepo(), bytes.Repeat([]byte{1}, 32))
	ctx := context.Background()
	_ = s.Put(ctx, "hermes", "B", "secret", []byte("v"), "dev_9")
	_ = s.Put(ctx, "hermes", "A", "oauth_token", []byte("v"), "dev_9")
	m, err := s.List(ctx, "hermes")
	if err != nil || len(m) != 2 || m[0].Name != "A" || m[0].Kind != "oauth_token" || m[1].CreatedByDevice != "dev_9" {
		t.Fatalf("list = %+v, %v", m, err)
	}
}
