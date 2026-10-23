package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/persistence/sqlite"
	"vornik.io/vornik/internal/persistence/sqlite/sqlitetest"
)

// Knowledge-graph LLD §3.3b (2026-10-16, after GitHub #284). Found at:
// production — 1,760 chunks were quarantined by the invalid-UTF-8 mention bug
// and `vornikctl memory regraph` could not target them: it re-flagged every
// edge-less chunk of one project and left the attempt counter at the bound.
// Production is not reachable from a test, so this drives the operator's
// command against the real API routes over HTTP and a real migrated SQLite
// database, the highest level a test reaches.

func seedQuarantined(t *testing.T, db *sqlite.DB, repo *sqlite.ChunkGraphExtractionRepository) {
	t.Helper()
	ctx := context.Background()
	at := time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)
	for i, c := range []struct{ id, project, reason string }{
		{"a-utf8", "alpha", `insert mention: pq: invalid byte sequence for encoding "UTF8": 0xe2`},
		{"a-other", "alpha", "insert mention for entity: not found"},
		{"b-utf8", "beta", `insert mention: pq: invalid byte sequence for encoding "UTF8": 0x80`},
	} {
		if _, err := db.ExecContext(ctx, `INSERT INTO project_memory_chunks
			(id, project_id, content, content_hash, needs_graph_extraction, created_at)
			VALUES (?, ?, ?, ?, 1, ?)`, c.id, c.project, "content "+c.id, "h-"+c.id,
			at.Add(time.Duration(i)*time.Minute).Format(time.RFC3339Nano)); err != nil {
			t.Fatalf("seed %s: %v", c.id, err)
		}
		for n := 0; n < 3; n++ {
			if _, _, err := repo.RecordExtractionFailure(ctx, c.id, c.reason, 3); err != nil {
				t.Fatalf("RecordExtractionFailure: %v", err)
			}
		}
	}
}

func TestMemoryRegraphQuarantined_CLIAgainstRealAPIAndDB(t *testing.T) {
	db := sqlitetest.Open(t, filepath.Join(t.TempDir(), "vornik.db"))
	repo := sqlite.NewChunkGraphExtractionRepository(db.DB)
	seedQuarantined(t, db, repo)
	srv := httptest.NewServer(api.NewServer(
		api.WithLogger(zerolog.Nop()),
		api.WithMemoryGraphReflagger(repo),
	).Routes())
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	t.Setenv("VORNIK_API_KEY", "test-key")

	pending := func() map[string]bool {
		t.Helper()
		rows, err := repo.FetchUnextracted(context.Background(), 50)
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]bool{}
		for _, r := range rows {
			got[r.ID] = true
		}
		return got
	}

	t.Run("dry run over every project prints the denominator and writes nothing", func(t *testing.T) {
		var out bytes.Buffer
		if err := runMemoryRegraphQuarantined("", "invalid byte sequence", true, false, &out); err != nil {
			t.Fatal(err)
		}
		s := out.String()
		for _, want := range []string{"alpha", "beta", "would re-flag 2 of 3 quarantined chunks", "dry-run"} {
			if !strings.Contains(s, want) {
				t.Errorf("output lacks %q:\n%s", want, s)
			}
		}
		if len(pending()) != 0 {
			t.Errorf("a dry run re-flagged chunks: %v", pending())
		}
	})

	t.Run("commit for one project re-flags only its matched chunk", func(t *testing.T) {
		var out bytes.Buffer
		if err := runMemoryRegraphQuarantined("alpha", "invalid byte sequence", false, false, &out); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), "re-flagged 1 of 2 quarantined chunks") {
			t.Errorf("output:\n%s", out.String())
		}
		got := pending()
		if !got["a-utf8"] || got["a-other"] || got["b-utf8"] {
			t.Errorf("re-flagged set = %v, want only a-utf8", got)
		}
	})

	t.Run("json output carries the per-project rows", func(t *testing.T) {
		var out bytes.Buffer
		if err := runMemoryRegraphQuarantined("", "invalid byte sequence", false, true, &out); err != nil {
			t.Fatal(err)
		}
		var res api.MemoryGraphQuarantineReflagResult
		if err := json.Unmarshal(out.Bytes(), &res); err != nil {
			t.Fatalf("decode %q: %v", out.String(), err)
		}
		if res.Totals.ReFlagged != 1 || res.Totals.Quarantined != 2 || res.Totals.Matched != 1 {
			t.Errorf("totals = %+v, want quarantined 2 (a-other, b-utf8), matched 1, reflagged 1", res.Totals)
		}
		if !pending()["b-utf8"] {
			t.Error("the all-projects commit did not re-flag beta's chunk")
		}
	})
}

func TestMemoryRegraph_FlagValidation(t *testing.T) {
	cases := []struct {
		name              string
		project, contains string
		quarantined       bool
		wantErrSubstring  string
	}{
		{"edge-less regraph still needs a project", "", "", false, "--project is required"},
		{"error-contains needs quarantined", "p", "x", false, "--error-contains needs --quarantined"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validateRegraphFlags(c.project, c.contains, c.quarantined)
			if err == nil || !strings.Contains(err.Error(), c.wantErrSubstring) {
				t.Errorf("err = %v, want %q", err, c.wantErrSubstring)
			}
		})
	}
	if err := validateRegraphFlags("", "invalid byte sequence", true); err != nil {
		t.Errorf("--quarantined without a project is the all-projects mode: %v", err)
	}
}
