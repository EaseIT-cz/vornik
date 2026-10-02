package service

import (
	"context"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/mcp"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/repotest"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/storage"
)

type nopBrokerActions struct {
	persistence.BrokerActionRepository
}

func newBrokerActionContainer() *Container {
	c := &Container{Logger: zerolog.Nop(), Config: &config.Config{}, Registry: registry.New()}
	c.repos = &storage.Repositories{BrokerActions: nopBrokerActions{}}
	c.mcpManager = mcp.NewManager(zerolog.Nop())
	return c
}

// Broker write-actions design §5.4: a node without the store or without an
// MCP manager runs no worker; the /inbox kick is then a no-op and another
// node's sweep executes the action.
func TestNewBrokerActionWorker_NeedsStoreAndMCP(t *testing.T) {
	c := newBrokerActionContainer()
	if c.newBrokerActionWorker() == nil {
		t.Fatal("store and MCP manager present: want a worker")
	}
	c.mcpManager = nil
	if w := c.newBrokerActionWorker(); w != nil {
		t.Fatal("no MCP manager: want no worker")
	}
	c = newBrokerActionContainer()
	c.repos.BrokerActions = nil
	if w := c.newBrokerActionWorker(); w != nil {
		t.Fatal("no store: want no worker")
	}
	// The kick closure the UI holds must tolerate the nil worker.
	c.brokerActionWorker.Kick("ba_1")
}

func TestBrokerActionWorkerConfig_ReadsTheGatesAtCallTime(t *testing.T) {
	c := newBrokerActionContainer()
	cfg := c.brokerActionWorkerConfig()
	if cfg.WritesOn() {
		t.Fatal("broker.writes defaults off")
	}
	c.Config.Broker.Writes = "on"
	if !cfg.WritesOn() {
		t.Fatal("WritesOn must read the config at call time")
	}
	if err := cfg.ToolDeclared("gone", "mcp__mail__send"); err == nil || !strings.Contains(err.Error(), "no longer loaded") {
		t.Fatalf("unloaded project: %v", err)
	}
	c.Registry = nil
	if err := cfg.ToolDeclared("p", "mcp__mail__send"); err == nil {
		t.Fatal("no registry: want a refusal")
	}
	if cfg.Timeout <= 0 {
		t.Fatalf("Timeout = %v", cfg.Timeout)
	}
	// Secrets off: each stored response is counted unscanned, with its reason.
	if cfg.Redact != nil || cfg.OnUnscanned == nil || cfg.OnFinishError == nil {
		t.Fatal("unscanned and finish-failure hooks must be wired")
	}
	cfg.OnUnscanned()
	cfg.OnFinishError(nil, "executed")
}

func TestBrokerActionRedactor(t *testing.T) {
	c := newBrokerActionContainer()
	if r, reason := c.brokerActionRedactor(); r != nil || reason != "secrets_disabled" {
		t.Fatalf("secrets disabled: want no redactor and reason secrets_disabled, got %q", reason)
	}
	c.Config.Secrets.Enabled = true
	redact, reason := c.brokerActionRedactor()
	if redact == nil || reason != "" {
		t.Fatalf("secrets enabled: want a redactor, reason %q", reason)
	}
	secret := "ghp_" + strings.Repeat("a1B2c3D4e5", 4)[:36]
	out := string(redact([]byte(`{"sent":true,"token":"` + secret + `"}`)))
	if strings.Contains(out, secret) {
		t.Fatalf("token survived redaction: %s", out)
	}
	if clean := string(redact([]byte(`{"sent":true}`))); clean != `{"sent":true}` {
		t.Fatalf("clean response altered: %s", clean)
	}
}

// A stuck executing row must be one whose call cannot still be in flight:
// the stuck threshold has to exceed the longest broker.action_timeout the
// loader accepts (review-20260930-1334 F1).
func TestBrokerActionStuckThresholdExceedsTheLongestActionTimeout(t *testing.T) {
	longest, err := config.BrokerDaemonConfig{ActionTimeout: "10m"}.EffectiveActionTimeout()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (config.BrokerDaemonConfig{ActionTimeout: "10m1s"}).EffectiveActionTimeout(); err == nil {
		t.Fatal("the loader's ceiling moved; re-check the stuck threshold against it")
	}
	if persistence.BrokerActionStuckAfter <= longest {
		t.Fatalf("stuck threshold %v must exceed the longest action timeout %v", persistence.BrokerActionStuckAfter, longest)
	}
}

// No Telegram bot: the notify hook is a no-op, never a panic.
func TestNotifyBrokerActionsPending_NoBotIsANoOp(_ *testing.T) {
	c := newBrokerActionContainer()
	c.notifyBrokerActionsPending(context.Background(), "p", "t", 1)
}

