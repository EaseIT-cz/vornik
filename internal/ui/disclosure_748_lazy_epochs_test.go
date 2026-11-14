package ui

// The memory Operate tab's older epochs load when their disclosure opens
// (web-ui-design.md #748 M). Found at: production; the page was 1,186,587
// bytes, 988,394 of them the 500 Epochs rows, 480 of which sat in a closed
// disclosure.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

func epochFixture(n int) []*persistence.CorpusEpoch {
	base := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	out := make([]*persistence.CorpusEpoch, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, &persistence.CorpusEpoch{
			ID: fmt.Sprintf("epoch_%03d", n-i), ProjectID: "p1",
			CreatedAt: base.Add(time.Duration(n-i) * time.Hour), IsActive: i == 0,
		})
	}
	return out
}

func lazyEpochServer() *Server {
	eps := epochFixture(30)
	return NewServer(
		WithCorpusEpochRepository(&uiStubEpochRepo{listEpochsFn: func(context.Context, string, int) ([]*persistence.CorpusEpoch, error) { return eps, nil }}),
		WithMemoryQuarantineRepository(&uiStubQuarantineRepo{}),
		WithMemoryEvictor(&stubMemoryEvictor{}),
	)
}

func TestLazyEpochs_748M_PageDefersTheOlderRows(t *testing.T) {
	body := renderMemoryOperate(t, 30, "")
	if got := len(regexp.MustCompile(`title="epoch_\d{3}"`).FindAllString(body, -1)); got != 20 {
		t.Errorf("rows in the page: got %d, want the newest 20", got)
	}
	if strings.Contains(body, `title="epoch_010"`) {
		t.Error("an older epoch must not render in the page")
	}
	tag := regexp.MustCompile(`<details class="vk-disc vk-disc-row"[^>]*hx-get="[^"]*"[^>]*>`).FindString(body)
	for _, want := range []string{`hx-get="/ui/memory/p1/epochs/older?epochs_limit=500"`, `hx-trigger="toggle once"`, `hx-target="find tbody"`, `hx-swap="innerHTML"`} {
		if !strings.Contains(tag, want) {
			t.Errorf("the older-epochs disclosure lacks %s: %q", want, tag)
		}
	}
	if !strings.Contains(body, "Loading older epochs…") {
		t.Error("the disclosure carries a placeholder until its rows load")
	}
	if few := renderMemoryOperate(t, 5, ""); strings.Contains(few, "/epochs/older") {
		t.Error("with 20 or fewer epochs there is no older disclosure")
	}
}

func TestLazyEpochs_748M_FragmentServesTheOlderRows(t *testing.T) {
	srv := lazyEpochServer()
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/memory/p1/epochs/older?epochs_limit=500", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	rows := regexp.MustCompile(`title="(epoch_\d{3})"`).FindAllStringSubmatch(body, -1)
	if len(rows) != 10 || rows[0][1] != "epoch_010" || rows[9][1] != "epoch_001" {
		t.Errorf("fragment rows: got %d (%v), want the 10 older, epoch_010 to epoch_001", len(rows), rows)
	}
	if got := strings.Count(body, `action="/ui/memory/p1/rollback"`); got != 10 {
		t.Errorf("fragment rollback forms: got %d, want 10", got)
	}
	if strings.Contains(body, "<html") || strings.Contains(body, "<table") {
		t.Error("the fragment is rows only, for the disclosure's tbody")
	}
}

func TestLazyEpochs_748M_FragmentIsScopedAndNeedsTheStore(t *testing.T) {
	srv := lazyEpochServer()
	req := sessionUserUIRequest(http.MethodGet, "/memory/beta/epochs/older", []string{"alpha"})
	if rec := servePath(srv, req); rec.Code != http.StatusNotFound {
		t.Errorf("a key scoped to another project: got %d, want 404", rec.Code)
	}
	bare := NewServer()
	rec := httptest.NewRecorder()
	bare.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/memory/p1/epochs/older", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("no epoch store: got %d, want 404", rec.Code)
	}
}

// Rollback targets straddle the newest-20 split (review-20261113-1e08 m2):
// with the active epoch at position 25, the page's 20 rows are newer than it
// (no targets) and the fragment's rows after it are targets. Both are
// computed over the whole loaded list.
func TestLazyEpochs_748M_TargetFlagsMatchAcrossTheSplit(t *testing.T) {
	eps := epochFixture(30)
	eps[0].IsActive = false
	eps[25].IsActive = true // epoch_005
	srv := NewServer(
		WithCorpusEpochRepository(&uiStubEpochRepo{listEpochsFn: func(context.Context, string, int) ([]*persistence.CorpusEpoch, error) { return eps, nil }}),
		WithMemoryQuarantineRepository(&uiStubQuarantineRepo{}),
	)
	page := httptest.NewRecorder()
	srv.MemoryProject(page, httptest.NewRequest(http.MethodGet, "/memory/p1?tab=operate", nil), "p1")
	if got := strings.Count(page.Body.String(), `action="/ui/memory/p1/rollback"`); got != 0 {
		t.Errorf("page: the newest 20 are newer than the active epoch, so none is a target; got %d forms", got)
	}
	frag := httptest.NewRecorder()
	srv.Handler().ServeHTTP(frag, httptest.NewRequest(http.MethodGet, "/memory/p1/epochs/older", nil))
	body := frag.Body.String()
	if got := strings.Count(body, `action="/ui/memory/p1/rollback"`); got != 4 {
		t.Errorf("fragment: epochs older than epoch_005 are targets (004 to 001); got %d forms", got)
	}
	for _, id := range []string{"epoch_004", "epoch_001"} {
		if !strings.Contains(body, `name="to" value="`+id+`"`) {
			t.Errorf("fragment: %s must carry its rollback form", id)
		}
	}
	if strings.Contains(body, `name="to" value="epoch_005"`) {
		t.Error("the active epoch itself is not a target")
	}
}

// With the active epoch inside the page's newest 20, a baseline computed
// over the fetched slice alone would flag nothing; over the whole list every
// older row is a target (review-20261113-0c18).
func TestLazyEpochs_748M_BaselineIsTheWholeList(t *testing.T) {
	eps := epochFixture(30)
	eps[0].IsActive = false
	eps[15].IsActive = true // epoch_015
	srv := NewServer(WithCorpusEpochRepository(&uiStubEpochRepo{listEpochsFn: func(context.Context, string, int) ([]*persistence.CorpusEpoch, error) { return eps, nil }}))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/memory/p1/epochs/older", nil))
	if got := strings.Count(rec.Body.String(), `action="/ui/memory/p1/rollback"`); got != 10 {
		t.Errorf("fragment rollback forms: got %d, want 10 (all older than epoch_015)", got)
	}
}

func TestLazyEpochs_748M_FragmentMethodAndStoreError(t *testing.T) {
	srv := lazyEpochServer()
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/memory/p1/epochs/older", nil))
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != http.MethodGet {
		t.Errorf("POST: got %d, Allow %q; want 405, GET", rec.Code, rec.Header().Get("Allow"))
	}
	failing := NewServer(WithCorpusEpochRepository(&uiStubEpochRepo{listEpochsFn: func(context.Context, string, int) ([]*persistence.CorpusEpoch, error) {
		return nil, fmt.Errorf("store down")
	}}))
	rec = httptest.NewRecorder()
	failing.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/memory/p1/epochs/older", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("store error: got %d, want 500 so the placeholder stays", rec.Code)
	}
}
