package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/repotest"
)

// fakeCLIBrokerActionRepo records Resolve and serves ListByStatus. Any other
// method panics through the nil embedded interface: resolve must call nothing
// that could execute the action.
type fakeCLIBrokerActionRepo struct {
	persistence.BrokerActionRepository
	rows       []*persistence.BrokerAction
	resolved   []string
	gotNote    []byte
	resolveErr error
}

func (f *fakeCLIBrokerActionRepo) ListByStatus(_ context.Context, project, status string, _ int) ([]*persistence.BrokerAction, error) {
	var out []*persistence.BrokerAction
	for _, a := range f.rows {
		if a.Status == status && (project == "" || a.ProjectID == project) {
			out = append(out, a)
		}
	}
	return out, nil
}

func (f *fakeCLIBrokerActionRepo) Get(_ context.Context, id string) (*persistence.BrokerAction, error) {
	for _, a := range f.rows {
		if a.ActionID == id {
			cp := *a
			return &cp, nil
		}
	}
	return nil, persistence.ErrNotFound
}

func (f *fakeCLIBrokerActionRepo) Resolve(_ context.Context, id, status, approver string, note []byte, _ time.Time) error {
	f.resolved = append(f.resolved, id+"|"+status+"|"+approver)
	f.gotNote = note
	return f.resolveErr
}

func TestBrokerActionResolveStatus(t *testing.T) {
	for _, c := range []struct {
		executed, failed bool
		want             string
	}{
		{false, false, ""}, {true, true, ""},
		{true, false, persistence.BrokerActionExecuted}, {false, true, persistence.BrokerActionFailed},
	} {
		got, err := brokerActionResolveStatus(c.executed, c.failed)
		if got != c.want || (c.want == "") != (err != nil) {
			t.Errorf("(%v,%v) = %q, %v; want %q", c.executed, c.failed, got, err, c.want)
		}
	}
}

func unknownRow(id string) []*persistence.BrokerAction {
	return []*persistence.BrokerAction{{ActionID: id, Status: persistence.BrokerActionUnknown}}
}

func TestResolveBrokerAction_RecordsTheOperatorAndANonEmptyNote(t *testing.T) {
	repo := &fakeCLIBrokerActionRepo{rows: unknownRow("ba_1")}
	if err := resolveBrokerAction(context.Background(), repo, "ba_1", persistence.BrokerActionFailed, "ada", "", time.Now()); err != nil {
		t.Fatal(err)
	}
	if len(repo.resolved) != 1 || repo.resolved[0] != "ba_1|failed|ada" {
		t.Fatalf("Resolve calls = %v", repo.resolved)
	}
	var doc map[string]string
	if err := json.Unmarshal(repo.gotNote, &doc); err != nil || doc["note"] == "" || doc["resolved_by"] != "ada" {
		t.Fatalf("stored note = %s (%v); want a non-empty note and resolved_by", repo.gotNote, err)
	}
}

func TestResolveBrokerAction_NotResolvableIsAFriendlyError(t *testing.T) {
	repo := &fakeCLIBrokerActionRepo{rows: unknownRow("ba_1"), resolveErr: persistence.ErrBrokerActionNoTransition}
	err := resolveBrokerAction(context.Background(), repo, "ba_1", persistence.BrokerActionExecuted, "ada", "x", time.Now())
	if err == nil || !strings.Contains(err.Error(), "not resolvable") || !strings.Contains(err.Error(), "nothing was changed") {
		t.Fatalf("err = %v", err)
	}
}

