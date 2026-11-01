package service

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/secretstore"
	"vornik.io/vornik/internal/storage"
)

// GitHub #571 U3a (mcp-server-authentication-design.md §15.2 A3): the
// one-time seal pass seals every plaintext token row, mints the first key
// only when nothing is sealed yet, refuses a key that opens none of the sealed
// values, quarantines one bad row, and never overwrites a concurrent writer.

type memTokens571 struct {
	persistence.MCPOAuthTokenRepository
	mu   sync.Mutex
	rows map[[3]string]*persistence.MCPOAuthToken
	// beforeReplace, when set, runs once before the first ReplaceTokens (a
	// concurrent writer between the pass's read and its update).
	beforeReplace func()
}

func newMemTokens571(rows ...*persistence.MCPOAuthToken) *memTokens571 {
	m := &memTokens571{rows: map[[3]string]*persistence.MCPOAuthToken{}}
	for _, r := range rows {
		cp := *r
		m.rows[[3]string{r.ProjectID, r.ServerName, r.OwnerUserID}] = &cp
	}
	return m
}

func (m *memTokens571) Get(_ context.Context, p, s, owner string) (*persistence.MCPOAuthToken, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rows[[3]string{p, s, owner}]
	if !ok {
		return nil, persistence.ErrNotFound
	}
	cp := *r
	return &cp, nil
}

func (m *memTokens571) Upsert(_ context.Context, t *persistence.MCPOAuthToken) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := *t
	m.rows[[3]string{t.ProjectID, t.ServerName, t.OwnerUserID}] = &cp
	return nil
}

func (m *memTokens571) ListAll(context.Context) ([]*persistence.MCPOAuthToken, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*persistence.MCPOAuthToken
	for _, r := range m.rows {
		cp := *r
		out = append(out, &cp)
	}
	return out, nil
}

