package approverdevice

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

// Broker write-actions design, approval fatigue tier 1 (review 5c20,
// 2026-10-03): the writes one task drafted are reviewed together and decided
// in one pass. Each stays bound to its own shown hash; nothing is
// preselected; a stale one is refused and named, the others proceed.
func TestPages_BatchReviewOfOneTasksWrites(t *testing.T) {
	f := newFixture(t)
	srv := serve(t, f)
	phone := newBrowser(t, srv)
	code, _, _ := f.svc.StartPairing(context.Background(), "Pixel")
	phone.pairWith(code)
	applied := map[string]bool{}
	f.svc.RegisterEffect(persistence.ApprovalKindBrokerAction, func(_ context.Context, r persistence.AgentApprovalRequestRow) error {
		applied[r.ID] = true
		return nil
	})
	f.svc.RegisterDescriber(persistence.ApprovalKindBrokerAction, func(persistence.AgentApprovalRequestRow) *Description {
		return &Description{Summary: "Send a reply.", Level: "High", Reasons: []string{"it sends mail"}, Group: "task_t1", GroupTitle: "inbox-digest, one run"}
	})
	for _, id := range []string{"apr_ba_1", "apr_ba_2", "apr_ba_3"} {
		if err := f.repo.CreateRequest(context.Background(), persistence.AgentApprovalRequestRow{ID: id, Kind: persistence.ApprovalKindBrokerAction,
			Sentence: "Reply " + id + ".", Rendered: []byte(`{"args":{"body":"` + id + `"}}`), RenderedSHA256: "h_" + id, Status: persistence.ApprovalPending,
			CreatedAt: f.clock(), ExpiresAt: f.clock().Add(RequestTTL)}); err != nil {
			t.Fatal(err)
		}
	}
	_, list := phone.do(http.MethodGet, "/ui/approve/", nil, nil)
	if !strings.Contains(list, "3 waiting: inbox-digest, one run") || !strings.Contains(list, "/ui/approve/group/task_t1") {
		t.Fatalf("the list does not group the task's writes:\n%s", list)
	}
	_, page := phone.do(http.MethodGet, "/ui/approve/group/task_t1", nil, nil)
	if n := strings.Count(page, `type="checkbox"`); n != 3 {
		t.Fatalf("%d checkboxes, want 3", n)
	}
	if regexp.MustCompile(`type="checkbox"[^>]*\bchecked\b`).MatchString(page) {
		t.Fatal("something is preselected")
	}
	for _, id := range []string{"apr_ba_1", "apr_ba_2", "apr_ba_3"} {
		if !strings.Contains(page, id) {
			t.Fatalf("the page does not show %s's arguments", id)
		}
	}
	res, _ := phone.do(http.MethodPost, "/ui/approve/group/task_t1", url.Values{
		"decision": {"approve"},
		"pick":     {"apr_ba_1|h_apr_ba_1", "apr_ba_2|stale", "apr_ba_3|h_apr_ba_3"},
	}, nil)
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("batch POST: %d", res.StatusCode)
	}
	loc := res.Header.Get("Location")
	if !applied["apr_ba_1"] || !applied["apr_ba_3"] || applied["apr_ba_2"] {
		t.Fatalf("applied %v, want 1 and 3 only", applied)
	}
	if !regexp.MustCompile(`refused=apr_ba_2`).MatchString(loc) {
		t.Fatalf("the refused write is not named: %s", loc)
	}
	if r, _ := f.repo.GetRequest(context.Background(), "apr_ba_2"); r.Status != persistence.ApprovalPending {
		t.Fatalf("the stale one was decided: %s", r.Status)
	}
	_, after := phone.do(http.MethodGet, loc, nil, nil)
	if !strings.Contains(after, "apr_ba_2") {
		t.Fatalf("the list after the batch does not say which one changed:\n%s", after)
	}
}

// Tier 1 (review 5c20): the writes one task drafted reach the person as one
// push, content-free, not one push per write.
func TestFileRequestBatch_OnePushForTheBatch(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	var rows []persistence.AgentApprovalRequestRow
	for _, id := range []string{"apr_ba_1", "apr_ba_2", "apr_ba_3"} {
		rows = append(rows, persistence.AgentApprovalRequestRow{ID: id, Kind: persistence.ApprovalKindBrokerAction,
			Sentence: "Reply " + id + ".", Rendered: []byte(`{"secret_body":"PRIVATE"}`), RenderedSHA256: "h", Status: persistence.ApprovalPending,
			CreatedAt: f.clock(), ExpiresAt: f.clock().Add(RequestTTL)})
	}
	if err := f.svc.FileRequestBatch(ctx, rows, "3 writes drafted by inbox-digest are waiting for you"); err != nil {
		t.Fatal(err)
	}
	if len(f.pushes) != 1 || !strings.Contains(f.pushes[0].body, "3 writes drafted by inbox-digest") ||
		strings.Contains(f.pushes[0].body, "PRIVATE") || !strings.Contains(f.pushes[0].body, "https://vornik.example/ui/approve/") {
		t.Fatalf("pushes %+v", f.pushes)
	}
	for _, r := range rows {
		if _, err := f.repo.GetRequest(ctx, r.ID); err != nil {
			t.Fatalf("%s not filed: %v", r.ID, err)
		}
	}
	// Filing the same batch again files and pushes nothing.
	if err := f.svc.FileRequestBatch(ctx, rows, "again"); err != nil {
		t.Fatal(err)
	}
	if len(f.pushes) != 1 {
		t.Fatalf("a refiled batch pushed again: %+v", f.pushes)
	}
}
