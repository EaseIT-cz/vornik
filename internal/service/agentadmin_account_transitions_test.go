package service

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"vornik.io/vornik/internal/agentadmin"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/mcp"
	"vornik.io/vornik/internal/mcpauth"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

func TestAgentAccount_DaemonMissingGrantStoreFailsClosed(t *testing.T) {
	f := accountGrantFixture(t, agentadmin.MCPAuthInput{Mode: "none"}, map[string]*agentadmin.AccountInput{"mail": {Label: "work"}})
	ctx := context.Background()
	f.c.repos.BrokerGrants = nil
	tok := &persistence.MCPOAuthToken{ServerName: "daemon-mail", AccessToken: "candidate"}
	if handled, err := f.c.publishOAuthAccount(ctx, tok, "", false); !handled || err == nil {
		t.Fatal("missing grant denominator allowed publication")
	}
	if _, err := f.c.repos.MCPOAuthTokens.Get(ctx, "", "daemon-mail"); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatal("missing grant store published token")
	}
	if _, err := f.c.accountDecisionEpoch(ctx, "hermes"); !errors.Is(err, persistence.ErrAccountTransition) {
		t.Fatal("missing denominator cleared busy")
	}
}

type rotateOnSnapshot struct {
	persistence.AgentGrantRepository
	c       *Container
	rotated bool
}

func (r *rotateOnSnapshot) ListIntegrations(ctx context.Context, ns string) ([]persistence.AgentIntegrationApproval, error) {
	if !r.rotated {
		r.rotated = true
		ticket, err := r.c.beginAccountTransition(ctx, persistence.AccountDaemonScope, "snapshot-test")
		if err != nil {
			return nil, err
		}
		if err = r.c.repos.AccountTransitions.Finish(ctx, ticket, persistence.AccountPublication{}); err != nil {
			return nil, err
		}
	}
	return r.AgentGrantRepository.ListIntegrations(ctx, ns)
}

func TestAgentAccount_InertCapturesEpochBeforeSnapshot(t *testing.T) {
	f := accountGrantFixture(t, agentadmin.MCPAuthInput{Mode: "none"}, map[string]*agentadmin.AccountInput{"mail": {Label: "work"}})
	f.svc.grants = &rotateOnSnapshot{AgentGrantRepository: f.svc.grants, c: f.c}
	f.svc.engine.Reload = func() error { t.Fatal("stale snapshot reached filesystem apply"); return nil }
	input, _ := json.Marshal(agentadmin.RemoveInput{Kind: "integration", Project: "comms", ID: "mail"})
	_, err := f.svc.doVerb(context.Background(), f.key, agentadmin.VerbRemove, input)
	if !errors.Is(err, persistence.ErrAccountTransition) {
		t.Fatalf("inert accepted daemon rotation during state snapshot: %v", err)
	}
}

func TestAgentAccount_ConfigRequiresCapturedSnapshot(t *testing.T) {
	f := accountGrantFixture(t, agentadmin.MCPAuthInput{Mode: "none"}, map[string]*agentadmin.AccountInput{"mail": {Label: "work"}})
	if _, _, err := f.c.beginConfigAccountTransition(context.Background(), "hermes", []agentadmin.IntegrationRef{{Project: "hermes--comms", Name: "mail"}}); !errors.Is(err, persistence.ErrAccountTransition) {
		t.Fatal("absent snapshot silently recaptured")
	}
	if _, err := f.c.accountDecisionEpoch(context.Background(), "hermes"); err != nil {
		t.Fatal("missing snapshot left transition busy")
	}
}

type rotateBeforeConfigBegin struct {
	persistence.AccountTransitionRepository
	scope string
}

func (r *rotateBeforeConfigBegin) BeginConfig(ctx context.Context, scope, owner, operation string, now time.Time, guard persistence.AccountDecisionGuard) (persistence.AccountTransitionTicket, error) {
	ticket, err := r.Begin(ctx, r.scope, "interleaved", "test", now)
	if err != nil {
		return persistence.AccountTransitionTicket{}, err
	}
	if err = r.Finish(ctx, ticket, persistence.AccountPublication{}); err != nil {
		return persistence.AccountTransitionTicket{}, err
	}
	return r.AccountTransitionRepository.BeginConfig(ctx, scope, owner, operation, now, guard)
}

