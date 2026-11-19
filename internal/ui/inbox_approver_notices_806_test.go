package ui

// GitHub #806 (design 22 §22.2.16d, part 06 §6.5 amendment 2026-11-17): the
// console inbox's informational cards had no action. Found at: production
// (operator). Regression tests at: integration (the rendered inbox and its
// POST routes); the Chromium case clicks the button.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// noticeFixture806 is a console whose cards live in memory, as the
// approver-device service would hold them.
type noticeFixture806 struct {
	mu      sync.Mutex // the browser case reads while a handler writes
	failed  []ApplyFailedCard
	notices []NoticeCard
	actors  []string
}

// actorCount is len(actors) under the lock.
func (f *noticeFixture806) actorCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.actors)
}

func (f *noticeFixture806) server() *Server {
	return NewServer(
		WithAgentApplyFailed(func(context.Context) ([]ApplyFailedCard, bool, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			return append([]ApplyFailedCard(nil), f.failed...), false, nil
		}),
		WithApproverNoticeActions(&ApproverNoticeActions{
			Notices: func(context.Context) []NoticeCard {
				f.mu.Lock()
				defer f.mu.Unlock()
				return append([]NoticeCard(nil), f.notices...)
			},
			Acknowledge: func(_ context.Context, key, actor string) error {
				f.mu.Lock()
				defer f.mu.Unlock()
				for i, n := range f.notices {
					if n.Key == key {
						f.notices = append(f.notices[:i], f.notices[i+1:]...)
						f.actors = append(f.actors, actor)
						return nil
					}
				}
				return ErrNotChangeable
			},
			Dismiss: func(_ context.Context, id, actor string) error {
				f.mu.Lock()
				defer f.mu.Unlock()
				for i, c := range f.failed {
					if c.ID == id {
						f.failed = append(f.failed[:i], f.failed[i+1:]...)
						f.actors = append(f.actors, actor)
						return nil
					}
				}
				return ErrNotChangeable
			},
		}))
}

func TestInbox806_DismissAndAcknowledge(t *testing.T) {
	for _, b := range []keysSessionBackend{admin794, owner754} {
		f := &noticeFixture806{
			failed:  []ApplyFailedCard{{ID: "apr_1", Sentence: "Connect the Hermes account.", ApprovedAt: time.Now(), Reason: "rejected"}},
			notices: []NoticeCard{{Key: "k1", Text: "Withheld: hermes's server scraper."}},
		}
		srv := f.server()
		body := doConsole794(t, srv, http.MethodGet, "/inbox", b, nil).Body.String()
		for _, want := range []string{`action="/ui/inbox/apply-failed/apr_1/dismiss"`, `action="/ui/inbox/notices/k1/ack"`,
			"Withheld: hermes&#39;s server scraper.", "Ask your assistant to request it again."} {
			if !strings.Contains(body, want) {
				t.Errorf("%s: the inbox lacks %q", b.role, want)
			}
		}
		if rec := doConsole794(t, srv, http.MethodPost, "/inbox/apply-failed/apr_1/dismiss", b, nil); rec.Code != http.StatusSeeOther {
			t.Fatalf("%s: dismiss answered %d", b.role, rec.Code)
		}
		if rec := doConsole794(t, srv, http.MethodPost, "/inbox/notices/k1/ack", b, nil); rec.Code != http.StatusSeeOther {
			t.Fatalf("%s: ack answered %d", b.role, rec.Code)
		}
		if len(f.failed) != 0 || len(f.notices) != 0 || len(f.actors) != 2 || !strings.HasPrefix(f.actors[0], "operator:") {
			t.Fatalf("%s: after the two POSTs: failed %v notices %v actors %v", b.role, f.failed, f.notices, f.actors)
		}
		body = doConsole794(t, srv, http.MethodGet, "/inbox", b, nil).Body.String()
		if strings.Contains(body, "Connect the Hermes account.") || strings.Contains(body, "server scraper") {
			t.Errorf("%s: a cleared card is still shown", b.role)
		}
		if rec := doConsole794(t, srv, http.MethodPost, "/inbox/apply-failed/apr_1/dismiss", b, nil); rec.Code != http.StatusConflict {
			t.Errorf("%s: a second dismiss answered %d, want 409", b.role, rec.Code)
		}
	}
}

func TestInbox806_GateRefusals(t *testing.T) {
	f := &noticeFixture806{failed: []ApplyFailedCard{{ID: "apr_1", Sentence: "s", Reason: "r"}}}
	srv := f.server()
	// A project member does not see the cards, so the POST is a 404.
	if rec := doConsole794(t, srv, http.MethodPost, "/inbox/apply-failed/apr_1/dismiss", member754, nil); rec.Code != http.StatusNotFound {
		t.Errorf("project member answered %d, want 404", rec.Code)
	}
	// A cross-site POST is refused before anything changes.
	r := httptest.NewRequest(http.MethodPost, "/inbox/apply-failed/apr_1/dismiss", nil)
	r.Header.Set("Sec-Fetch-Site", "cross-site")
	rec := httptest.NewRecorder()
	srv.ApproverNoticeRouter(rec, r)
	if rec.Code < 400 {
		t.Errorf("cross-site dismiss answered %d", rec.Code)
	}
	// GET is not an action.
	if rec := doConsole794(t, srv, http.MethodGet, "/inbox/apply-failed/apr_1/dismiss", admin794, nil); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET answered %d", rec.Code)
	}
	if len(f.failed) != 1 || len(f.actors) != 0 {
		t.Fatalf("a refused request changed a card: %v %v", f.failed, f.actors)
	}
	// The member sees the inbox without the cards or their buttons.
	if body := doConsole794(t, srv, http.MethodGet, "/inbox", member754, nil).Body.String(); strings.Contains(body, "apply-failed/apr_1") {
		t.Error("a project member is offered Dismiss")
	}
}
