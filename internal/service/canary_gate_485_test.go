package service

import (
	"testing"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/storage"
)

// GitHub #485 (cost-quality canary design §9.1, operator decision A11).
// Found at review (the #447 lane, journal design §14.3 O-447). The canary
// guard had its own lease, so on two daemons the guard could lead on one
// while the config writer was the other: its rollback was refused every tick
// and the regressed change stayed applied. The guard is now led by the
// daemon's one config-writer elector.

type noLeaderLocks struct {
	persistence.DaemonLeaderLockRepository
}

func TestCanaryGuardGate485_IsTheConfigWriter(t *testing.T) {
	c := &Container{repos: &storage.Repositories{LeaderLocks: noLeaderLocks{}}}
	gate := c.canaryGuardLeaderGate()
	writer := c.configWriter()
	if writer == nil {
		t.Fatal("setup: the container must build a config-writer elector")
	}
	if gate != writer {
		t.Fatalf("the canary guard's gate is %v, want the config-writer elector %p", gate, writer)
	}
	if g := (&Container{}).canaryGuardLeaderGate(); g != nil {
		t.Fatalf("with no elector the gate must be an untyped nil, got %#v", g)
	}
}
