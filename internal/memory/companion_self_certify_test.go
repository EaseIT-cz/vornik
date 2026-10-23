package memory

import (
	"context"
	"database/sql/driver"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// GitHub #435 (security audit 2026-10-17): a companion `remember` with
// class "decision" or "spec" minted a chunk with validation_status
// 'verified', validator_role = the companion itself, the class's full
// confidence and no expiry, because the role-of-record shortcut keyed on the
// class alone. Design 22, "A companion deposit never certifies itself". The
// behavioural regression test is at the MCP tool level on the pgvector lane
// (test/integration/companion_remember_role_of_record_test.go); these pin the
// policy table and the pipeline on the SQLite lane.

func TestEffectiveClassPolicy(t *testing.T) {
	note := DefaultClassPolicies[ClassCompanionNote]
	for _, tc := range []struct {
		name     string
		class    ContentClass
		producer string
		conf     float32
		ttl      time.Duration
		rok      bool
	}{
		// Agent roles keep the class policy unchanged.
		{"agent decision", ClassDecision, "reviewer", 0.9, 0, true},
		{"agent spec", ClassSpec, "rag-ingester", 0.8, 0, true},
		{"agent research", ClassResearch, "researcher", 0.6, 90 * 24 * time.Hour, false},
		// A companion deposit never carries a class's authority.
		{"companion decision", ClassDecision, "companion:claude-code", note.DefaultConfidence, note.TTL, false},
		{"companion spec", ClassSpec, "companion:codex", note.DefaultConfidence, note.TTL, false},
		{"companion research keeps its finite TTL", ClassResearch, "companion:hermes", note.DefaultConfidence, 90 * 24 * time.Hour, false},
		{"companion diagnostic keeps the lower confidence", ClassDiagnostic, "companion:claude-code", 0.2, 7 * 24 * time.Hour, false},
		{"companion class with no policy entry", ContentClass("incident"), "companion:claude-code", 0, note.TTL, false},
		{"companion note", ClassCompanionNote, "companion:claude-code", note.DefaultConfidence, note.TTL, false},
		// A chat deposit is the same kind of producer.
		{"chat decision", ClassDecision, "chat:slack", note.DefaultConfidence, note.TTL, false},
		{"chat memory unchanged", ClassChatMemory, "chat:telegram", 0.3, 90 * 24 * time.Hour, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := EffectiveClassPolicy(tc.class, tc.producer)
			if got.DefaultConfidence != tc.conf || got.TTL != tc.ttl || got.RoleOfRecordEligible != tc.rok {
				t.Errorf("EffectiveClassPolicy(%q, %q) = %+v; want confidence %v ttl %v role-of-record %v",
					tc.class, tc.producer, got, tc.conf, tc.ttl, tc.rok)
			}
		})
	}
}

// capture records the value sqlmock binds to one placeholder.
type capture struct{ v driver.Value }

func (c *capture) Match(v driver.Value) bool { c.v = v; return true }

func TestIngestCompanionNote_DecisionClassIsNotVerified(t *testing.T) {
	for _, class := range []ContentClass{ClassDecision, ClassSpec} {
		t.Run(string(class), func(t *testing.T) {
			p, _, _, mock, cleanup := newPipelineTestRig(t)
			defer cleanup()
			mock.ExpectExec("INSERT INTO project_memory_chunks").WillReturnResult(sqlmock.NewResult(0, 1))
			mock.ExpectExec("INSERT INTO memory_embed_queue").WillReturnResult(sqlmock.NewResult(0, 1))
			conf, expires := &capture{}, &capture{}
			mock.ExpectExec("SET content_class").
				WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), string(class), conf, "companion:claude-code",
					sqlmock.AnyArg(), expires, sqlmock.AnyArg()).
				WillReturnResult(sqlmock.NewResult(0, 1))
			// Before the fix the role-of-record shortcut issued this
			// statement and the deposit counted as Verified.
			mock.ExpectExec("SET validation_status = 'verified'").WillReturnResult(sqlmock.NewResult(0, 1))

			got, err := p.IngestCompanionNote(context.Background(), "companion-example", "claude-code", "akey-mem", "",
				"We decided to drop the pgvector lane from the release gates entirely.", class, 0, "", time.Time{})
			if err != nil {
				t.Fatalf("IngestCompanionNote: %v", err)
			}
			if got.Stats.Admitted != 1 {
				t.Fatalf("not admitted: %+v", got.Stats)
			}
			if got.Stats.Verified != 0 {
				t.Errorf("a companion %s deposit was stamped verified (Verified=%d)", class, got.Stats.Verified)
			}
			if c, ok := conf.v.(float64); !ok || c > 0.3+1e-6 {
				t.Errorf("confidence bound = %v (%T); want <= 0.3", conf.v, conf.v)
			}
			e, ok := expires.v.(time.Time)
			if !ok {
				t.Fatalf("expires_at bound = %v (%T); want a time (never NULL for a companion deposit)", expires.v, expires.v)
			}
			if d := time.Until(e); d < 29*24*time.Hour || d > 31*24*time.Hour {
				t.Errorf("expires_at in %v; want about 30 days", d)
			}
			if err := mock.ExpectationsWereMet(); err == nil || !strings.Contains(err.Error(), "verified") {
				t.Errorf("the role-of-record statement ran; want it never issued (err=%v)", err)
			}
		})
	}
}

// A deliberate human deposit that reaches the classify backfill (a chat:
// producer has no role-map entry, so the LLM places it) and is classified
// spec/decision keeps a finite expiry: the backfill reads the producer-aware
// policy, not the class table (GitHub #435).
func TestClassifyBackfill_HumanDepositClassifiedDecisionStillExpires(t *testing.T) {
	bf, mock, cleanup := newClassifyBackfiller(t, []titlerReply{{content: "decision"}})
	defer cleanup()
	mock.ExpectQuery("FROM project_memory_chunks").
		WithArgs("p", 10, 0).
		WillReturnRows(sqlmock.NewRows([]string{"id", "project_id", "source_name", "producer_role", "content"}).
			AddRow("c1", "p", "chat:slack:s1", "chat:slack", "we decided to move the release to friday"))
	want := int64(DefaultClassPolicies[ClassCompanionNote].TTL.Seconds())
	mock.ExpectExec("UPDATE project_memory_chunks").WithArgs("c1", "decision", want).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("FROM project_memory_chunks").
		WillReturnRows(sqlmock.NewRows([]string{"role", "n"}).AddRow("chat:slack", 0))
	res, err := bf.BackfillBatch(context.Background(), "p", 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.Succeeded != 1 {
		t.Fatalf("want the row persisted with a %ds TTL; got %+v", want, res)
	}
}
