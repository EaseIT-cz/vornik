package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/persistence/sqlite"
	"vornik.io/vornik/internal/persistence/sqlite/sqlitetest"
)

// Knowledge-graph LLD §3.3b (2026-10-16, after GitHub #284). Found at:
// production — 1,760 chunks quarantined by the invalid-UTF-8 mention bug had
// no targeted recovery: `vornikctl memory regraph` re-flagged every edge-less
// chunk of a project and left the attempt counter at the bound. These tests
// drive POST /api/v1/memory/regraph/quarantined through the real router over
// a real SQLite repository.

const regraphQPath = "/api/v1/memory/regraph/quarantined"

// regraphQFixture seeds two projects with quarantined chunks: alpha holds one
// invalid-byte-sequence chunk and one other, beta one invalid-byte-sequence
// chunk; alpha also holds a cleanly extracted chunk that must never move.
func regraphQFixture(t *testing.T) (*Server, *sqlite.ChunkGraphExtractionRepository) {
	t.Helper()
	db := sqlitetest.Memory(t)
	repo := sqlite.NewChunkGraphExtractionRepository(db.DB)
	ctx := context.Background()
	at := time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)
	for i, c := range []struct{ id, project, reason string }{
		{"a-utf8", "alpha", `insert mention: pq: invalid byte sequence for encoding "UTF8": 0xe2`},
		{"a-other", "alpha", "insert mention for entity: not found"},
		{"b-utf8", "beta", `insert mention: pq: invalid byte sequence for encoding "UTF8": 0x80`},
		{"a-done", "alpha", ""},
	} {
		if _, err := db.ExecContext(ctx, `INSERT INTO project_memory_chunks
			(id, project_id, content, content_hash, needs_graph_extraction, created_at)
			VALUES (?, ?, ?, ?, 1, ?)`, c.id, c.project, "content "+c.id, "h-"+c.id,
			at.Add(time.Duration(i)*time.Minute).Format(time.RFC3339Nano)); err != nil {
			t.Fatalf("seed %s: %v", c.id, err)
		}
		if c.reason == "" {
			if err := repo.MarkExtracted(ctx, c.id); err != nil {
				t.Fatalf("MarkExtracted: %v", err)
			}
			continue
		}
		for n := 0; n < 3; n++ {
			if _, _, err := repo.RecordExtractionFailure(ctx, c.id, c.reason, 3); err != nil {
				t.Fatalf("RecordExtractionFailure: %v", err)
			}
		}
	}
	s := &Server{logger: zerolog.Nop(), memoryGraphReflagger: repo}
	return s, repo
}

func regraphQDo(t *testing.T, s *Server, method, query string, scoped []string) (*httptest.ResponseRecorder, MemoryGraphQuarantineReflagResult) {
	t.Helper()
	req := httptest.NewRequest(method, regraphQPath+query, nil)
	if scoped != nil {
		ctx := context.WithValue(req.Context(), authEnabledKey, true)
		ctx = context.WithValue(ctx, projectIDKey, scoped)
		req = req.WithContext(ctx)
	}
	rec := httptest.NewRecorder()
	s.MemoryRegraphQuarantined(rec, req)
	var out MemoryGraphQuarantineReflagResult
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode %s: %v", rec.Body.String(), err)
		}
	}
	return rec, out
}

func regraphQRow(t *testing.T, res MemoryGraphQuarantineReflagResult, project string) MemoryGraphQuarantineReflagRow {
	t.Helper()
	for _, r := range res.Projects {
		if r.Project == project {
			return r
		}
	}
	t.Fatalf("no row for %s in %+v", project, res.Projects)
	return MemoryGraphQuarantineReflagRow{}
}

func TestMemoryRegraphQuarantined_RouteIsRegistered(t *testing.T) {
	s, _ := regraphQFixture(t)
	router := NewRouter(s, &config.Config{})
	req := httptest.NewRequest(http.MethodPost, regraphQPath+"?dry_run=true", nil)
	rec := httptest.NewRecorder()
	router.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST %s through the router: %d %s", regraphQPath, rec.Code, rec.Body.String())
	}
}

