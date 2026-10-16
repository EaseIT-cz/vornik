package memory

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
)

// GitHub #389 (memory-ingest-paths.md §8.2a): the text-damage predicate. A
// chunk held control bytes and invalid UTF-8 from a base64 upload a model had
// transcribed; nothing on the ingest path looked at the text.

func TestScanTextDamage_CleanTextIsNotDamaged(t *testing.T) {
	for name, s := range map[string]string{
		"empty":          "",
		"ascii":          "plain prose.\n",
		"whitespace":     "tab\there\r\nnew line\n\fpage two",
		"unicode":        "Příliš žluťoučký kůň — ⟦vm:abc⟧",
		"replacement":    "a legitimate � quoted in a design doc",
		"del-is-not-c0":  "x\x7fy", // DEL is not C0; the issue's class is C0
		"emoji-4-byte":   "ok 🙂",
		"formfeed-pages": "page 1\fpage 2\f",
	} {
		t.Run(name, func(t *testing.T) {
			d := ScanTextDamage(s)
			if d.Damaged() {
				t.Fatalf("clean text reported damaged: %+v", d)
			}
			if d.FirstOffset != -1 {
				t.Errorf("FirstOffset = %d; want -1 for clean text", d.FirstOffset)
			}
		})
	}
}

func TestScanTextDamage_CountsEachClassAndTheFirstOffset(t *testing.T) {
	cases := []struct {
		name       string
		in         string
		controls   int
		invalid    int
		firstOffst int
	}{
		{"vertical tab", "ab\x0bc", 1, 0, 2},
		{"NUL", "\x00abc", 1, 0, 0},
		{"ESC (ANSI)", "ok \x1b[31mred", 1, 0, 3},
		{"two controls", "a\x01b\x1f", 2, 0, 1},
		{"lone continuation byte", "abc\x80d", 0, 1, 3},
		{"truncated rune", "ab\xc3", 0, 1, 2},
		{"mixed, invalid first", "\xffa\x02", 1, 1, 0},
		// The incident's own bytes: "telemety.vornik.io" then the 6-bit-shifted tail.
		{"incident 277f4def", "Example: https://telemety.vornik.io\x0b\xdd\x8cK\xd8\xdb\xdb\x1b\x19X", 3, 3, 35},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := ScanTextDamage(tc.in)
			if !d.Damaged() {
				t.Fatalf("damaged text reported clean: %q", tc.in)
			}
			if d.Controls != tc.controls {
				t.Errorf("Controls = %d; want %d", d.Controls, tc.controls)
			}
			if d.InvalidUTF8 != tc.invalid {
				t.Errorf("InvalidUTF8 = %d; want %d", d.InvalidUTF8, tc.invalid)
			}
			if d.FirstOffset != tc.firstOffst {
				t.Errorf("FirstOffset = %d; want %d", d.FirstOffset, tc.firstOffst)
			}
		})
	}
}

func TestTextDamage_ErrWrapsErrDamagedTextAndNamesTheOffset(t *testing.T) {
	err := ScanTextDamage("ab\x0bc").Err("design.md")
	if !errors.Is(err, ErrDamagedText) {
		t.Fatalf("Err must wrap ErrDamagedText; got %v", err)
	}
	for _, want := range []string{"design.md", "offset 2", "1 control"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q must name %q", err, want)
		}
	}
	if ScanTextDamage("clean").Err("x") != nil {
		t.Error("clean text must yield a nil error")
	}
}

// The structural floor (§8.2b): the two writers of project_memory_chunks
// refuse a damaged row before touching the database, so a future caller that
// skips the Indexer entry points still cannot store one. A lazily-opened
// *sql.DB that was never reachable proves no statement ran: reaching the DB
// would fail with a connection error, not ErrDamagedText.
func TestStorageFloor_WritersRefuseDamagedRowsBeforeTheDatabase(t *testing.T) {
	db, err := sql.Open("postgres", "host=127.0.0.1 port=1 dbname=none sslmode=disable connect_timeout=1")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = db.Close() }()
	repo := NewRepository(db)
	ctx := context.Background()

	err = repo.UpsertChunks(ctx, []MemoryChunk{
		{ID: "c1", ProjectID: "p", SourceName: "ok", Content: "clean"},
		{ID: "c2", ProjectID: "p", SourceName: "telemetry · 2.", Content: "Example: https://telemety.vornik.io\x0b\xdd\x8cK"},
	})
	if !errors.Is(err, ErrDamagedText) {
		t.Errorf("UpsertChunks must refuse the whole batch with ErrDamagedText; got %v", err)
	}

	err = repo.insertOperatorCorrection(ctx, &operatorCorrectionRow{
		ID: "c3", ProjectID: "p", SourceName: "correction", Content: "fix\x00ed", ContentHash: "h",
	})
	if !errors.Is(err, ErrDamagedText) {
		t.Errorf("insertOperatorCorrection must refuse damaged text with ErrDamagedText; got %v", err)
	}
}

func TestTextDamageGate_QuarantinesDamagedAndAllowsClean(t *testing.T) {
	bad := &IngestCandidate{Content: "Example\x0b\xdd\x8c"}
	if out := TextDamageGate(bad); out.Action != GateQuarantine || out.Gate != GateTextDamage {
		t.Fatalf("damaged candidate: got %+v; want quarantine at text_damage", out)
	}
	good := &IngestCandidate{Content: "clean\ttext\n"}
	if out := TextDamageGate(good); out.Action != GateAllow {
		t.Fatalf("clean candidate: got %+v; want allow", out)
	}
}

func TestRunStandardGates_TextDamageQuarantinesBeforeLaterGates(t *testing.T) {
	c := &IngestCandidate{
		ProjectID:        "p",
		SourceArtifactID: "a",
		ProducerRole:     "writer",
		Content:          strings.Repeat("real prose words here. ", 20) + "\x0b\xdd\x8cK",
	}
	final, trail := RunStandardGates(c, DefaultGateConfig(), nil, 0, nil)
	if final.Action != GateQuarantine || final.Gate != GateTextDamage {
		t.Fatalf("final = %+v; want quarantine at text_damage", final)
	}
	if len(trail) != 2 || trail[0].Gate != GateSchemaMatch {
		t.Errorf("text_damage must run right after schema_match; trail = %+v", trail)
	}
}
