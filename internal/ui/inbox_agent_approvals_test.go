package ui

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"vornik.io/vornik/internal/persistence"
)

// The operator's iPhone, 2026-10-03 (agent-administered design §9.2a item
// 3): agent approval requests waited on the approver device while /inbox,
// the console the operator already had open, showed nothing.
func TestInbox_ListsWhatWaitsOnTheApproverDevice(t *testing.T) {
	now := time.Now()
	var rows []persistence.AgentApprovalRequestRow
	for i, kind := range []string{"widening_change", "credential_slot", "broker_action", "host_action", "device_enrollment"} {
		rows = append(rows, persistence.AgentApprovalRequestRow{
			ID: "apr_000000000000000" + string(rune('1'+i)), Namespace: "claudecode", Kind: kind,
			Sentence: "Sentence for " + kind + ".", Status: "pending", Rendered: []byte(`{"body":"ARGUMENT-MARKER"}`),
			CreatedAt: now.Add(-time.Duration(i+1) * time.Minute), ExpiresAt: now.Add(time.Hour),
		})
	}
	srv := NewServer(WithAgentApprovals(func(context.Context) ([]persistence.AgentApprovalRequestRow, error) { return rows, nil }))
	rec := httptest.NewRecorder()
	srv.Inbox(rec, httptest.NewRequest(http.MethodGet, "/ui/inbox", nil))
	body := rec.Body.String()
	if !strings.Contains(body, "Waiting on your approver device") {
		t.Fatal("no section for the approver device")
	}
	for _, r := range rows {
		if !strings.Contains(body, r.Sentence) || !strings.Contains(body, `href="/ui/approve/`+r.ID+`"`) {
			t.Errorf("%s: sentence or link missing", r.Kind)
		}
	}
	if strings.Count(body, "Open on your approver device") != len(rows) {
		t.Error("each request needs its link label")
	}
	// The sentence only, never the arguments: those stay on the device page
	// (review 20261003-601e F3).
	if strings.Contains(body, "ARGUMENT-MARKER") {
		t.Error("the inbox rendered a request's arguments")
	}
	// It decides nothing here: no form posts to an agent request.
	if strings.Contains(body, `action="/ui/approve/`) || strings.Contains(body, `hx-post="/ui/approve/`) {
		t.Error("the inbox offers a decision on an agent request")
	}
}

// A project-scoped viewer is not the operator; the requests name other
// namespaces' changes, so the section is not shown to them.
func TestInbox_ApproverDeviceSectionHiddenFromAScopedViewer(t *testing.T) {
	srv := NewServer(WithAgentApprovals(func(context.Context) ([]persistence.AgentApprovalRequestRow, error) {
		return []persistence.AgentApprovalRequestRow{{ID: "apr_0000000000000001", Kind: "widening_change", Sentence: "Secret sentence.", CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour)}}, nil
	}))
	rec := httptest.NewRecorder()
	srv.Inbox(rec, scopedUIRequest(http.MethodGet, "/ui/inbox", []string{"p1"}))
	if strings.Contains(rec.Body.String(), "Secret sentence.") {
		t.Fatal("a scoped viewer saw an agent approval request")
	}
}

// The section counts toward "Needs you", and the page does not also say
// "All clear" while a request waits.
func TestInbox_ApproverDeviceRequestsAreNotAllClear(t *testing.T) {
	srv := NewServer(WithAgentApprovals(func(context.Context) ([]persistence.AgentApprovalRequestRow, error) {
		return []persistence.AgentApprovalRequestRow{{ID: "apr_0000000000000001", Kind: "widening_change", Sentence: "Raise a budget.", CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour)}}, nil
	}))
	rec := httptest.NewRecorder()
	srv.Inbox(rec, httptest.NewRequest(http.MethodGet, "/ui/inbox", nil))
	body := rec.Body.String()
	if strings.Contains(body, "All clear") {
		t.Error("All clear shown while a request waits")
	}
	if !strings.Contains(body, `<span class="text-xs text-gray-500">1</span>`) {
		t.Error("the waiting request is not counted")
	}
}

// Review 20261003-6d79 F9: a failed list hides the section and adds nothing
// to "Needs you", rather than failing the inbox.
func TestInbox_ApproverDeviceListFailureHidesTheSection(t *testing.T) {
	srv := NewServer(WithAgentApprovals(func(context.Context) ([]persistence.AgentApprovalRequestRow, error) {
		return nil, errors.New("store down")
	}))
	rec := httptest.NewRecorder()
	srv.Inbox(rec, httptest.NewRequest(http.MethodGet, "/ui/inbox", nil))
	body := rec.Body.String()
	if rec.Code != http.StatusOK || strings.Contains(body, "Waiting on your approver device") {
		t.Fatalf("status %d; section shown on failure", rec.Code)
	}
	if !strings.Contains(body, `<span class="text-xs text-gray-500">0</span>`) {
		t.Error("a failed list changed the count")
	}
}

// Review 20261003-6d79 F10: a row stamped ahead of this host's clock reads
// "0s ago", never a negative age.
func TestInbox_ApproverDeviceFutureRowHasNoNegativeAge(t *testing.T) {
	srv := NewServer(WithAgentApprovals(func(context.Context) ([]persistence.AgentApprovalRequestRow, error) {
		return []persistence.AgentApprovalRequestRow{{ID: "apr_0000000000000001", Sentence: "Skewed.", CreatedAt: time.Now().Add(time.Minute)}}, nil
	}))
	rec := httptest.NewRecorder()
	srv.Inbox(rec, httptest.NewRequest(http.MethodGet, "/ui/inbox", nil))
	if body := rec.Body.String(); !strings.Contains(body, ">0s ago<") {
		t.Fatal("future row did not read 0s ago")
	}
}

// GitHub review 9fd5 (2026-10-03), T13: a failed approver-request list is
// counted as error, a good one as ok; an unwired seam and a scoped viewer
// attempt no load and count nothing.
func TestInbox_AgentApprovalsLoadIsCounted(t *testing.T) {
	list := func(err error) ServerOption {
		return WithAgentApprovals(func(context.Context) ([]persistence.AgentApprovalRequestRow, error) { return nil, err })
	}
	render := func(m *InboxMetrics, opt ServerOption, r *http.Request) {
		NewServer(WithInboxMetrics(m), opt).Inbox(httptest.NewRecorder(), r)
	}
	get := func() *http.Request { return httptest.NewRequest(http.MethodGet, "/ui/inbox", nil) }
	val := func(m *InboxMetrics, o string) float64 {
		return testutil.ToFloat64(m.AgentApprovalsLoadTotal.WithLabelValues(o))
	}

	m := NewInboxMetrics(prometheus.NewRegistry())
	render(m, list(errors.New("store down")), get())
	if val(m, "error") != 1 || val(m, "ok") != 0 {
		t.Fatalf("failure: error=%v ok=%v", val(m, "error"), val(m, "ok"))
	}
	m = NewInboxMetrics(prometheus.NewRegistry())
	render(m, list(nil), get())
	if val(m, "ok") != 1 || val(m, "error") != 0 {
		t.Fatalf("success: ok=%v error=%v", val(m, "ok"), val(m, "error"))
	}
	m = NewInboxMetrics(prometheus.NewRegistry())
	render(m, list(errors.New("x")), scopedUIRequest(http.MethodGet, "/ui/inbox", []string{"p1"}))
	if n := testutil.CollectAndCount(m.AgentApprovalsLoadTotal); n != 0 {
		t.Fatalf("a scoped viewer attempted a load: %d series", n)
	}
}
