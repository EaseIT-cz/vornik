package service

import (
	"context"
	"errors"
	"testing"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/repotest"
	"vornik.io/vornik/internal/registry"
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

// reachGrants answers one device-approved reach hash for every workflow.
type reachGrants struct {
	persistence.AgentGrantRepository
	reach string
}

func (g reachGrants) GetWorkflowReach(_ context.Context, workflowID string) (*persistence.AgentWorkflowApproval, error) {
	if g.reach == "" {
		return nil, persistence.ErrNotFound
	}
	return &persistence.AgentWorkflowApproval{WorkflowID: workflowID, ReachHash: g.reach}, nil
}

// The test double keeps the repository's miss contract.
func TestReachGrants_MissContract(t *testing.T) {
	repotest.AssertMiss(t, "AgentGrantRepository.GetWorkflowReach", func() (*persistence.AgentWorkflowApproval, error) {
		return reachGrants{}.GetWorkflowReach(context.Background(), "absent")
	})
}

// fixedEpochs answers a settable account generation vector.
type fixedEpochs struct {
	persistence.AccountTransitionRepository
	guard persistence.AccountDecisionGuard
	err   error
	asked []string
}

func (f *fixedEpochs) ReadDecisionEpoch(_ context.Context, ns string) (persistence.AccountDecisionGuard, error) {
	f.asked = append(f.asked, ns)
	return f.guard, f.err
}

func agentReuseContainer(t *testing.T) (*Container, *fixedEpochs) {
	t.Helper()
	c := newBrokerActionContainer()
	c.repos.AgentGrants = reachGrants{reach: "device-approved"}
	epochs := &fixedEpochs{guard: persistence.AccountDecisionGuard{
		{Scope: persistence.AccountDaemonScope, Epoch: 3},
		{Scope: persistence.AccountNamespaceScope("hermes"), Epoch: 7},
	}}
	c.repos.AccountTransitions = epochs
	return c, epochs
}

// audit 2026-10-12, finding B: the reuse reach omitted the account generation
// commit f9751d3cd added, so a credential re-pointed to another principal under
// the same label and secret name was served from the reuse cache for up to
// 900 s. A changed generation must change the reach (check 4 then misses).
func TestResultReuseReachHash_AccountGenerationChangesTheHash(t *testing.T) {
	c, epochs := agentReuseContainer(t)
	ctx := context.Background()
	first, err := c.resultReuseReachHash(ctx, "hermes--mail", "hermes--mail--digest")
	if err != nil || first == "" {
		t.Fatalf("first: %q, %v", first, err)
	}
	if again, _ := c.resultReuseReachHash(ctx, "hermes--mail", "hermes--mail--digest"); again != first {
		t.Fatalf("same generation: %q != %q", again, first)
	}
	if len(epochs.asked) == 0 || epochs.asked[0] != "hermes" {
		t.Fatalf("generation read for %v, want the hermes namespace", epochs.asked)
	}
	standing, _ := c.standingReachHash(ctx, "hermes--mail", "hermes--mail--digest")
	if first == standing {
		t.Fatal("an agent namespace's reuse reach must bind the account generation, not the standing reach alone")
	}
	epochs.guard[1].Epoch = 8 // a credential republished in the namespace
	if ns, _ := c.resultReuseReachHash(ctx, "hermes--mail", "hermes--mail--digest"); ns == first {
		t.Fatal("namespace generation changed: the reach must change")
	}
	epochs.guard[1].Epoch = 7
	epochs.guard[0].Epoch = 4 // a daemon-owned credential republished
	if d, _ := c.resultReuseReachHash(ctx, "hermes--mail", "hermes--mail--digest"); d == first {
		t.Fatal("daemon generation changed: the reach must change")
	}
}

// audit 2026-10-12, finding B: a transition in flight (busy) or an unreadable
// generation is unknown, never an empty or a stale reach.
func TestResultReuseReachHash_BusyTransitionIsUnknown(t *testing.T) {
	c, epochs := agentReuseContainer(t)
	ctx := context.Background()
	epochs.err = persistence.ErrAccountTransition
	if h, err := c.resultReuseReachHash(ctx, "hermes--mail", "hermes--mail--digest"); err == nil || h != "" || !errors.Is(err, persistence.ErrAccountTransition) {
		t.Fatalf("busy: got %q, %v; want ErrAccountTransition", h, err)
	}
	epochs.err, epochs.guard = nil, nil
	if h, err := c.resultReuseReachHash(ctx, "hermes--mail", "hermes--mail--digest"); err == nil || h != "" {
		t.Fatalf("no generation: got %q, %v; want an error", h, err)
	}
	// A partial vector would hash a subset and drop a fence silently
	// (review-20261012-8996 F3).
	epochs.guard = persistence.AccountDecisionGuard{{Scope: persistence.AccountNamespaceScope("hermes"), Epoch: 7}}
	if h, err := c.resultReuseReachHash(ctx, "hermes--mail", "hermes--mail--digest"); err == nil || h != "" {
		t.Fatalf("partial generation: got %q, %v; want an error", h, err)
	}
	c.repos.AccountTransitions = nil
	if h, err := c.resultReuseReachHash(ctx, "hermes--mail", "hermes--mail--digest"); err == nil || h != "" {
		t.Fatalf("no transition table: got %q, %v; want an error", h, err)
	}
}

// audit 2026-10-12, finding B: an operator project has no account generation;
// its reuse reach stays the standing reach, and no generation is read.
func TestResultReuseReachHash_OperatorProjectIsTheStandingHash(t *testing.T) {
	c, epochs := agentReuseContainer(t)
	ctx := context.Background()
	registry.SeedSwarmsForTest(c.Registry, map[string]*registry.Swarm{"ops": {ID: "ops"}})
	registry.SeedForTest(c.Registry, map[string]*registry.Project{"ops-project": {ID: "ops-project", SwarmID: "ops"}})
	registry.SeedWorkflowsForTest(c.Registry, map[string]*registry.Workflow{"wf": {ID: "wf", Broker: &registry.WorkflowBroker{}}})
	standing, err := c.standingReachHash(ctx, "ops-project", "wf")
	if err != nil {
		t.Fatalf("standing: %v", err)
	}
	got, err := c.resultReuseReachHash(ctx, "ops-project", "wf")
	if err != nil || got != standing {
		t.Fatalf("operator: got %q, %v; want the standing reach %q", got, err, standing)
	}
	if len(epochs.asked) != 0 {
		t.Fatalf("an operator project read account generations: %v", epochs.asked)
	}
	if _, err := c.resultReuseReachHash(ctx, "ops-project", "missing"); err == nil {
		t.Fatal("an uncomputable standing reach must stay an error")
	}
}
