package service

// GitHub #623 (found at: production, 2026-11-02): the Context7 MCP server,
// referencing secret://context7-mcp, was refused at registration because the
// value existed nowhere the daemon looked, and nothing in the console could
// supply it. These tests run the real resolution path (daemonMCPServerConfigs
// -> applyMCPAuth -> mcpauth.Resolve -> the container's secret source) over a
// real store created on first write: a daemon-level server absent for a
// missing credential is present, with its header, once the credential is
// saved, with no restart (round-2 review finding 3). Only the background
// network probe is replaced by a counter. Regression test at: integration.
// Design: 2026-10-02-agent-administered-vornik-design.md, Amendment
// 2026-11-02 (#623).

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"vornik.io/vornik/internal/persistence"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/mcpauth"
	"vornik.io/vornik/internal/persistence/sqlite"
	"vornik.io/vornik/internal/persistence/sqlite/sqlitetest"
	"vornik.io/vornik/internal/storage"
)

const credName623 = "CTX_TEST_623_KEY"

func newCredContainer623(t *testing.T) *Container {
	t.Helper()
	db := sqlitetest.Memory(t)
	return &Container{
		Logger:     zerolog.Nop(),
		ConfigPath: filepath.Join(t.TempDir(), "config.yaml"),
		repos:      &storage.Repositories{AgentSecrets: sqlite.NewAgentSecretRepository(db.DB)},
		Config: &config.Config{MCP: config.MCPConfig{Servers: []config.MCPServerConfig{{
			Name:      "Context7",
			Transport: "streamable-http",
			URL:       "https://mcp.context7.example/mcp",
			Auth: mcpauth.Auth{
				Mode:        mcpauth.ModeStatic,
				ValueFrom:   "secret://" + credName623,
				ValuePrefix: "Bearer ",
			},
		}}}},
	}
}

func TestOperatorCredentials623_SavingRegistersTheWaitingServer(t *testing.T) {
	c := newCredContainer623(t)
	if got := c.daemonMCPServerConfigs(); len(got) != 0 {
		t.Fatalf("before the credential exists the server is present: %+v", got)
	}
	daemon, projects := 0, 0
	oc := &operatorCredentials{c: c,
		refreshDaemon:   func() { daemon++ },
		refreshProjects: func(context.Context) error { projects++; return nil },
	}
	ctx := context.Background()

	view, err := oc.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Rows) != 1 || view.Rows[0].Name != credName623 || view.Rows[0].State() != "missing" ||
		len(view.Rows[0].NeededBy) != 1 || view.Rows[0].NeededBy[0] != "Context7" {
		t.Fatalf("status before = %+v, want %s missing, needed by Context7", view, credName623)
	}

	if err := oc.Put(ctx, credName623, []byte("ctx7-token"), persistence.AdminAuditEntry{Principal: "console:user_1", Source: "ui"}); err != nil {
		t.Fatalf("put: %v", err)
	}
	rep, err := oc.Refresh(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if daemon != 1 || projects != 1 {
		t.Errorf("refreshes: daemon %d, projects %d, want 1 and 1 (review F10)", daemon, projects)
	}
	if len(rep.DaemonPresent) != 1 || rep.DaemonPresent[0] != "Context7" {
		t.Errorf("present after save = %v, want [Context7]", rep.DaemonPresent)
	}
	servers := c.daemonMCPServerConfigs()
	if len(servers) != 1 || servers[0].AuthHeaders["Authorization"] != "Bearer ctx7-token" {
		t.Fatalf("after the save the server resolves to %+v", servers)
	}

	view, _ = oc.Status(ctx)
	if view.Rows[0].State() != "set" || view.Rows[0].StoreSetBy != "console:user_1" {
		t.Errorf("status after = %+v, want set by console:user_1", view.Rows[0])
	}

	// The environment wins (operator decision): an env value shadows it.
	t.Setenv(credName623, "env-token")
	if s := c.daemonMCPServerConfigs(); s[0].AuthHeaders["Authorization"] != "Bearer env-token" {
		t.Errorf("env did not win: %q", s[0].AuthHeaders["Authorization"])
	}
	view, _ = oc.Status(ctx)
	if view.Rows[0].State() != "the environment value is used" {
		t.Errorf("both present = %q", view.Rows[0].State())
	}

	if err := oc.Delete(ctx, credName623, persistence.AdminAuditEntry{Principal: "console:user_1", Source: "ui"}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	t.Setenv(credName623, "")
	if got := c.daemonMCPServerConfigs(); len(got) != 0 {
		t.Fatalf("after removal the server is still present: %+v", got)
	}
}

// Code review F4 and F6 (review-20261102-ce24): removing a name that was never
// stored is not an error, before or after the store exists; a save with an
// unusable store key fails and stores nothing.
func TestOperatorCredentials623_RemoveMissingAndUnusableKey(t *testing.T) {
	c := newCredContainer623(t)
	oc := &operatorCredentials{c: c}
	ctx := context.Background()
	if err := oc.Delete(ctx, "NEVER_STORED", persistence.AdminAuditEntry{Principal: "console:u", Source: "ui"}); err != nil {
		t.Fatalf("remove before any store exists: %v", err)
	}
	if err := oc.Put(ctx, "SOMETHING", []byte("v"), persistence.AdminAuditEntry{Principal: "console:u", Source: "ui"}); err != nil {
		t.Fatal(err)
	}
	if err := oc.Delete(ctx, "NEVER_STORED", persistence.AdminAuditEntry{Principal: "console:u", Source: "ui"}); err != nil {
		t.Fatalf("remove of a missing name: %v", err)
	}

	broken := newCredContainer623(t)
	broken.secretStoreLoaded, broken.secretStoreErr = true, errors.New("store key unusable")
	if err := (&operatorCredentials{c: broken}).Put(ctx, "X", []byte("v"), persistence.AdminAuditEntry{Principal: "console:u"}); err == nil {
		t.Fatal("a save with an unusable key succeeded")
	}
	view, err := (&operatorCredentials{c: broken}).Status(ctx)
	if err != nil || !view.KeyUnusable {
		t.Fatalf("status with an unusable key = (%+v, %v), want KeyUnusable", view, err)
	}
}