func (m *memTokens571) ReplaceTokens(_ context.Context, p, s, owner, oa, or, na, nr string, needsReconnect bool) (bool, error) {
	if f := m.beforeReplace; f != nil {
		m.beforeReplace = nil
		f()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rows[[3]string{p, s, owner}]
	if !ok || r.AccessToken != oa || r.RefreshToken != or {
		return false, nil
	}
	r.AccessToken, r.RefreshToken = na, nr
	r.NeedsReconnect = r.NeedsReconnect || needsReconnect
	return true, nil
}

func (m *memTokens571) MarkNeedsReconnect(_ context.Context, p, s, owner string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r, ok := m.rows[[3]string{p, s, owner}]; ok {
		r.NeedsReconnect = true
	}
	return nil
}

func (m *memTokens571) WithRefreshLock(ctx context.Context, _, _, _ string, fn func(context.Context) error) error {
	return fn(ctx)
}

func (m *memTokens571) raw(p, s string) *persistence.MCPOAuthToken {
	r, _ := m.Get(context.Background(), p, s, "")
	return r
}

type countSecrets571 struct {
	persistence.AgentSecretRepository
	n int
}

func (c countSecrets571) CountAll(context.Context) (int, error) { return c.n, nil }

func sealPassContainer(t *testing.T, tokens *memTokens571, agentSecrets int) *Container {
	t.Helper()
	dir := t.TempDir()
	return &Container{
		Logger:     zerolog.Nop(),
		ConfigPath: filepath.Join(dir, "config.yaml"),
		repos:      &storage.Repositories{MCPOAuthTokens: tokens, AgentSecrets: countSecrets571{n: agentSecrets}},
	}
}

func plainGrant(project, server string) *persistence.MCPOAuthToken {
	return &persistence.MCPOAuthToken{ProjectID: project, ServerName: server, AccessToken: "AT-" + project + server, RefreshToken: "RT-" + project + server}
}

func TestSealPass571_SealsEveryPlaintextRowOnce(t *testing.T) {
	tokens := newMemTokens571(plainGrant("ops", "jira"), plainGrant("", "daemon-mail"), plainGrant("acme--crm", "crm"))
	c := sealPassContainer(t, tokens, 0)
	got := c.sealExistingTokens(context.Background())
	if got.Sealed != 3 || got.Refused {
		t.Fatalf("pass: %+v, want 3 sealed", got)
	}
	if _, err := os.Stat(c.storeKeyPath()); err != nil {
		t.Fatalf("no key was minted: %v", err)
	}
	for _, k := range [][2]string{{"ops", "jira"}, {"", "daemon-mail"}, {"acme--crm", "crm"}} {
		raw := tokens.raw(k[0], k[1])
		if !strings.HasPrefix(raw.AccessToken, secretstore.SealedPrefix) || !strings.HasPrefix(raw.RefreshToken, secretstore.SealedPrefix) {
			t.Errorf("%v is not sealed: %+v", k, raw)
		}
		open, err := c.agentSealedTokens().Get(context.Background(), k[0], k[1], "")
		if err != nil || open.AccessToken != "AT-"+k[0]+k[1] {
			t.Errorf("%v does not open to its token: %v", k, err)
		}
	}
	if again := c.sealExistingTokens(context.Background()); again.Sealed != 0 || again.Quarantined != 0 {
		t.Errorf("a second pass did work: %+v", again)
	}
}

// GitHub #571 U3b: the pass walks every owner's rows, so each row is read,
// sealed and quarantined under its own owner. Found at: review (the automated
// security review of 4b4b0a2dc); regression test at: unit, the pass over a
// store keyed as the drivers key it (project, server, owner).
func TestSealPass571_SealsAnOwnedRowUnderItsOwner(t *testing.T) {
	owned := plainGrant("ops", "mail")
	owned.OwnerUserID, owned.AccessToken, owned.RefreshToken = "alice", "AT-alice", "RT-alice"
	tokens := newMemTokens571(plainGrant("ops", "mail"), owned)
	c := sealPassContainer(t, tokens, 0)
	if got := c.sealExistingTokens(context.Background()); got.Sealed != 2 || got.Quarantined != 0 || got.Refused {
		t.Fatalf("pass: %+v, want both rows sealed", got)
	}
	for owner, want := range map[string]string{"": "AT-opsmail", "alice": "AT-alice"} {
		open, err := c.agentSealedTokens().Get(context.Background(), "ops", "mail", owner)
		if err != nil || open.AccessToken != want {
			t.Fatalf("owner %q: %+v, %v; want %q", owner, open, err, want)
		}
	}
}

func TestSealPass571_NoKeyAndSomethingSealedRefuses(t *testing.T) {
	// Another host's key sealed this row; this host has no key.
	sealed := plainGrant("ops", "old")
	sealed.AccessToken, sealed.RefreshToken = secretstore.SealedPrefix+"AAAA", ""
	tokens := newMemTokens571(sealed, plainGrant("ops", "jira"))
	c := sealPassContainer(t, tokens, 0)
	if got := c.sealExistingTokens(context.Background()); !got.Refused || got.Sealed != 0 {
		t.Fatalf("pass: %+v, want refused", got)
	}
	if _, err := os.Stat(c.storeKeyPath()); err == nil {
		t.Error("a key was minted although the database is already sealed")
	}
	if tokens.raw("ops", "jira").AccessToken != "AT-opsjira" {
		t.Error("a row was changed by a refused pass")
	}
	// Agent secrets stored elsewhere count as sealed too.
	c2 := sealPassContainer(t, newMemTokens571(plainGrant("ops", "jira")), 3)
	if got := c2.sealExistingTokens(context.Background()); !got.Refused {
		t.Fatalf("with agent secrets and no key: %+v, want refused", got)
	}
}

func TestSealPass571_WrongKeyRefusesOneBadRowIsQuarantined(t *testing.T) {
	tokens := newMemTokens571(plainGrant("ops", "a"), plainGrant("ops", "b"))
	c := sealPassContainer(t, tokens, 0)
	c.sealExistingTokens(context.Background()) // seals a and b under this host's key
	// A third row sealed under another key, and a plaintext row.
	other := sealPassContainer(t, newMemTokens571(plainGrant("ops", "c")), 0)
	other.sealExistingTokens(context.Background())
	foreign := other.repos.MCPOAuthTokens.(*memTokens571).raw("ops", "c")
	_ = tokens.Upsert(context.Background(), foreign)
	_ = tokens.Upsert(context.Background(), plainGrant("ops", "d"))

	got := c.sealExistingTokens(context.Background())
	if got.Refused || got.Quarantined != 1 || got.Sealed != 1 {
		t.Fatalf("pass: %+v, want 1 quarantined and 1 sealed", got)
	}
	q := tokens.raw("ops", "c")
	if q.AccessToken != "" || q.RefreshToken != "" || !q.NeedsReconnect {
		t.Errorf("the bad row was not quarantined: %+v", q)
	}
	// A host whose key opens none of the sealed values seals nothing.
	stranger := sealPassContainer(t, tokens, 0)
	if _, err := stranger.secretStoreForWrite(); err != nil {
		t.Fatal(err)
	}
	_ = tokens.Upsert(context.Background(), plainGrant("ops", "e"))
	if got := stranger.sealExistingTokens(context.Background()); !got.Refused {
		t.Fatalf("a key that opens nothing: %+v, want refused", got)
	}
	if tokens.raw("ops", "e").AccessToken != "AT-opse" {
		t.Error("a refused pass sealed a row")
	}
}

func TestSealPass571_NeverOverwritesAConcurrentWriter(t *testing.T) {
	tokens := newMemTokens571(plainGrant("ops", "jira"))
	c := sealPassContainer(t, tokens, 0)
	tokens.beforeReplace = func() {
		// A refresh lands between the pass's read and its update, through
		// the sealing wrapper.
		next := plainGrant("ops", "jira")
		next.AccessToken = "AT-REFRESHED"
		if err := c.agentSealedTokens().Upsert(context.Background(), next); err != nil {
			t.Fatal(err)
		}
	}
	if got := c.sealExistingTokens(context.Background()); got.Lost != 1 || got.Sealed != 0 {
		t.Fatalf("pass: %+v, want the row lost to the writer", got)
	}
	open, err := c.agentSealedTokens().Get(context.Background(), "ops", "jira", "")
	if err != nil || open.AccessToken != "AT-REFRESHED" {
		t.Fatalf("the concurrent writer's value was overwritten: %+v %v", open, err)
	}
}

// A host whose key does not open the stored agent secrets seals nothing, even
// when no token row is sealed yet (code review 59c4 #1): the agent-secret probe
// is the only guard in that window.
func TestSealPass571_KeyThatDoesNotOpenAgentSecretsRefuses(t *testing.T) {
	ctx := context.Background()
	tokens := newMemTokens571(plainGrant("ops", "jira"))
	secrets := &memSecrets571{}
	c := sealPassContainer(t, tokens, 0)
	c.repos.AgentSecrets = secrets
	// Another host's key sealed an agent secret into the shared repository.
	other, err := secretstore.New(secrets, bytes.Repeat([]byte{1}, 32))
	if err != nil {
		t.Fatal(err)
	}
	if err := other.Put(ctx, "acme", "API_KEY", "secret", []byte("v"), "device"); err != nil {
		t.Fatal(err)
	}
	// This host holds a different key file of its own.
	if _, err := secretstore.LoadOrCreateKey(c.storeKeyPath()); err != nil {
		t.Fatal(err)
	}
	if got := c.sealExistingTokens(ctx); !got.Refused || got.Sealed != 0 {
		t.Fatalf("pass: %+v, want refused", got)
	}
	if tokens.raw("ops", "jira").AccessToken != "AT-opsjira" {
		t.Error("a refused pass sealed a row")
	}
}

// memSecrets571 is an in-memory agent secret repository for the probe.
type memSecrets571 struct {
	persistence.AgentSecretRepository
	mu   sync.Mutex
	rows []persistence.AgentSecretRow
}

func (m *memSecrets571) Upsert(_ context.Context, r persistence.AgentSecretRow) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rows = append(m.rows, r)
	return nil
}

func (m *memSecrets571) Get(_ context.Context, ns, name string) (*persistence.AgentSecretRow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.rows {
		if m.rows[i].Namespace == ns && m.rows[i].Name == name {
			r := m.rows[i]
			return &r, nil
		}
	}
	return nil, persistence.ErrNotFound
}

func (m *memSecrets571) List(_ context.Context, ns string) ([]persistence.AgentSecretRow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []persistence.AgentSecretRow
	for _, r := range m.rows {
		if r.Namespace == ns {
			r.Ciphertext, r.Nonce = nil, nil
			out = append(out, r)
		}
	}
	return out, nil
}

func (m *memSecrets571) ListNamespaces(context.Context) ([]string, error) {
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
	return out, nil
}

func (m *memSecrets571) CountAll(context.Context) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.rows), nil
}
