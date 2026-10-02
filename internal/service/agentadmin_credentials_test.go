package service

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/agentadmin"
	"vornik.io/vornik/internal/approverdevice"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/secretstore"
	"vornik.io/vornik/internal/storage"
)

const credCanary = "CANARY-fio-token-7b1e6a0c9d"

func requestIDOf(res agentadmin.Result) string {
	return res.ApprovalURL[strings.LastIndex(res.ApprovalURL, "/")+1:]
}

// Design §8.2, plan P4.1/P4.2 end to end: request_credential files a slot
// (no proposal, the credential locked while pending); entering the value
// decides, then stores it in the agent's namespace; the value reaches no log
// line, no request row and no config file (the canary, with its
// denominator). A refused decision stores nothing and leaves a rotated
// credential's old value in place.
func TestAgentAdmin_CredentialSlotEndToEnd(t *testing.T) {
	f := newAgentAdminFixture(t)
	var logs bytes.Buffer
	f.c.Logger = zerolog.New(&logs)
	ctx := context.Background()
	f.do(agentadmin.VerbCreateProject, agentadmin.CreateProjectInput{Slug: "finance", Purpose: "Monthly finance"})
	f.approve(f.do(agentadmin.VerbAddMCPServer, agentadmin.AddMCPServerInput{Project: "finance", Name: "bank",
		URL: "https://bank.invalid/mcp", Auth: agentadmin.MCPAuthInput{Mode: "static", Credential: "FIO"}}))

	ask := agentadmin.RequestCredentialInput{Project: "finance", Name: "FIO", Purpose: "read balances", Kind: "secret"}
	res := f.do(agentadmin.VerbRequestCredential, ask)
	if res.Effect != agentadmin.EffectAwaiting || res.ChangeID != "" {
		t.Fatalf("request_credential: %+v", res)
	}
	if again := f.do(agentadmin.VerbRequestCredential, ask); again.Effect != agentadmin.EffectRefused {
		t.Fatalf("a second slot for a pending credential: %+v", again)
	}
	id := requestIDOf(res)
	req, err := f.c.repos.ApproverDevices.GetRequest(ctx, id)
	if err != nil || req.Kind != persistence.ApprovalKindCredentialSlot {
		t.Fatalf("request: %+v %v", req, err)
	}
	if err := f.svc.enterCredential(ctx, f.device, *req, req.RenderedSHA256, []byte(credCanary)); err != nil {
		t.Fatalf("enter: %v", err)
	}
	st := f.c.currentSecretSource().Store
	if st == nil {
		t.Fatal("no store after the first entry")
	}
	if got, err := st.Get(ctx, "hermes", "FIO"); err != nil || string(got) != credCanary {
		t.Fatalf("stored %q, %v", got, err)
	}
	// It resolves for the project's server, through the resolver every MCP
	// connection uses.
	if v, ok := f.c.secretSource().Get("hermes/FIO"); !ok || v != credCanary {
		t.Fatalf("the server's credential does not resolve (%v)", ok)
	}
	if fi, err := os.Stat(f.c.storeKeyPath()); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("key file: %v %v", fi, err)
	}
	decided, _ := f.c.repos.ApproverDevices.GetRequest(ctx, id)
	if decided.Status != persistence.ApprovalApproved || decided.DecidedByDevice != f.device.ID {
		t.Fatalf("the slot is %s by %q", decided.Status, decided.DecidedByDevice)
	}

	// Rotation with a stale hash: refused before anything is stored; the old
	// value stays.
	rot := f.do(agentadmin.VerbRequestCredential, ask)
	rreq, _ := f.c.repos.ApproverDevices.GetRequest(ctx, requestIDOf(rot))
	err = f.svc.enterCredential(ctx, f.device, *rreq, "stale", []byte("NEW-VALUE"))
	if err != approverdevice.ErrNotDecidable {
		t.Fatalf("a stale hash: %v", err)
	}
	if got, _ := st.Get(ctx, "hermes", "FIO"); string(got) != credCanary {
		t.Fatalf("a refused rotation changed the value to %q", got)
	}

	f.svc.bg.Wait() // the detached tools listing logs too
	// The canary, with its denominator.
	examined := 0
	check := func(where, s string) {
		examined++
		if strings.Contains(s, credCanary) {
			t.Errorf("the value reached %s", where)
		}
	}
	check("the log", logs.String())
	// The agent's own view (review 20261002-a5d8 F5).
	setup, err := f.svc.ListSetupJSON(ctx, f.key)
	if err != nil {
		t.Fatal(err)
	}
	rawSetup, _ := json.Marshal(setup)
	check("list_my_setup", string(rawSetup))
	for _, rid := range []string{id, requestIDOf(rot)} {
		r, _ := f.c.repos.ApproverDevices.GetRequest(ctx, rid)
		check("request "+rid, r.Sentence+string(r.Rendered)+r.ApplyError)
	}
	_ = filepath.WalkDir(f.cfgDir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Base(path) == "store.key" {
			return nil
		}
		b, _ := os.ReadFile(path)
		check(path, string(b))
		return nil
	})
	t.Logf("canary: examined %d places", examined)
	if examined < 5 {
		t.Fatalf("the canary examined only %d places", examined)
	}
}

// Plan P4.1: concurrent first entries make one key and one store; the key
// on disk opens what was stored. The read path never creates a key.
func TestSecretStoreForWrite_ConcurrentFirstEntry(t *testing.T) {
	dir := t.TempDir()
	c := &Container{ConfigPath: filepath.Join(dir, "config.yaml"), repos: &storage.Repositories{AgentSecrets: &countingSecrets{}}}
	c.ensureSecretStore()
	if _, err := os.Stat(c.storeKeyPath()); !os.IsNotExist(err) {
		t.Fatalf("the read path created a key: %v", err)
	}
	var wg sync.WaitGroup
	stores := make([]*secretstore.Store, 16)
	for i := range stores {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			st, err := c.secretStoreForWrite()
			if err != nil {
				t.Error(err)
			}
			stores[i] = st
		}(i)
	}
	wg.Wait()
	for _, st := range stores {
		if st != stores[0] {
			t.Fatal("two stores were built")
		}
	}
	ctx := context.Background()
	if err := stores[0].Put(ctx, "hermes", "A", "secret", []byte("one"), "dev"); err != nil {
		t.Fatal(err)
	}
	key, err := secretstore.LoadKey(c.storeKeyPath())
	if err != nil {
		t.Fatal(err)
	}
	reopened, _ := secretstore.New(c.repos.AgentSecrets, key)
	if got, err := reopened.Get(ctx, "hermes", "A"); err != nil || string(got) != "one" {
		t.Fatalf("the key on disk does not open the value: %q %v", got, err)
	}
}

// Rows sealed under a lost key: a first write must not mint a new key.
func TestSecretStoreForWrite_RefusesWhenRowsExistWithoutKey(t *testing.T) {
	c := &Container{ConfigPath: filepath.Join(t.TempDir(), "config.yaml"), repos: &storage.Repositories{AgentSecrets: &countingSecrets{n: 2}}}
	if _, err := c.secretStoreForWrite(); err == nil {
		t.Fatal("a key was created although sealed rows exist")
	}
	if _, err := os.Stat(c.storeKeyPath()); !os.IsNotExist(err) {
		t.Fatal("a key file was written")
	}
}
