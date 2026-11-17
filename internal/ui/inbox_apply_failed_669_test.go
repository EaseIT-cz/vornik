package ui

// GitHub #669 item 5 (design 22, §22.2.16b): approved requests that could
// not be applied are shown in the console inbox for an operator-wide viewer,
// read-only, beside the pending ones. Before, they appeared nowhere. Found
// at: production. Regression test at: integration (the rendered inbox).
// Lower because there is no browser lane.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func applyFailedServer(rows []ApplyFailedCard, more bool, err error) *Server {
	return NewServer(WithAgentApplyFailed(func(context.Context) ([]ApplyFailedCard, bool, error) { return rows, more, err }))
}

func TestInbox669_ApplyFailedShownToTheOperator(t *testing.T) {
	srv := applyFailedServer([]ApplyFailedCard{{Sentence: "Connect the Hermes account.", ApprovedAt: time.Now().Add(-2 * time.Hour),
		Reason: "gave up after 60 attempts: busy"}}, true, nil)
	rec := httptest.NewRecorder()
	srv.Inbox(rec, httptest.NewRequest(http.MethodGet, "/ui/inbox", nil))
	body := rec.Body.String()
	for _, want := range []string{"Approved, but could not be applied", "Connect the Hermes account.", "gave up after 60 attempts: busy", "More requests failed than are shown here."} {
		if !strings.Contains(body, want) {
			t.Errorf("the inbox lacks %q", want)
		}
	}
	if strings.Contains(body, "All clear") {
		t.Error(`the inbox says "All clear" beside a failed request`)
	}
}

func TestInbox669_ApplyFailedHiddenFromAScopedViewer(t *testing.T) {
	srv := applyFailedServer([]ApplyFailedCard{{Sentence: "Secret failed sentence.", ApprovedAt: time.Now(), Reason: "x"}}, false, nil)
	rec := httptest.NewRecorder()
	srv.Inbox(rec, scopedUIRequest(http.MethodGet, "/ui/inbox", []string{"p1"}))
	if strings.Contains(rec.Body.String(), "Secret failed sentence.") {
		t.Fatal("a scoped viewer saw a failed agent request")
	}
}

func TestInbox669_ApplyFailedLoadErrorShowsNoSection(t *testing.T) {
	srv := applyFailedServer(nil, false, errors.New("db down"))
	rec := httptest.NewRecorder()
	srv.Inbox(rec, httptest.NewRequest(http.MethodGet, "/ui/inbox", nil))
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "Approved, but could not be applied") {
		t.Fatalf("a load error broke the inbox or showed an empty section (status %d)", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "All clear") || !strings.Contains(rec.Body.String(), "Could not load the approved requests that failed to apply") {
		t.Error(`a load error says "All clear" or hides that the list could not be read`)
	}
}
