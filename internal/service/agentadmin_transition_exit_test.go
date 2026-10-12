package service

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/agentadmin"
	"vornik.io/vornik/internal/approverdevice"
	"vornik.io/vornik/internal/persistence"
)

// Audit 2026-10-12 finding A (design §22.2.16, "Change 2026-10-12: every exit
// path leaves the transition row consistent"): a config apply refused before
// any write left ns:<namespace> busy forever, so every later connection
// change, Stage and grant decision in the namespace refused.

func notBusy(t *testing.T, f *agentAdminFixture, why string) {
	t.Helper()
	if _, err := f.c.accountDecisionEpoch(context.Background(), "hermes"); err != nil {
		t.Fatalf("%s: namespace left busy: %v", why, err)
	}
}

// audit 2026-10-12: finding A, inert path, ErrBusy refusal.
func TestAgentAccount_RefusedConfigApplyAbortsTransition(t *testing.T) {
	f := accountGrantFixture(t, agentadmin.MCPAuthInput{Mode: "none"}, map[string]*agentadmin.AccountInput{"mail": {Label: "work"}})
	busy := true
	f.svc.engine.HasActiveTasks = func(context.Context, string) (bool, error) { return busy, nil }
	input, _ := json.Marshal(agentadmin.RemoveInput{Kind: "integration", Project: "comms", ID: "mail"})
	res, err := f.svc.doVerb(context.Background(), f.key, agentadmin.VerbRemove, input)
	if err != nil || res.Effect != EffectRefused || !strings.Contains(res.Reason, "a task of this project is running") {
		t.Fatalf("fixture did not refuse on a running task: %+v %v", res, err)
	}
	notBusy(t, f, "pre-write ErrBusy refusal")
	busy = false
	res, err = f.svc.doVerb(context.Background(), f.key, agentadmin.VerbRemove, input)
	if err != nil || res.Effect != EffectApplied {
		t.Fatalf("the namespace did not accept a new change after the refusal: %+v %v", res, err)
	}
	notBusy(t, f, "successful retry")
}

