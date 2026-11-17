package api

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"vornik.io/vornik/internal/config"
)

// GitHub #786 (enterprise-packaging design, amendment 2026-11-15 (#786)).
// Found at: production (vornik-cloud staging, 2026.11.5): a fresh managed
// instance's doctor reported three ERRORs. Behind the first, agents had no
// daemon access at all: provisioning blanks server.unix_socket while the
// shipped example runs every role daemon-only, and an empty
// runtime.agent_llm.endpoint sent agents to the model gateway, which refuses
// their per-task key. Regression tests at: integration, through the doctor's
// own wiring (SetServerConfig) and the real config accessors. Lower because:
// there is no managed-VM lane on this host.

// managedFresh786 is the managed config as provisioning leaves it: an empty
// socket, an empty agent endpoint, the example's daemon-only default.
func managedFresh786() *config.Config {
	c := config.DefaultConfig()
	c.Hosting.Mode = config.HostingModeManaged
	c.Server.Address = ":8080"
	c.Server.UnixSocket = ""
	c.Runtime.DefaultNetwork = "daemon-only"
	c.Runtime.AgentLLM.Endpoint = ""
	c.Chat.Endpoint = "https://gateway.internal/v1"
	return c
}

func TestDoctor786_ManagedFreshInstanceAgentsReachTheDaemon(t *testing.T) {
	h := NewDoctorHandlers(nil)
	h.SetServerConfig(managedFresh786())

	if got := h.checkAgentLLMTopology(); got.Status != "OK" {
		t.Fatalf("agent_llm_topology on a fresh managed instance: %s %q", got.Status, got.Message)
	}
	if got := agentDaemonReach(h.serverAddress, h.unixSocketPath, h.defaultNetwork, []string{"", ""}); got.Status != "OK" {
		t.Fatalf("agent_daemon_reach on a fresh managed instance: %s %q", got.Status, got.Message)
	}
	if h.unixSocketPath != config.ManagedUnixSocket {
		t.Fatalf("the doctor's socket is %q, want the managed default %q", h.unixSocketPath, config.ManagedUnixSocket)
	}
}

// The gap that hid it: daemon-only roles with no socket have no daemon, on
// any listen address.
func TestDoctor786_DaemonOnlyWithoutASocketIsAnError(t *testing.T) {
	for _, listen := range []string{":8080", "0.0.0.0:8080", "127.0.0.1:8080"} {
		got := agentDaemonReach(listen, "", "daemon-only", []string{"", "host"})
		if got.Status != "ERROR" || !strings.Contains(got.Message, "server.unix_socket") || !strings.Contains(got.Message, "1 role") {
			t.Errorf("%s: daemon-only roles without a socket: %s %q", listen, got.Status, got.Message)
		}
	}
	// A self-hosted install that sets a socket is unchanged.
	if got := agentDaemonReach(":8080", "/run/user/1000/vornik/vornik.sock", "daemon-only", []string{""}); got.Status != "OK" {
		t.Errorf("with a socket: %s %q", got.Status, got.Message)
	}
	// A self-hosted install has no managed default.
	c := managedFresh786()
	c.Hosting.Mode = ""
	if c.DaemonSocketPath() != "" {
		t.Errorf("a self-hosted install got a socket it did not configure: %q", c.DaemonSocketPath())
	}
}

// Every fresh instance: an empty window summed to NULL and failed the scan.
func TestDoctor786_UnclassifiedShareOnAnEmptyTableIsSkipped(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`CREATE TABLE execution_step_outcomes (
		model TEXT, outcome TEXT, error_class TEXT, error_detail TEXT, recorded_at TIMESTAMP)`); err != nil {
		t.Fatal(err)
	}
	h := NewDoctorHandlers(db)
	got := h.checkUnclassifiedShare(context.Background())
	if got.Status != "SKIPPED" {
		t.Fatalf("an empty window: %s %q, want SKIPPED", got.Status, got.Message)
	}
}
