package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/persistence"
)

// GitHub #569: the service hands the runtime the effective hardening, and
// answers the orphan sweep's task lookup from the task store with the keep-on-
// doubt rules of 08-runtime-manager-podman.md, "Change 2026-10-28" R4.

type taskGetter569 map[string]*persistence.Task

func (g taskGetter569) Get(_ context.Context, id string) (*persistence.Task, error) {
	if id == "boom" {
		return nil, errors.New("connection refused")
	}
	if t, ok := g[id]; ok {
		return t, nil
	}
	return nil, persistence.ErrNotFound
}

func TestOrphanTaskLookup569(t *testing.T) {
	at := time.Date(2026, 10, 28, 11, 0, 0, 0, time.UTC)
	g := taskGetter569{
		"done":   {Status: persistence.TaskStatusCompleted, UpdatedAt: at},
		"closed": {Status: persistence.TaskStatusClosed, UpdatedAt: at},
		"run":    {Status: persistence.TaskStatusRunning},
		"wait":   {Status: persistence.TaskStatusAwaitingInput},
		"paused": {Status: persistence.TaskStatusPaused},
	}
	lookup := orphanTaskLookup(g)
	ctx := context.Background()
	for id, terminal := range map[string]bool{"done": true, "closed": true, "run": false, "wait": false, "paused": false} {
		got, err := lookup(ctx, id)
		if err != nil || !got.Found || got.Terminal != terminal {
			t.Errorf("%s: %+v %v, want found, terminal %v", id, got, err, terminal)
		}
	}
	if got, err := lookup(ctx, "other-daemon"); err != nil || got.Found {
		t.Errorf("unknown task: %+v %v, want not found and no error", got, err)
	}
	if _, err := lookup(ctx, "boom"); err == nil {
		t.Error("a failing store read was not an error (the sweep would not keep)")
	}
}

func TestAgentHardeningOptions569(t *testing.T) {
	cfg := config.DefaultConfig()
	c := &Container{Config: cfg, Logger: zerolog.Nop()}
	if n := len(c.agentHardeningOptions()); n != 2 {
		t.Fatalf("%d options, want 2", n)
	}
	cfg.Hosting.Mode = config.HostingModeManaged
	cfg.Runtime.AgentPidsLimit = 4096
	if ro, pids := cfg.Runtime.AgentHardening(cfg.Hosting); !ro || pids != config.ManagedAgentPidsLimit {
		t.Errorf("managed: %v %d", ro, pids)
	}
}