func TestMemoryRegraphQuarantined_DryRunReportsDenominatorAndWritesNothing(t *testing.T) {
	s, _ := regraphQFixture(t)
	rec, res := regraphQDo(t, s, http.MethodPost, "?dry_run=true&error_contains=invalid+byte+sequence", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if !res.DryRun || res.ErrorContains != "invalid byte sequence" {
		t.Errorf("echo: dryRun=%t errorContains=%q", res.DryRun, res.ErrorContains)
	}
	if a := regraphQRow(t, res, "alpha"); a.Quarantined != 2 || a.Matched != 1 || a.ReFlagged != 0 {
		t.Errorf("alpha = %+v, want 2/1/0", a)
	}
	if b := regraphQRow(t, res, "beta"); b.Quarantined != 1 || b.Matched != 1 || b.ReFlagged != 0 {
		t.Errorf("beta = %+v, want 1/1/0", b)
	}
	if res.Totals.Quarantined != 3 || res.Totals.Matched != 2 || res.Totals.ReFlagged != 0 {
		t.Errorf("totals = %+v, want 3/2/0", res.Totals)
	}
	// Nothing written: a second dry run reads the same.
	_, again := regraphQDo(t, s, http.MethodPost, "?dry_run=true&error_contains=invalid+byte+sequence", nil)
	if again.Totals != res.Totals {
		t.Errorf("a dry run changed state: %+v then %+v", res.Totals, again.Totals)
	}
}

func TestMemoryRegraphQuarantined_CommitReflagsMatchedOnlyAndIsIdempotent(t *testing.T) {
	s, repo := regraphQFixture(t)
	rec, res := regraphQDo(t, s, http.MethodPost, "?project=alpha&error_contains=invalid+byte+sequence", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if a := regraphQRow(t, res, "alpha"); a.Quarantined != 2 || a.Matched != 1 || a.ReFlagged != 1 {
		t.Errorf("alpha = %+v, want 2/1/1", a)
	}
	if len(res.Projects) != 1 {
		t.Errorf("a project-scoped call reported %d projects: %+v", len(res.Projects), res.Projects)
	}
	rows, err := repo.FetchUnextracted(context.Background(), 50)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, r := range rows {
		got[r.ID] = true
	}
	if !got["a-utf8"] || got["a-other"] || got["b-utf8"] || got["a-done"] {
		t.Errorf("re-flagged set = %v, want only a-utf8", got)
	}
	_, again := regraphQDo(t, s, http.MethodPost, "?project=alpha&error_contains=invalid+byte+sequence", nil)
	if again.Totals.ReFlagged != 0 {
		t.Errorf("re-run re-flagged %d chunks, want 0", again.Totals.ReFlagged)
	}
}

func TestMemoryRegraphQuarantined_Authorisation(t *testing.T) {
	s, _ := regraphQFixture(t)
	// All projects with a project-scoped key: refused, nothing written.
	rec, _ := regraphQDo(t, s, http.MethodPost, "?error_contains=invalid", []string{"alpha"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("scoped key, all projects: %d %s", rec.Code, rec.Body.String())
	}
	// A project outside the key's scope: refused.
	if rec, _ := regraphQDo(t, s, http.MethodPost, "?project=beta", []string{"alpha"}); rec.Code != http.StatusForbidden {
		t.Fatalf("scoped key, other project: %d", rec.Code)
	}
	// Its own project: allowed.
	rec, res := regraphQDo(t, s, http.MethodPost, "?project=alpha&dry_run=true", []string{"alpha"})
	if rec.Code != http.StatusOK || regraphQRow(t, res, "alpha").Quarantined != 2 {
		t.Fatalf("scoped key, own project: %d %s", rec.Code, rec.Body.String())
	}
	// The refused all-projects commit wrote nothing.
	_, all := regraphQDo(t, s, http.MethodPost, "?dry_run=true", nil)
	if all.Totals.Quarantined != 3 {
		t.Errorf("a refused call changed state: %+v", all.Totals)
	}
}

func TestMemoryRegraphQuarantined_MethodAndWiring(t *testing.T) {
	s, _ := regraphQFixture(t)
	if rec, _ := regraphQDo(t, s, http.MethodGet, "", nil); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET: %d", rec.Code)
	}
	unwired := &Server{logger: zerolog.Nop()}
	if rec, _ := regraphQDo(t, unwired, http.MethodPost, "", nil); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("unwired: %d", rec.Code)
	}
}