func TestBrokerInboxURL(t *testing.T) {
	for in, want := range map[string]string{
		"":                             "/ui/inbox",
		"https://vornik.example/":      "https://vornik.example/ui/inbox",
		"\thttps://vornik.example:8\n": "https://vornik.example:8/ui/inbox",
	} {
		if got := brokerInboxURL(in); got != want {
			t.Errorf("brokerInboxURL(%q) = %q, want %q", in, got, want)
		}
	}
}

type nopPushOutbox struct {
	persistence.CompanionPushOutbox
}
type nopPushConfigs struct {
	persistence.A2APushConfigRepository
}

// Design §7a: the pusher runs, and the api advertises companion-push, under
// one condition — the outbox and the push-config store are both wired.
func TestNewCompanionPusher_OneCondition(t *testing.T) {
	c := newBrokerActionContainer()
	if c.companionPushWired() || c.newCompanionPusher() != nil {
		t.Fatal("no outbox: no pusher")
	}
	c.repos.CompanionPushOutbox = nopPushOutbox{}
	if c.companionPushWired() {
		t.Fatal("no push-config store: not wired")
	}
	c.repos.A2APushConfigs = nopPushConfigs{}
	if !c.companionPushWired() || c.newCompanionPusher() == nil {
		t.Fatal("outbox and store: want a pusher")
	}
	// The hooks that kick it tolerate its absence.
	c.companionPusher = nil
	c.notifyBrokerActionsPending(context.Background(), "p", "t", 1)
	c.brokerActionWorkerConfig().OnChange()
}

// grantsFor answers GetIntegration from a fixed row set.
type grantsFor struct {
	persistence.AgentGrantRepository
	rows map[string]*persistence.AgentIntegrationApproval
}

func (g grantsFor) GetIntegration(_ context.Context, projectID, integration string) (*persistence.AgentIntegrationApproval, error) {
	if r, ok := g.rows[projectID+"/"+integration]; ok {
		return r, nil
	}
	return nil, persistence.ErrNotFound
}

// Review 20261002-a048 F1 (agent-administered Vornik design §7.3): the broker
// write worker is a tool route that does not pass CallMCPTool. For an agent
// project, a declared broker_write tool is not enough: the tool must be in
// the integration's DEVICE-approved write set. Control: the agent branch of
// ToolDeclared. Without it, a hand-edited project file declaring a
// broker_write server would send an approved action through a server no
// person approved for writes.
func TestBrokerActionWorkerConfig_AgentProjectNeedsApprovedWriteSet(t *testing.T) {
	c := newBrokerActionContainer()
	registry.SeedForTest(c.Registry, map[string]*registry.Project{"hermes--fin": {
		ID: "hermes--fin", Broker: true,
		MCP: registry.ProjectMCP{Servers: []registry.MCPServerConfig{{
			Name: "mail-write", URL: "https://mail.example/mcp", BrokerWrite: true, AllowedTools: []string{"send"},
		}}},
	}})
	cfg := c.brokerActionWorkerConfig()
	if err := cfg.ToolDeclared("hermes--fin", "mcp__mail-write__send"); err == nil {
		t.Fatal("no approval tables wired: an agent project's write must be refused")
	}
	rows := map[string]*persistence.AgentIntegrationApproval{}
	c.repos.AgentGrants = grantsFor{rows: rows}
	if err := cfg.ToolDeclared("hermes--fin", "mcp__mail-write__send"); err == nil || !strings.Contains(err.Error(), "not approved") {
		t.Fatalf("no approval row: %v", err)
	}
	rows["hermes--fin/mail"] = &persistence.AgentIntegrationApproval{ProjectID: "hermes--fin", Integration: "mail", ReadTools: []string{"send"}}
	if err := cfg.ToolDeclared("hermes--fin", "mcp__mail-write__send"); err == nil {
		t.Fatal("a tool approved for READ only was accepted as a write")
	}
	rows["hermes--fin/mail"].WriteTools = []string{"send"}
	if err := cfg.ToolDeclared("hermes--fin", "mcp__mail-write__send"); err != nil {
		t.Fatalf("an approved write was refused: %v", err)
	}
}

// The test double keeps the repository's miss contract.
func TestGrantsFor_MissContract(t *testing.T) {
	repotest.AssertMiss(t, "AgentGrantRepository.GetIntegration", func() (*persistence.AgentIntegrationApproval, error) {
		return grantsFor{}.GetIntegration(context.Background(), "p", "absent")
	})
}
