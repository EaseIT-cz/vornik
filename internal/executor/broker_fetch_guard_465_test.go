package executor

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/registry"
)

// GitHub #465 (2026-10-21, trading-dashboard-design "Amendment 2026-10-21
// (#465)"): fetchQuote and fetchDailyBars used http.DefaultClient, which
// follows up to 10 redirects to any host and forwards X-Project-ID on every
// hop, with no dial guard. They now dial through the shared guard
// (integrations.LoopbackURLGuard(...).NoRedirectHTTPClient) and read the body
// through integrations.ReadBoundedBody. These tests use a real HTTP broker
// (httptest), the level the broker's answer reaches the daemon.

// redirectTarget answers 200 and records each request and its X-Project-ID.
func redirectTarget(t *testing.T) (*httptest.Server, *atomic.Int32, *atomic.Value) {
	t.Helper()
	var hits atomic.Int32
	var projectHeader atomic.Value
	projectHeader.Store("")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		projectHeader.Store(r.Header.Get("X-Project-ID"))
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits, &projectHeader
}

// redirectingBroker answers every request with a 307 (method and body kept,
// so a following client re-POSTs with the header) to target.
func redirectingBroker(t *testing.T, target string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target+"/message", http.StatusTemporaryRedirect)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// loopbackAliasURL rewrites a loopback httptest URL to the host "localhost."
// (trailing dot): it resolves to loopback but is not the literal the loopback
// rule allow-lists, so only the resolved-IP check can refuse it.
func loopbackAliasURL(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	u.Host = "localhost.:" + u.Port()
	return u.String()
}

func TestFetchQuote_RedirectIsNotFollowedAndProjectIDStaysHome_Issue465(t *testing.T) {
	target, hits, hdr := redirectTarget(t)
	broker := redirectingBroker(t, target.URL)

	_, err := fetchQuote(context.Background(), broker.URL, "proj-465", "NVDA")
	if err == nil || !strings.Contains(err.Error(), "broker returned 307") {
		t.Fatalf("want broker returned 307, got %v", err)
	}
	if hits.Load() != 0 {
		t.Fatalf("redirect target received %d request(s), X-Project-ID=%q; want none", hits.Load(), hdr.Load())
	}
}

func TestFetchDailyBars_RedirectIsNotFollowed_Issue465(t *testing.T) {
	// fetchDailyBars is also the entry gate's SMA source (entryGateIndicators).
	target, hits, hdr := redirectTarget(t)
	broker := redirectingBroker(t, target.URL)

	_, err := fetchDailyBars(context.Background(), broker.URL, "proj-465", "NVDA")
	if err == nil || !strings.Contains(err.Error(), "broker returned 307") {
		t.Fatalf("want broker returned 307, got %v", err)
	}
	if hits.Load() != 0 {
		t.Fatalf("redirect target received %d request(s), X-Project-ID=%q; want none", hits.Load(), hdr.Load())
	}
}

func TestBrokerFetches_RebindingShapedHostIsRefused_Issue465(t *testing.T) {
	requireLoopbackAlias465(t)
	var hits atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()
	alias := loopbackAliasURL(t, backend.URL)

	if _, err := fetchQuote(context.Background(), alias, "p", "NVDA"); err == nil || !strings.Contains(err.Error(), "dial guard") {
		t.Errorf("fetchQuote: want a dial guard refusal, got %v", err)
	}
	if _, err := fetchDailyBars(context.Background(), alias, "p", "NVDA"); err == nil || !strings.Contains(err.Error(), "dial guard") {
		t.Errorf("fetchDailyBars: want a dial guard refusal, got %v", err)
	}
	if hits.Load() != 0 {
		t.Fatalf("a name resolving to loopback was dialled %d time(s)", hits.Load())
	}
}

func TestBrokerFetches_OversizedBodyIsRefusedNotTruncated_Issue465(t *testing.T) {
	// One byte over each fetch's limit (quote 64 KiB, bars 4 MiB).
	for _, tc := range []struct {
		name  string
		limit int
		fetch func(string) error
	}{
		{"quote", 64 * 1024, func(u string) error { _, err := fetchQuote(context.Background(), u, "p", "X"); return err }},
		{"bars", 4 * 1024 * 1024, func(u string) error { _, err := fetchDailyBars(context.Background(), u, "p", "X"); return err }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, strings.Repeat(" ", tc.limit+1))
			}))
			defer srv.Close()
			err := tc.fetch(srv.URL)
			if err == nil || !strings.Contains(err.Error(), "exceeds") {
				t.Fatalf("want a size refusal, got %v", err)
			}
		})
	}
}

func TestBuildWatchlistQuotesBlock_RedirectingBrokerRendersFetchFailed_Issue465(t *testing.T) {
	target, hits, _ := redirectTarget(t)
	broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), `"EVIL"`) {
			http.Redirect(w, r, target.URL+"/message", http.StatusTemporaryRedirect)
			return
		}
		inner, _ := json.Marshal(map[string]any{"symbol": "NVDA", "last": 100.0, "bid": 99.9, "ask": 100.1})
		_ = json.NewEncoder(w).Encode(map[string]any{
			"result": map[string]any{"content": []map[string]any{{"type": "text", "text": string(inner)}}},
		})
	}))
	defer broker.Close()
	t.Setenv("VORNIK_BROKER_BASE_URL", broker.URL)

	e := &Executor{logger: zerolog.Nop()}
	project := &registry.Project{ID: "p", Trading: registry.ProjectTrading{Watchlist: []string{"NVDA", "EVIL"}}}
	got := e.buildWatchlistQuotesBlock(context.Background(), project)

	if hits.Load() != 0 {
		t.Fatalf("redirect target received %d request(s); want none", hits.Load())
	}
	if !strings.Contains(got, "| EVIL | — | — | — | — | fetch_failed: broker returned 307") {
		t.Fatalf("the redirected symbol must render fetch_failed, got:\n%s", got)
	}
	if !strings.Contains(got, "$100.00") {
		t.Fatalf("the clean symbol must still render, got:\n%s", got)
	}
}

// requireLoopbackAlias465 pins the rebinding tests' premise (code review a71e
// finding 1): "localhost." must resolve, and only to loopback, so that the
// dial reaches the guard's resolved-IP check. Without it a DNS failure would
// refuse the dial for the wrong reason and the test could pass on broken code.
func requireLoopbackAlias465(t *testing.T) {
	t.Helper()
	addrs, err := net.DefaultResolver.LookupIPAddr(context.Background(), "localhost.")
	if err != nil || len(addrs) == 0 {
		t.Fatalf("test premise: \"localhost.\" must resolve on this host (got %v, %v)", addrs, err)
	}
	for _, a := range addrs {
		if !a.IP.IsLoopback() {
			t.Fatalf("test premise: \"localhost.\" resolved to non-loopback %s", a.IP)
		}
	}
}
