package repotest

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// grantSuspended pins the sticky read-tool suspension of
// agent_integration_approvals on both drivers. GitHub #481 (2026-10-21):
// suspension not sticky; no re-list between reloads (agent-administered
// design §6.5, amendment #481). SuspendIntegrationTools only adds, stamps
// suspended_at once and refuses a removed row; every upsert of a live row
// (a re-grant, a narrowing) keeps the set and ignores the argument's; only
// a re-add of a removed row (which the person approves on the device)
// clears it; ListSuspendedIntegrations returns live rows only.
func grantSuspended(t *testing.T, h *grantHarness) {
	get := func() *persistence.AgentIntegrationApproval { return accountGet(t, h, "susp") }
	a := persistence.AgentIntegrationApproval{Namespace: h.ns, ProjectID: h.project, Integration: "susp", Kind: "mcp",
		URL: "https://susp.example/mcp", ReadTools: []string{"get", "list", "submit"}, ApprovedByDevice: "dev_a", ApprovedAt: h.now}
	accountUpsert(t, h, a)
	if got := get(); len(got.SuspendedTools) != 0 || got.SuspendedAt != nil {
		t.Fatalf("a new row starts suspended: %+v", got)
	}
	if err := h.repo.SuspendIntegrationTools(h.ctx, h.project, "susp", []string{"submit"}, h.now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := h.repo.SuspendIntegrationTools(h.ctx, h.project, "susp", []string{"list", "submit"}, h.now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	got := get()
	if !reflect.DeepEqual(got.SuspendedTools, []string{"list", "submit"}) || got.SuspendedAt == nil || !got.SuspendedAt.Equal(h.now.Add(time.Minute)) {
		t.Fatalf("suspend must union and stamp once: %+v", got)
	}
	if !suspendedListed(t, h, "susp", []string{"list", "submit"}) {
		t.Fatal("ListSuspendedIntegrations misses a live suspended row")
	}

	// A live re-grant and a narrowing keep the set, whatever they carry.
	regrant := a
	regrant.ReadTools, regrant.SuspendedTools, regrant.SuspendedAt = []string{"get", "list"}, nil, nil
	accountUpsert(t, h, regrant)
	if got := get(); !reflect.DeepEqual(got.SuspendedTools, []string{"list", "submit"}) || got.SuspendedAt == nil {
		t.Fatalf("an upsert of a live row cleared the suspension: %+v", got)
	}

	// A removed row: not listed, and not suspendable.
	if err := h.repo.MarkIntegrationRemoved(h.ctx, h.project, "susp", h.now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if suspendedListed(t, h, "susp", nil) {
		t.Fatal("ListSuspendedIntegrations returns a removed row")
	}
	if err := h.repo.SuspendIntegrationTools(h.ctx, h.project, "susp", []string{"get"}, h.now); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("suspend on a removed row: %v, want ErrNotFound", err)
	}
	if err := h.repo.SuspendIntegrationTools(h.ctx, h.project, "absent", []string{"get"}, h.now); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("suspend on no row: %v, want ErrNotFound", err)
	}

	// The re-add (approved on the device) starts clean.
	accountUpsert(t, h, a)
	if got := get(); got.RemovedAt != nil || len(got.SuspendedTools) != 0 || got.SuspendedAt != nil {
		t.Fatalf("a re-add must clear the suspension: %+v", got)
	}
	if suspendedListed(t, h, "susp", nil) {
		t.Fatal("ListSuspendedIntegrations returns a cleared row")
	}

	// A listing taken before the re-approval cannot hold what the person
	// just approved (design round 1 F2/F3): a re-list dial in flight
	// across the re-add. The guard rests on the re-add advancing approved_at
	// to the re-approval time (round 2 New-1): re-add again, approved two
	// hours on, and judge with a listing taken between the two approvals.
	if err := h.repo.MarkIntegrationRemoved(h.ctx, h.project, "susp", h.now.Add(90*time.Minute)); err != nil {
		t.Fatal(err)
	}
	readd := a
	readd.ApprovedAt = h.now.Add(2 * time.Hour)
	accountUpsert(t, h, readd)
	if got := get(); !got.ApprovedAt.Equal(readd.ApprovedAt) {
		t.Fatalf("a re-add did not advance approved_at: %v, want %v", got.ApprovedAt, readd.ApprovedAt)
	}
	if err := h.repo.SuspendIntegrationTools(h.ctx, h.project, "susp", []string{"submit"}, h.now.Add(time.Hour)); !errors.Is(err, persistence.ErrStaleListing) {
		t.Fatalf("a listing older than the approval suspended it: %v, want ErrStaleListing", err)
	}
	if got := get(); len(got.SuspendedTools) != 0 {
		t.Fatalf("a stale listing wrote the set: %+v", got)
	}
	// A listing taken at or after it does.
	if err := h.repo.SuspendIntegrationTools(h.ctx, h.project, "susp", []string{"submit"}, readd.ApprovedAt); err != nil {
		t.Fatal(err)
	}
	if got := get(); !reflect.DeepEqual(got.SuspendedTools, []string{"submit"}) {
		t.Fatalf("a listing at the approval time did not suspend: %+v", got)
	}
}

// suspendedListed reports whether ListSuspendedIntegrations holds this
// harness's row for integration, with want as its set when want is non-nil.
// It filters by project: the Postgres lane's database is shared.
func suspendedListed(t *testing.T, h *grantHarness, integration string, want []string) bool {
	t.Helper()
	rows, err := h.repo.ListSuspendedIntegrations(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.ProjectID != h.project || r.Integration != integration {
			continue
		}
		if want != nil && !reflect.DeepEqual(r.SuspendedTools, want) {
			t.Fatalf("listed set %v, want %v", r.SuspendedTools, want)
		}
		return true
	}
	return false
}
