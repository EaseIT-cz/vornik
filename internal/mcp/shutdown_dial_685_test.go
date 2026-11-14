package mcp

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/rs/zerolog"
)

// GitHub #685 (MCP failed-connect recovery design, amendment 2026-11-12): a
// stopping daemon logged "mcp: failed to connect" at ERROR for each stdio
// server whose dial systemd's SIGTERM ended. Once BeginShutdown is called, a
// failed dial logs at DEBUG and records nothing to retry.
//
// Found at: production (journal, 30 lines over 10 restarts by 2026-11-08).
// Regression test at: unit TestShutdownDial685_*.
// Lower because: the production interleaving needs systemd killing the unit's
// cgroup mid-dial; the connect seam makes the same failure deterministic.

func failingDials685(t *testing.T) {
	t.Helper()
	orig := connectFn
	t.Cleanup(func() { connectFn = orig })
	connectFn = func(context.Context, ServerConfig, zerolog.Logger) (*Client, error) {
		return nil, errors.New("mcp server pagedrop closed stdout before responding")
	}
}

func managerWithLog685() (*Manager, *bytes.Buffer) {
	var buf bytes.Buffer
	return NewManager(zerolog.New(&buf).Level(zerolog.DebugLevel)), &buf
}

func errorLines685(log string) int {
	n := 0
	for _, l := range strings.Split(log, "\n") {
		if strings.Contains(l, `"level":"error"`) && strings.Contains(l, "mcp: failed to connect") {
			n++
		}
	}
	return n
}

func TestShutdownDial685_StartForProject(t *testing.T) {
	failingDials685(t)
	cfg := []ServerConfig{{Name: "pagedrop", Transport: "stdio"}}

	m, buf := managerWithLog685()
	m.StartForProject(context.Background(), "assistant", cfg)
	if errorLines685(buf.String()) != 1 {
		t.Fatalf("before shutdown a failed dial must log ERROR once:\n%s", buf.String())
	}
	if _, pending := m.PendingStatus(); len(pending) != 1 {
		t.Fatalf("before shutdown the failed server is pending: %+v", pending)
	}

	m, buf = managerWithLog685()
	m.BeginShutdown()
	m.StartForProject(context.Background(), "assistant", cfg)
	if n := errorLines685(buf.String()); n != 0 {
		t.Fatalf("after BeginShutdown: %d ERROR lines:\n%s", n, buf.String())
	}
	if !strings.Contains(buf.String(), "mcp: dial ended by daemon shutdown") {
		t.Fatalf("the shutdown case is not logged at DEBUG:\n%s", buf.String())
	}
	if _, pending := m.PendingStatus(); len(pending) != 0 {
		t.Fatalf("after BeginShutdown nothing is recorded to retry: %+v", pending)
	}
}

func TestShutdownDial685_SyncProjects(t *testing.T) {
	failingDials685(t)
	desired := map[string][]ServerConfig{"assistant": {{Name: "pagedrop", Transport: "stdio"}}}

	m, buf := managerWithLog685()
	m.SyncProjects(context.Background(), desired)
	if errorLines685(buf.String()) != 1 {
		t.Fatalf("before shutdown a failed dial must log ERROR once:\n%s", buf.String())
	}

	m, buf = managerWithLog685()
	m.BeginShutdown()
	m.SyncProjects(context.Background(), desired)
	if n := errorLines685(buf.String()); n != 0 {
		t.Fatalf("after BeginShutdown: %d ERROR lines:\n%s", n, buf.String())
	}
	if !strings.Contains(buf.String(), "mcp: dial ended by daemon shutdown") {
		t.Fatalf("the shutdown case is not logged at DEBUG:\n%s", buf.String())
	}
}