func TestAgentAccount_WideningStaleSnapshotRefusesBeforeApply(t *testing.T) {
	for _, scope := range []string{persistence.AccountNamespaceScope("hermes"), persistence.AccountDaemonScope} {
		t.Run(scope, func(t *testing.T) {
			f := accountGrantFixture(t, agentadmin.MCPAuthInput{Mode: "none"}, map[string]*agentadmin.AccountInput{"mail": {Label: "work"}})
			res := f.do(agentadmin.VerbAddMCPServer, agentadmin.AddMCPServerInput{Project: "comms", Name: "extra", URL: "https://extra.example/mcp", Auth: agentadmin.MCPAuthInput{Mode: "static", Credential: "EXTRA"}})
			req, err := f.c.repos.ApproverDevices.GetRequest(context.Background(), filepath.Base(res.ApprovalURL))
			if err != nil {
				t.Fatal(err)
			}
			if res.Effect != EffectAwaiting {
				t.Fatal("fixture did not file widening change")
			}
			f.c.repos.AccountTransitions = &rotateBeforeConfigBegin{AccountTransitionRepository: f.c.repos.AccountTransitions, scope: scope}
			f.svc.engine.Reload = func() error { t.Fatal("stale widening snapshot reached filesystem apply"); return nil }
			if err := f.c.approverDeviceService().Decide(context.Background(), f.device, req.ID, req.RenderedSHA256, true); err == nil {
				t.Fatal("widening accepted stale captured epoch")
			}
			if _, err := f.c.accountDecisionEpoch(context.Background(), "hermes"); err != nil {
				t.Fatal("preapply refusal left namespace busy")
			}
		})
	}
}