// audit 2026-10-12: finding A, widening path, the transient ErrBusy retry.
func TestAgentAccount_RefusedApprovedApplyRetries(t *testing.T) {
	f := accountGrantFixture(t, agentadmin.MCPAuthInput{Mode: "none"}, map[string]*agentadmin.AccountInput{"mail": {Label: "work"}})
	ctx := context.Background()
	res := f.do(agentadmin.VerbAddMCPServer, agentadmin.AddMCPServerInput{Project: "comms", Name: "extra", URL: "https://extra.example/mcp", Auth: agentadmin.MCPAuthInput{Mode: "static", Credential: "EXTRA"}})
	if res.Effect != EffectAwaiting {
		t.Fatalf("fixture did not file a widening change: %+v", res)
	}
	busy := true
	f.svc.engine.HasActiveTasks = func(context.Context, string) (bool, error) { return busy, nil }
	req, err := f.c.repos.ApproverDevices.GetRequest(ctx, filepath.Base(res.ApprovalURL))
	if err != nil {
		t.Fatal(err)
	}
	_ = f.c.approverDeviceService().Decide(ctx, f.device, req.ID, req.RenderedSHA256, true)
	notBusy(t, f, "transient ErrBusy on an approved change")
	busy = false
	row, err := f.c.repos.ApproverDevices.GetRequest(ctx, req.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.svc.applyApproved(ctx, *row); err != nil {
		t.Fatalf("the approval tick's retry did not apply: %v", err)
	}
	if a, err := f.c.repos.AgentGrants.GetIntegration(ctx, "hermes--comms", "extra"); err != nil || a.RemovedAt != nil {
		t.Fatalf("the retried apply did not record the connection: %v", err)
	}
	notBusy(t, f, "retried apply")
}

type failReachOnce struct {
	persistence.AgentGrantRepository
	failed bool
}

func (r *failReachOnce) UpsertWorkflowReach(ctx context.Context, a persistence.AgentWorkflowApproval) error {
	if !r.failed {
		r.failed = true
		return errors.New("reach write failed")
	}
	return r.AgentGrantRepository.UpsertWorkflowReach(ctx, a)
}

// audit 2026-10-12: finding A (review e5cd F1). A non-account write that
// fails after Finish returns with busy clear; the retry finds the proposal
// APPLIED, does not apply files again, begins a fresh transition and
// finishes.
func TestAgentAccount_PostFinishWriteFailureRetries(t *testing.T) {
	f := accountGrantFixture(t, agentadmin.MCPAuthInput{Mode: "none"}, map[string]*agentadmin.AccountInput{"mail": {Label: "work"}})
	ctx := context.Background()
	res := f.do(agentadmin.VerbAddMCPServer, agentadmin.AddMCPServerInput{Project: "comms", Name: "extra", URL: "https://extra.example/mcp", Auth: agentadmin.MCPAuthInput{Mode: "static", Credential: "EXTRA"}})
	row, err := f.c.repos.ApproverDevices.GetRequest(ctx, filepath.Base(res.ApprovalURL))
	if err != nil {
		t.Fatal(err)
	}
	var pl map[string]any
	if err = json.Unmarshal(row.Rendered, &pl); err != nil {
		t.Fatal(err)
	}
	pl["grant"].(map[string]any)["workflows"] = map[string]any{"hermes--comms--reply-mail": "reach-hash"}
	if row.Rendered, err = json.Marshal(pl); err != nil {
		t.Fatal(err)
	}
	row.DecidedByDevice = f.device.ID
	f.svc.grants = &failReachOnce{AgentGrantRepository: f.svc.grants}
	if err = f.svc.applyApproved(ctx, *row); err == nil {
		t.Fatal("fixture did not fail the post-Finish write")
	}
	notBusy(t, f, "post-Finish write failure")
	if a, err := f.c.repos.AgentGrants.GetIntegration(ctx, "hermes--comms", "extra"); err != nil || a.RemovedAt != nil {
		t.Fatalf("Finish did not record the connection: %v", err)
	}
	applies := 0
	reload := f.svc.engine.Reload
	f.svc.engine.Reload = func() error { applies++; return reload() }
	if err = f.svc.applyApproved(ctx, *row); err != nil {
		t.Fatalf("the retry did not finish: %v", err)
	}
	if applies != 0 {
		t.Fatalf("the retry applied files again (%d reloads)", applies)
	}
	notBusy(t, f, "retry after a post-Finish failure")
}

// audit 2026-10-12: finding A, modelRouteMoved returns before any apply.
func TestAgentAccount_ModelRouteMovedAbortsTransition(t *testing.T) {
	f := accountGrantFixture(t, agentadmin.MCPAuthInput{Mode: "none"}, map[string]*agentadmin.AccountInput{"mail": {Label: "work"}})
	ctx := context.Background()
	res := f.do(agentadmin.VerbAddMCPServer, agentadmin.AddMCPServerInput{Project: "comms", Name: "extra", URL: "https://extra.example/mcp", Auth: agentadmin.MCPAuthInput{Mode: "static", Credential: "EXTRA"}})
	row, err := f.c.repos.ApproverDevices.GetRequest(ctx, filepath.Base(res.ApprovalURL))
	if err != nil {
		t.Fatal(err)
	}
	var pl map[string]any
	if err = json.Unmarshal(row.Rendered, &pl); err != nil {
		t.Fatal(err)
	}
	pl["grant"].(map[string]any)["models"] = []any{map[string]any{"destination": "remote:nowhere", "model": "no-such-model", "role": "worker"}}
	if row.Rendered, err = json.Marshal(pl); err != nil {
		t.Fatal(err)
	}
	row.DecidedByDevice = f.device.ID
	if err = f.svc.applyApproved(ctx, *row); !errors.Is(err, approverdevice.ErrPermanent) || !strings.Contains(err.Error(), "route changed") {
		t.Fatalf("fixture did not take the model-route-moved exit: %v", err)
	}
	notBusy(t, f, "model route moved")
}

// audit 2026-10-12: finding A, OAuth publication whose pre-suspension read
// fails (no integration row) published nothing and must not stay busy.
func TestAgentAccount_OAuthPreSuspensionFailureAborts(t *testing.T) {
	f := accountGrantFixture(t, agentadmin.MCPAuthInput{Mode: "none"}, map[string]*agentadmin.AccountInput{"mail": {Label: "work"}})
	ctx := context.Background()
	expiry := time.Now().Add(time.Hour)
	tok := &persistence.MCPOAuthToken{ProjectID: "hermes--comms", ServerName: "nosuch", AccessToken: "candidate", ExpiresAt: &expiry}
	if handled, err := f.c.publishOAuthAccount(ctx, tok, "", false); !handled || err == nil {
		t.Fatalf("a connect without an integration row published: %v/%v", handled, err)
	}
	if _, err := f.c.repos.MCPOAuthTokens.Get(ctx, "hermes--comms", "nosuch"); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatal("the refused connect stored a token")
	}
	notBusy(t, f, "OAuth pre-suspension failure")
}

// audit 2026-10-12: finding B, a Disconnect of a removed agent server's
// leftover token rolled back in Finish (SetIntegrationIdentity matches live
// rows only), kept the token and left the namespace busy.
func TestAgentAccount_DisconnectRemovedIntegrationDeletesAndClears(t *testing.T) {
	f, _ := oauthWriteFixture(t)
	ctx := context.Background()
	signInFromPhone(t, f, "before-removal")
	if err := f.c.repos.AgentGrants.MarkIntegrationRemoved(ctx, "hermes--work", "jira", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := f.c.mcpConnector().Disconnect(ctx, "hermes--work", "jira", "operator"); err != nil {
		t.Fatalf("Disconnect of a removed integration failed: %v", err)
	}
	if _, err := f.c.repos.MCPOAuthTokens.Get(ctx, "hermes--work", "jira"); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("Disconnect kept the token: %v", err)
	}
	if _, err := f.c.accountDecisionEpoch(ctx, "hermes"); err != nil {
		t.Fatalf("Disconnect left the namespace busy: %v", err)
	}
	// A Connect to the removed connection is refused and Aborted: nothing
	// is published and the namespace stays usable (review f2c1 B-2).
	expiry := time.Now().Add(time.Hour)
	tok := &persistence.MCPOAuthToken{ProjectID: "hermes--work", ServerName: "jira", AccessToken: "after-removal", ExpiresAt: &expiry}
	if handled, err := f.c.publishOAuthAccount(ctx, tok, "", false); !handled || err == nil {
		t.Fatalf("a Connect to a removed connection published: %v/%v", handled, err)
	}
	if _, err := f.c.repos.MCPOAuthTokens.Get(ctx, "hermes--work", "jira"); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("the refused Connect stored a token: %v", err)
	}
	if _, err := f.c.accountDecisionEpoch(ctx, "hermes"); err != nil {
		t.Fatalf("the refused Connect left the namespace busy: %v", err)
	}
}
