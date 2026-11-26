package github

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// GitHub #906: after a held start is released, the daemon redelivers the
// App webhook deliveries that failed while it was held (deploy idle gate
// design §13.13).

type fakeDelivery struct {
	ID           int64
	GUID         string
	At           time.Time
	StatusCode   int
	Redelivery   bool
	Event        string
	Installation int64
}

// fakeGitHubDeliveries serves GET /app/hook/deliveries in pages of pageSize,
// newest first, with a Link rel="next" cursor, and records the redelivery
// POSTs. attemptStatus is the answer to a POST (202 when zero).
type fakeGitHubDeliveries struct {
	t             *testing.T
	pageSize      int
	deliveries    []fakeDelivery
	listStatus    int
	attemptStatus int
	listDelay     time.Duration

	mu        sync.Mutex
	pagesRead []string
	posted    []int64
	authOK    bool
	authSeen  int
}

func (f *fakeGitHubDeliveries) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.authSeen++
		if a := r.Header.Get("Authorization"); !strings.HasPrefix(a, "Bearer ") || strings.Count(a, ".") != 2 {
			f.authOK = false
			f.t.Errorf("request %s %s without an App JWT: %q", r.Method, r.URL.Path, a)
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/app/hook/deliveries":
			cursor := r.URL.Query().Get("cursor")
			f.pagesRead = append(f.pagesRead, cursor)
			if f.listDelay > 0 {
				f.mu.Unlock()
				time.Sleep(f.listDelay)
				f.mu.Lock()
			}
			if f.listStatus != 0 {
				w.WriteHeader(f.listStatus)
				_, _ = w.Write([]byte(`{"message":"boom"}`))
				return
			}
			start := 0
			if cursor != "" {
				start, _ = strconv.Atoi(cursor)
			}
			end := start + f.pageSize
			if end > len(f.deliveries) {
				end = len(f.deliveries)
			}
			if end < len(f.deliveries) {
				w.Header().Set("Link", fmt.Sprintf(`<http://%s/app/hook/deliveries?per_page=100&cursor=%d>; rel="next"`, r.Host, end))
			}
			out := make([]map[string]any, 0, end-start)
			for _, d := range f.deliveries[start:end] {
				status := "OK"
				if d.StatusCode/100 != 2 {
					status = "Invalid HTTP Response: " + strconv.Itoa(d.StatusCode)
				}
				out = append(out, map[string]any{
					"id": d.ID, "guid": d.GUID, "delivered_at": d.At.UTC().Format(time.RFC3339),
					"redelivery": d.Redelivery, "status": status, "status_code": d.StatusCode,
					"event": d.Event, "installation_id": d.Installation,
				})
			}
			_ = json.NewEncoder(w).Encode(out)
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/app/hook/deliveries/") && strings.HasSuffix(r.URL.Path, "/attempts"):
			idStr := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/app/hook/deliveries/"), "/attempts")
			id, err := strconv.ParseInt(idStr, 10, 64)
			if err != nil {
				f.t.Errorf("bad attempts path %q", r.URL.Path)
			}
			if f.attemptStatus != 0 && f.attemptStatus != http.StatusAccepted {
				w.WriteHeader(f.attemptStatus)
				_, _ = w.Write([]byte(`{"message":"nope"}`))
				return
			}
			f.posted = append(f.posted, id)
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{}`))
		default:
			f.t.Errorf("unexpected request %s %s", r.Method, r.URL.String())
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

func redeliveryChannel(t *testing.T, srv *httptest.Server, key *rsa.PrivateKey) *Channel {
	t.Helper()
	ch, err := New(Config{
		WebhookSecret: "shhh",
		APIBaseURL:    srv.URL,
		HTTPClient:    srv.Client(),
		Installations: []InstallationConfig{
			{ProjectID: "a", InstallationID: 100, AppID: 4040, PrivateKey: key, RepoAllowlist: []string{"acme/api"}},
			// The same App installed a second time is still one App: its
			// deliveries are listed once.
			{ProjectID: "b", InstallationID: 200, AppID: 4040, PrivateKey: key, RepoAllowlist: []string{"acme/web"}},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return ch
}

func TestRedeliverFailed_906_RedeliversUnavailabilityOnly(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)
	from, to := t0, t0.Add(time.Hour)
	m := t0.Add
	f := &fakeGitHubDeliveries{t: t, pageSize: 5, authOK: true, deliveries: []fakeDelivery{
		// newest first
		{ID: 12, GUID: "g7", At: to.Add(time.Minute), StatusCode: 503},                   // after the window: not counted
		{ID: 11, GUID: "g1", At: m(50 * time.Minute), StatusCode: 503},                   // redeliver
		{ID: 10, GUID: "g2", At: m(45 * time.Minute), StatusCode: 200, Redelivery: true}, // g2 succeeded
		{ID: 9, GUID: "g3", At: m(40 * time.Minute), StatusCode: 400},                    // the daemon's answer: not tried
		{ID: 8, GUID: "g4", At: m(35 * time.Minute), StatusCode: 0},                      // no response: redeliver
		{ID: 7, GUID: "g2", At: m(30 * time.Minute), StatusCode: 503},                    // skipped: g2 has a 2xx
		{ID: 6, GUID: "g5", At: m(20 * time.Minute), StatusCode: 502},                    // latest attempt of g5
		{ID: 5, GUID: "g5", At: m(10 * time.Minute), StatusCode: 503},                    // older attempt: not posted
		{ID: 4, GUID: "g6", At: from.Add(-time.Minute), StatusCode: 503},                 // before the window: stop
		{ID: 3, GUID: "g8", At: from.Add(-time.Hour), StatusCode: 503},                   // never read
		{ID: 2, GUID: "g9", At: from.Add(-2 * time.Hour), StatusCode: 503},               // page 3: never read
	}}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()

	rep := redeliveryChannel(t, srv, key).RedeliverFailed(context.Background(), from, to)

	sort.Slice(f.posted, func(i, j int) bool { return f.posted[i] > f.posted[j] })
	if got, want := fmt.Sprint(f.posted), "[11 8 6]"; got != want {
		t.Errorf("redelivered ids = %s, want %s", got, want)
	}
	if len(f.pagesRead) != 2 {
		t.Errorf("pages read = %d (%q), want 2: paging stops at the first delivery older than the window, and two installations of one App list once", len(f.pagesRead), f.pagesRead)
	}
	want := RedeliveryReport{State: "done", WindowFrom: from, WindowTo: to, Apps: 1,
		Examined: 7, Failed: 4, Redelivered: 3, NotRedelivered: 1, NotUnavailability: 1}
	if rep.State != want.State || !rep.WindowFrom.Equal(from) || !rep.WindowTo.Equal(to) ||
		rep.Apps != want.Apps || rep.Examined != want.Examined || rep.Failed != want.Failed ||
		rep.Redelivered != want.Redelivered || rep.NotRedelivered != want.NotRedelivered ||
		rep.NotUnavailability != want.NotUnavailability || rep.Truncated || len(rep.Errors) != 0 {
		t.Errorf("report = %+v\nwant     %+v", rep, want)
	}
}

func TestRedeliverFailed_906_BoundsTheRedeliveries(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	t0 := time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)
	var ds []fakeDelivery
	for i := 0; i < 250; i++ {
		ds = append(ds, fakeDelivery{ID: int64(1000 - i), GUID: fmt.Sprintf("g%d", i), At: t0.Add(time.Hour - time.Duration(i)*time.Second), StatusCode: 503})
	}
	f := &fakeGitHubDeliveries{t: t, pageSize: 100, authOK: true, deliveries: ds}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()

	rep := redeliveryChannel(t, srv, key).RedeliverFailed(context.Background(), t0, t0.Add(2*time.Hour))

	if len(f.posted) != maxRedeliveries {
		t.Errorf("POSTs = %d, want the bound %d", len(f.posted), maxRedeliveries)
	}
	if rep.Failed != 250 || rep.Redelivered != maxRedeliveries || rep.NotRedelivered != 250-maxRedeliveries || !rep.Truncated {
		t.Errorf("report = %+v, want failed 250, redelivered %d, not_redelivered %d, truncated", rep, maxRedeliveries, 250-maxRedeliveries)
	}
}

func TestRedeliverFailed_906_PageCapTruncates(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	t0 := time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)
	var ds []fakeDelivery
	for i := 0; i < maxRedeliveryPages*2+5; i++ {
		ds = append(ds, fakeDelivery{ID: int64(5000 - i), GUID: fmt.Sprintf("g%d", i), At: t0.Add(time.Hour - time.Duration(i)*time.Second), StatusCode: 200})
	}
	f := &fakeGitHubDeliveries{t: t, pageSize: 2, authOK: true, deliveries: ds}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()

	rep := redeliveryChannel(t, srv, key).RedeliverFailed(context.Background(), t0, t0.Add(2*time.Hour))

	if len(f.pagesRead) != maxRedeliveryPages || !rep.Truncated || rep.Examined != maxRedeliveryPages*2 {
		t.Errorf("pages %d, report %+v: want %d pages, truncated", len(f.pagesRead), rep, maxRedeliveryPages)
	}
}

func TestRedeliverFailed_906_ErrorsAreReported(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	t0 := time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)

	t.Run("list fails", func(t *testing.T) {
		f := &fakeGitHubDeliveries{t: t, pageSize: 10, authOK: true, listStatus: http.StatusInternalServerError}
		srv := httptest.NewServer(f.handler())
		defer srv.Close()
		rep := redeliveryChannel(t, srv, key).RedeliverFailed(context.Background(), t0, t0.Add(time.Hour))
		if len(rep.Errors) != 1 || !strings.Contains(rep.Errors[0], "500") || rep.State != "done" || rep.Redelivered != 0 {
			t.Errorf("report = %+v, want one error naming HTTP 500", rep)
		}
	})
	t.Run("a POST is refused", func(t *testing.T) {
		f := &fakeGitHubDeliveries{t: t, pageSize: 10, authOK: true, attemptStatus: http.StatusUnprocessableEntity,
			deliveries: []fakeDelivery{{ID: 9, GUID: "g", At: t0.Add(time.Minute), StatusCode: 503}}}
		srv := httptest.NewServer(f.handler())
		defer srv.Close()
		rep := redeliveryChannel(t, srv, key).RedeliverFailed(context.Background(), t0, t0.Add(time.Hour))
		if rep.Failed != 1 || rep.Redelivered != 0 || rep.NotRedelivered != 1 || len(rep.Errors) != 1 || !strings.Contains(rep.Errors[0], "422") {
			t.Errorf("report = %+v, want the refused POST counted as not redelivered with its error", rep)
		}
	})
}

func TestRedeliverFailed_906_NoAppKeyIsNotConfigured(t *testing.T) {
	ch, err := New(Config{WebhookSecret: "shhh", Installations: []InstallationConfig{
		{ProjectID: "a", InstallationID: 100, RepoAllowlist: []string{"acme/api"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if ch.HasAppKey() {
		t.Error("HasAppKey = true for a channel with no App key")
	}
	rep := ch.RedeliverFailed(context.Background(), time.Now().Add(-time.Hour), time.Now())
	if rep.State != "not_configured" {
		t.Errorf("state = %q, want not_configured", rep.State)
	}
}

// Review 21bd #2: a slow, busy App must not spend the whole deadline
// listing; what was read is still redelivered.
func TestRedeliverFailed_906_ListingLeavesTimeToRedeliver(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	old := redeliveryDeadline
	redeliveryDeadline = 600 * time.Millisecond
	t.Cleanup(func() { redeliveryDeadline = old })
	t0 := time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)
	var ds []fakeDelivery
	for i := 0; i < 40; i++ {
		ds = append(ds, fakeDelivery{ID: int64(900 - i), GUID: fmt.Sprintf("g%d", i), At: t0.Add(time.Hour - time.Duration(i)*time.Second), StatusCode: 503})
	}
	f := &fakeGitHubDeliveries{t: t, pageSize: 4, authOK: true, deliveries: ds, listDelay: 120 * time.Millisecond}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()

	rep := redeliveryChannel(t, srv, key).RedeliverFailed(context.Background(), t0, t0.Add(2*time.Hour))

	if !rep.Truncated || rep.Examined == 0 || rep.Examined >= len(ds) {
		t.Fatalf("report = %+v, want listing cut short by its half of the deadline", rep)
	}
	if rep.Redelivered != rep.Examined {
		t.Fatalf("redelivered %d of the %d read: listing used the redelivery time", rep.Redelivered, rep.Examined)
	}
}

// Review d94e #4: a next cursor on another host is never followed with the
// App's JWT; a valid one later in the header still is.
func TestNextLink_906_OnlyTheConfiguredHost(t *testing.T) {
	base := "https://api.github.com"
	cases := map[string]string{
		`<https://evil.example/app/hook/deliveries?cursor=1>; rel="next"`:                                                    "",
		`<http://api.github.com/app/hook/deliveries?cursor=1>; rel="next"`:                                                   "",
		`<https://api.github.com/app/hook/deliveries?cursor=2>; rel="next"`:                                                  "https://api.github.com/app/hook/deliveries?cursor=2",
		`<https://api.github.com/x?cursor=0>; rel="prev", <https://api.github.com/app/hook/deliveries?cursor=3>; rel="next"`: "https://api.github.com/app/hook/deliveries?cursor=3",
		`<https://evil.example/a?cursor=1>; rel="next", <https://api.github.com/app/hook/deliveries?cursor=4>; rel="next"`:   "https://api.github.com/app/hook/deliveries?cursor=4",
		``: "",
	}
	for link, want := range cases {
		if got := nextLink(link, base); got != want {
			t.Errorf("nextLink(%q) = %q, want %q", link, got, want)
		}
	}
}