func TestAgentAccount_StaticCredentialFreshWithoutReload(t *testing.T) {
	f := accountGrantFixture(t, agentadmin.MCPAuthInput{Mode: "static"}, map[string]*agentadmin.AccountInput{"mail": {Label: "work"}})
	cfg := mcp.ServerConfig{Name: "mail", Transport: "streamable-http"}
	auth := mcpauth.Auth{Mode: "static", Header: "Authorization", ValueFrom: "secret://hermes/MAIL_TOKEN", ValuePrefix: "Bearer "}
	if !f.c.applyMCPAuth(&cfg, auth, mcpauth.Grants{Unrestricted: true}, "hermes--comms", "test") || cfg.AuthHeaderProvider == nil {
		t.Fatal("static credential was frozen in headers")
	}
	ctx := context.Background()
	before, err := cfg.AuthHeaderProvider(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.svc.storeCredential(ctx, approvalPayload{Slot: &agentadmin.CredentialSlot{Namespace: "hermes", Name: "MAIL_TOKEN"}}, f.device.ID, []byte("rotated")); err != nil {
		t.Fatal(err)
	}
	after, err := cfg.AuthHeaderProvider(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if before["Authorization"] == after["Authorization"] || after["Authorization"] != "Bearer rotated" {
		t.Fatal("next request retained the old static credential")
	}
}

func TestAgentAccount_OAuthFreshDespiteWarmCache(t *testing.T) {
	f, vendor := oauthWriteFixture(t)
	signInFromPhone(t, f, "freshness-code")
	ref, ok := f.c.mcpServerRef("hermes--work", "jira")
	if !ok {
		t.Fatal("missing ref")
	}
	ctx := context.Background()
	conn := f.c.mcpConnector()
	old, err := conn.CachedAccessToken(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	cfg := mcp.ServerConfig{Name: "jira", Transport: "streamable-http"}
	if !f.c.applyMCPOAuthToken(&cfg, "hermes--work", "test") {
		t.Fatal("no auth")
	}
	tok, err := conn.Tokens.Get(ctx, "hermes--work", "jira")
	if err != nil {
		t.Fatal(err)
	}
	tok.AccessToken = old + "-rotated"
	expiry := time.Now().Add(time.Hour)
	tok.ExpiresAt = &expiry
	if handled, err := f.c.publishOAuthAccount(ctx, tok, "", false); !handled || err != nil {
		t.Fatalf("publish: %v/%v", handled, err)
	}
	_ = vendor
	headers, err := cfg.AuthHeaderProvider(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if headers["Authorization"] != "Bearer "+tok.AccessToken {
		t.Fatal("agent request reused a warm token cache")
	}
}

func TestAgentAccount_InheritedIdentityIsUnverified(t *testing.T) {
	f := accountGrantFixture(t, agentadmin.MCPAuthInput{Mode: "none"}, map[string]*agentadmin.AccountInput{"mail": {Label: "work"}})
	ctx := context.Background()
	if err := f.c.repos.AgentGrants.SetIntegrationIdentity(ctx, "hermes--comms", "mail", "alice@owned.test"); err != nil {
		t.Fatal(err)
	}
	project := f.c.Registry.GetProject("hermes--comms")
	project.MCP.Servers = []registry.MCPServerConfig{{Name: "mail", Account: "work"}}
	f.c.publishDaemonMCPServers([]config.MCPServerConfig{{Name: "mail", Transport: "streamable-http", URL: "https://mail.test/mcp", Auth: mcpauth.Auth{Mode: "oauth"}}})
	got, ok := f.c.writeAccountOf(ctx, "hermes--comms", "mcp__mail-write__send")
	if !ok {
		t.Fatal("confirmed label lost")
	}
	if got.identity != "" || got.source != "" {
		t.Fatal("owned-row provider identity masquerades as inherited credential identity")
	}
}

func TestAgentAccount_DaemonOAuthSuspendsHiddenAgentGrant(t *testing.T) {
	f := accountGrantFixture(t, agentadmin.MCPAuthInput{Mode: "none"}, map[string]*agentadmin.AccountInput{"mail": {Label: "work"}})
	ctx := context.Background()
	g := seedGrant(t, f, "mail")
	// Registry omission cannot hide an inherited or unrelated live grant.
	f.c.Registry = nil
	expiry := time.Now().Add(time.Hour)
	tok := &persistence.MCPOAuthToken{ProjectID: "", ServerName: "daemon-mail", AccessToken: "daemon-existing-representation", ExpiresAt: &expiry}
	if handled, err := f.c.publishOAuthAccount(ctx, tok, "", false); !handled || err != nil {
		t.Fatalf("daemon publication: %v/%v", handled, err)
	}
	row, err := f.c.repos.BrokerGrants.Get(ctx, g.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.SuspendedAt == nil {
		t.Fatal("stale registry hid a live agent grant")
	}
	stored, err := f.c.repos.MCPOAuthTokens.Get(ctx, "", "daemon-mail")
	if err != nil {
		t.Fatal(err)
	}
	if stored.AccessToken != tok.AccessToken {
		t.Fatal("daemon storage representation was changed")
	}
}

func TestAgentAccount_AdminCompleteAndDisconnectUseTransition(t *testing.T) {
	f, vendor := oauthWriteFixture(t)
	ctx := context.Background()
	vendor.idToken = idTokenFor(map[string]any{"email": "alice@corp.example", "email_verified": true})
	signInFromPhone(t, f, "device-first")
	approveToolsAndWorkflow(t, f)
	g := seedOAuthGrant(t, f)
	conn := f.c.mcpConnector()
	ref, _ := f.c.mcpServerRef("hermes--work", "jira")
	begin, err := conn.Begin(ctx, ref, "operator")
	if err != nil {
		t.Fatal(err)
	}
	vendor.idToken = idTokenFor(map[string]any{"email": "bob@corp.example", "email_verified": true})
	if _, err = conn.Complete(ctx, begin.State, "admin-code"); err != nil {
		t.Fatal(err)
	}
	if !suspended(t, f, g.ID) {
		t.Fatal("typed admin Complete bypassed standing-grant suspension")
	}
	row, err := f.c.repos.AgentGrants.GetIntegration(ctx, "hermes--work", "jira")
	if err != nil {
		t.Fatal(err)
	}
	if row.AccountIdentity != "bob@corp.example" {
		t.Fatal("admin token and display identity were not published together")
	}
	stored, err := f.c.repos.MCPOAuthTokens.Get(ctx, "hermes--work", "jira")
	if err != nil {
		t.Fatal(err)
	}
	opened, err := conn.Tokens.Get(ctx, "hermes--work", "jira")
	if err != nil {
		t.Fatal(err)
	}
	if stored.AccessToken == opened.AccessToken || stored.RefreshToken == opened.RefreshToken {
		t.Fatal("agent publication stored an unsealed token")
	}
	if err = conn.Disconnect(ctx, "hermes--work", "jira", "operator"); err != nil {
		t.Fatal(err)
	}
	row, err = f.c.repos.AgentGrants.GetIntegration(ctx, "hermes--work", "jira")
	if err != nil {
		t.Fatal(err)
	}
	if row.AccountIdentity != "" || row.IdentitySource != "" {
		t.Fatal("Disconnect left the old provider identity")
	}
	if _, err = conn.Tokens.Get(ctx, "hermes--work", "jira"); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("Disconnect kept its token: %v", err)
	}
	if _, err = f.c.accountDecisionEpoch(ctx, "hermes"); err != nil {
		t.Fatalf("successful Disconnect left busy: %v", err)
	}
}

func TestAgentAccount_ConfigSuspendsBeforeApplyWindow(t *testing.T) {
	f := accountGrantFixture(t, agentadmin.MCPAuthInput{Mode: "none"}, map[string]*agentadmin.AccountInput{"mail": {Label: "work"}})
	ctx := context.Background()
	g := seedGrant(t, f, "mail")
	_, publication, err := f.c.beginConfigAccountTransition(configSnapshotForTest(ctx, t, f.c, "hermes"), "hermes", []agentadmin.IntegrationRef{{Project: "hermes--comms", Name: "mail"}})
	if err != nil {
		t.Fatal(err)
	}
	if !suspended(t, f, g.ID) {
		t.Fatal("configuration can publish before suspension")
	}
	if _, err = f.c.accountDecisionEpoch(ctx, "hermes"); !errors.Is(err, persistence.ErrAccountTransition) {
		t.Fatalf("apply window is not busy: %v", err)
	}
	// A daemon change during external apply makes completion fail closed.
	daemon, err := f.c.beginAccountTransition(ctx, persistence.AccountDaemonScope, "test-daemon-window")
	if err != nil {
		t.Fatal(err)
	}
	if err = f.c.repos.AccountTransitions.Finish(ctx, daemon, persistence.AccountPublication{}); err != nil {
		t.Fatal(err)
	}
	if err = f.c.repos.AccountTransitions.Finish(ctx, publication.ticket, publication.rows); !errors.Is(err, persistence.ErrAccountTransition) {
		t.Fatalf("stale daemon completion accepted: %v", err)
	}
	if _, err = f.c.accountDecisionEpoch(ctx, "hermes"); !errors.Is(err, persistence.ErrAccountTransition) {
		t.Fatal("failed config completion cleared busy")
	}
}

func TestAgentAccount_ConfigSuspensionFailureKeepsBusy(t *testing.T) {
	f := accountGrantFixture(t, agentadmin.MCPAuthInput{Mode: "none"}, map[string]*agentadmin.AccountInput{"mail": {Label: "work"}})
	ctx := context.Background()
	seedGrant(t, f, "mail")
	f.c.repos.BrokerGrants = &staticGrantList{BrokerGrantRepository: f.c.repos.BrokerGrants, fail: errors.New("enumeration failed")}
	_, _, err := f.c.beginConfigAccountTransition(configSnapshotForTest(ctx, t, f.c, "hermes"), "hermes", []agentadmin.IntegrationRef{{Project: "hermes--comms", Name: "mail"}})
	if err == nil {
		t.Fatal("failed suspension allowed external apply")
	}
	if _, err = f.c.accountDecisionEpoch(ctx, "hermes"); !errors.Is(err, persistence.ErrAccountTransition) {
		t.Fatal("enumeration failure cleared config fence")
	}
}

func TestAgentAccount_ConfigPostRenameFailureDrainedRecovery(t *testing.T) {
	f := accountGrantFixture(t, agentadmin.MCPAuthInput{Mode: "none"}, map[string]*agentadmin.AccountInput{"mail": {Label: "work"}})
	ctx := context.Background()
	g := seedGrant(t, f, "mail")
	path := filepath.Join(f.cfgDir, "configs", "projects", "hermes--comms.yaml")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	originalReload := f.svc.engine.Reload
	worker, cancel := context.WithCancel(ctx)
	defer cancel()
	renameObserved := make(chan struct{})
	var observed bool
	var journalID string
	f.svc.engine.Reload = func() error {
		if observed {
			return originalReload()
		}
		observed = true
		after, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if string(after) == string(before) {
			return errors.New("fixture never crossed filesystem publication")
		}
		if err = f.c.DB.QueryRowContext(ctx, `SELECT id FROM config_apply_journal WHERE terminal_at IS NULL ORDER BY created_at DESC LIMIT 1`).Scan(&journalID); err != nil {
			return err
		}
		close(renameObserved)
		<-worker.Done()
		return errors.New("test reload failed after filesystem rename")
	}
	input, _ := json.Marshal(agentadmin.RemoveInput{Kind: "integration", Project: "comms", ID: "mail"})
	done := make(chan error, 1)
	go func() {
		res, err := f.svc.doVerb(ctx, f.key, agentadmin.VerbRemove, input)
		if err == nil && res.Effect != EffectRefused {
			err = errors.New("post-rename failure did not refuse")
		}
		done <- err
	}()
	select {
	case <-renameObserved:
	case <-time.After(5 * time.Second):
		cancel()
		<-done
		t.Fatal("actual apply did not reach rename/reload window")
	}
	// Cancellation stops the external reload writer; joining Do drains all
	// engine rollback/journal work BEFORE any manual epoch recovery.
	cancel()
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(before) {
		t.Fatalf("actual engine rollback did not restore config: %v", err)
	}
	journal, err := f.svc.engine.Journal.Get(ctx, journalID)
	if err != nil || journal.State != persistence.JournalStateReverted || journal.TerminalAt == nil {
		t.Fatalf("actual journal did not record completed rollback: %v", err)
	}
	if !suspended(t, f, g.ID) {
		t.Fatal("post-publication failure restored grant authority")
	}
	if _, err = f.c.accountDecisionEpoch(ctx, "hermes"); !errors.Is(err, persistence.ErrAccountTransition) {
		t.Fatal("failed filesystem publication cleared busy")
	}
	var ticket persistence.AccountTransitionTicket
	ticket.Scope = persistence.AccountNamespaceScope("hermes")
	if err = f.c.DB.QueryRowContext(ctx, `SELECT epoch,owner FROM agent_account_transitions WHERE scope=? AND busy=TRUE`, ticket.Scope).Scan(&ticket.Epoch, &ticket.Owner); err != nil {
		t.Fatal(err)
	}
	// The inspected file is restored, holder is canceled/joined and grants
	// remain suspended: controlled recovery advances, never rewinds, epoch.
	result, err := f.c.DB.ExecContext(ctx, `UPDATE agent_account_transitions SET epoch=epoch+1,busy=FALSE,owner='',operation='' WHERE scope=? AND epoch=? AND owner=? AND busy=TRUE`, ticket.Scope, ticket.Epoch, ticket.Owner)
	if err != nil {
		t.Fatal(err)
	}
	n, err := result.RowsAffected()
	if err != nil || n != 1 {
		t.Fatal("drained recovery CAS did not match one holder")
	}
	if err = f.c.repos.AccountTransitions.Finish(ctx, ticket, persistence.AccountPublication{Secret: faultRecoverySecret()}); !errors.Is(err, persistence.ErrAccountTransition) {
		t.Fatalf("late holder survived controlled recovery: %v", err)
	}
	if !suspended(t, f, g.ID) {
		t.Fatal("recovery reactivated suspended grant")
	}
	if _, err = f.c.repos.AgentSecrets.Get(ctx, "hermes", "late"); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatal("late holder published credential")
	}
}

func faultRecoverySecret() *persistence.AgentSecretRow {
	now := time.Now().UTC()
	return &persistence.AgentSecretRow{Namespace: "hermes", Name: "late", Kind: "secret", Ciphertext: []byte("sealed"), Nonce: []byte("nonce"), CreatedAt: now, UpdatedAt: now}
}

func configSnapshotForTest(ctx context.Context, t *testing.T, c *Container, ns string) context.Context {
	t.Helper()
	vector, err := c.accountDecisionEpoch(ctx, ns)
	if err != nil {
		t.Fatal(err)
	}
	return context.WithValue(ctx, configAccountSnapshotKey{}, configAccountSnapshot{vector: vector})
}