func TestListBrokerActions_DefaultsToAttentionStatesAndNeverPrintsArgs(t *testing.T) {
	mk := func(id, status string) *persistence.BrokerAction {
		return &persistence.BrokerAction{ActionID: id, ProjectID: "p1", Status: status, ActionKind: "send_reply",
			Tool: "mcp__mail__send", ArgsJSON: []byte(`{"to":"SENTINEL_ARGS"}`), OutcomeJSON: []byte(`{"r":"SENTINEL_OUTCOME"}`)}
	}
	repo := &fakeCLIBrokerActionRepo{rows: []*persistence.BrokerAction{
		mk("ba_unknown", persistence.BrokerActionUnknown), mk("ba_pending", persistence.BrokerActionPending),
		mk("ba_done", persistence.BrokerActionExecuted),
	}}
	for _, asJSON := range []bool{false, true} {
		var out bytes.Buffer
		if err := listBrokerActions(context.Background(), repo, "", "", asJSON, &out); err != nil {
			t.Fatal(err)
		}
		s := out.String()
		if !strings.Contains(s, "ba_unknown") || !strings.Contains(s, "ba_pending") || strings.Contains(s, "ba_done") {
			t.Errorf("json=%v: default listing wrong:\n%s", asJSON, s)
		}
		if strings.Contains(s, "SENTINEL") {
			t.Errorf("json=%v: listing leaked arguments or outcome:\n%s", asJSON, s)
		}
	}
	var out bytes.Buffer
	if err := listBrokerActions(context.Background(), repo, "", persistence.BrokerActionExecuted, false, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "ba_done") {
		t.Errorf("--status executed missing ba_done:\n%s", out.String())
	}
}

// review-20260930-1334 F1: an executing row that is not yet stuck may have its
// call in flight; resolving it --failed would misrecord a write that is
// about to be sent. resolve refuses it and changes nothing.
func TestResolveBrokerAction_RefusesAnExecutingRowStillInFlight(t *testing.T) {
	now := time.Now().UTC()
	claimed := now.Add(-time.Minute)
	repo := &fakeCLIBrokerActionRepo{rows: []*persistence.BrokerAction{{
		ActionID: "ba_1", Status: persistence.BrokerActionExecuting, ExecutedAt: &claimed,
	}}}
	err := resolveBrokerAction(context.Background(), repo, "ba_1", persistence.BrokerActionFailed, "ada", "x", now)
	if err == nil || !strings.Contains(err.Error(), "may still be in flight") {
		t.Fatalf("err = %v", err)
	}
	if len(repo.resolved) != 0 {
		t.Fatal("an in-flight action was resolved")
	}
	old := now.Add(-20 * time.Minute)
	repo.rows[0].ExecutedAt = &old
	if err := resolveBrokerAction(context.Background(), repo, "ba_1", persistence.BrokerActionFailed, "ada", "x", now); err != nil {
		t.Fatalf("a stuck executing row must be resolvable: %v", err)
	}
}

func TestFakeCLIBrokerActionRepo_HonoursTheMissContract(t *testing.T) {
	repotest.AssertMissRepo(t, "BrokerActionRepository.Get", (&fakeCLIBrokerActionRepo{}).Get)
}

func TestResolveBrokerAction_UnknownIDIsAClearError(t *testing.T) {
	repo := &fakeCLIBrokerActionRepo{}
	err := resolveBrokerAction(context.Background(), repo, "nope", persistence.BrokerActionFailed, "ada", "", time.Now())
	if err == nil || !strings.Contains(err.Error(), "no broker action nope") || len(repo.resolved) != 0 {
		t.Fatalf("err = %v, resolved = %v", err, repo.resolved)
	}
}

// review-20260930-1334 F3: a full page is disclosed, as the doctor check does.
func TestListBrokerActions_DisclosesTheCap(t *testing.T) {
	repo := &fakeCLIBrokerActionRepo{}
	for i := 0; i < brokerActionListCap; i++ {
		repo.rows = append(repo.rows, &persistence.BrokerAction{ActionID: "ba", ProjectID: "p1", Status: persistence.BrokerActionUnknown})
	}
	var out bytes.Buffer
	if err := listBrokerActions(context.Background(), repo, "", persistence.BrokerActionUnknown, false, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "only the oldest 200 unknown actions are shown") {
		t.Fatalf("cap not disclosed:\n%s", out.String()[len(out.String())-200:])
	}
}
