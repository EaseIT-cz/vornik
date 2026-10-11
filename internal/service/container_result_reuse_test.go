package service

import (
	"context"
	"testing"
)

// GitHub #119 item 9 (2026-10-08): delegate's max_age_seconds is pinned to
// the reach hash a standing grant pins (broker design §6b.4 check 4); the
// container wires standingReachHash into the api server. When the hash cannot
// be computed, reuse must miss: the function has to answer an error, never an
// empty hash that two runs would share.
func TestStandingReachHash_UnknownReachIsAnError(t *testing.T) {
	c := newBrokerActionContainer()
	ctx := context.Background()
	if h, err := c.standingReachHash(ctx, "ops-project", "wf"); err == nil || h != "" {
		t.Fatalf("operator workflow not loaded: got %q, %v; want an error", h, err)
	}
	if h, err := c.standingReachHash(ctx, "hermes--mail", "hermes--mail--digest"); err == nil || h != "" {
		t.Fatalf("agent project without the approval tables: got %q, %v; want an error", h, err)
	}
	c.Registry = nil
	if _, err := c.standingReachHash(ctx, "ops-project", "wf"); err == nil {
		t.Fatal("no registry: want an error")
	}
}
