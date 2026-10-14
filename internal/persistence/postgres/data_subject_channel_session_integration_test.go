//go:build integration

package postgres

import (
	"context"
	"strings"
	"testing"

	"vornik.io/vornik/internal/datasubject"
)

// channel_sessions in the Art 15/20 export, against a real database.
//
// audit 2026-10-12: channel_sessions.history was neither exported nor erased;
// the collector read only active_project, keyed on session_id although the
// primary key is (kind, session_id). Design
// 2026-07-29-art17-redact-and-reembed §15.10, GitHub #344.

func resetChannelSessionFixture(t *testing.T, db *DB) {
	t.Helper()
	ctx := context.Background()
	for _, stmt := range []string{
		`DELETE FROM data_subject_links WHERE subject_id LIKE 'ds_w42_%'`,
		`DELETE FROM data_subjects WHERE id LIKE 'ds_w42_%'`,
		`DELETE FROM channel_sessions WHERE session_id LIKE 'w42-%'`,
	} {
		if _, err := db.DB.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("reset: %s: %v", stmt, err)
		}
	}
}

func seedSession(t *testing.T, db *DB, kind, sid, history string) {
	t.Helper()
	if _, err := db.DB.ExecContext(context.Background(),
		`INSERT INTO channel_sessions (kind, session_id, active_project, history)
		 VALUES ($1, $2, 'proj', $3::jsonb)`, kind, sid, history); err != nil {
		t.Fatalf("seed session %s/%s: %v", kind, sid, err)
	}
}

func channelSessionRepo(t *testing.T) (*DB, *DataSubjectRepository) {
	t.Helper()
	db := newIntegrationDB(t)
	resetChannelSessionFixture(t, db)
	t.Cleanup(func() { resetChannelSessionFixture(t, db) })
	repo := NewDataSubjectRepository(db.DB)
	if err := repo.CreateSubject(context.Background(), datasubject.Subject{ID: "ds_w42_a", DisplayName: "Jane Doe"}); err != nil {
		t.Fatalf("subject: %v", err)
	}
	return db, repo
}

func sessionLink(rowID string) datasubject.Link {
	return datasubject.Link{
		Table: datasubject.TableChannelSessions, RowID: rowID,
		Source: datasubject.SourceOperatorAsserted, Confidence: datasubject.ConfidenceCertain,
		Exclusivity: datasubject.ExclusiveRow,
	}
}

// Two kinds share one session id: the export reads the linked kind's history
// and no other.
func TestIntegrationChannelSession_ExportReadsTheLinkedKindsHistory(t *testing.T) {
	db, repo := channelSessionRepo(t)
	seedSession(t, db, "webchat", "w42-same", `[{"role":"user","content":"jane webchat turn"}]`)
	seedSession(t, db, "slack", "w42-same", `[{"role":"user","content":"someone else's slack turn"}]`)

	items, err := repo.CollectItems(context.Background(), "ds_w42_a",
		[]datasubject.Link{sessionLink("webchat/w42-same")})
	if err != nil {
		t.Fatalf("CollectItems: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("want one item, got %+v", items)
	}
	if !strings.Contains(items[0].Content, "jane webchat turn") || strings.Contains(items[0].Content, "slack turn") {
		t.Errorf("export must carry the linked session's history only, got %q", items[0].Content)
	}
	// Art 20: the history is exported as its structured JSON, not flattened.
	if !strings.Contains(items[0].Content, `"role": "user"`) {
		t.Errorf("the history must be exported as JSON, got %q", items[0].Content)
	}
	if items[0].Exclusivity != datasubject.ExclusiveRow {
		t.Errorf("a webchat session linked to one subject is exclusive, got %s", items[0].Exclusivity)
	}
	if !items[0].ProvidedBySubject {
		t.Error("a session history holds the subject's own messages; it is in Art 20 scope")
	}
}

// A multi-party kind is shared whatever the link says (Art 15(4)), so the
// export withholds its content.
func TestIntegrationChannelSession_MultiPartyKindIsWithheldAsShared(t *testing.T) {
	db, repo := channelSessionRepo(t)
	seedSession(t, db, "slack", "w42-thread", `[{"role":"user","content":"From: bob\n\nbob's words"}]`)
	items, err := repo.CollectItems(context.Background(), "ds_w42_a",
		[]datasubject.Link{sessionLink("slack/w42-thread")})
	if err != nil {
		t.Fatalf("CollectItems: %v", err)
	}
	if len(items) != 1 || items[0].Exclusivity != datasubject.SharedRow {
		t.Fatalf("a slack session must be treated as shared, got %+v", items)
	}
	// export.go withholds the content of every TreatAsShared item; that rule is
	// tested there.
	if !items[0].Exclusivity.TreatAsShared() {
		t.Error("the exporter must withhold this item")
	}
}

// A legacy bare session id resolves when unambiguous and is refused, never
// silently picked, when several kinds hold it.
func TestIntegrationChannelSession_LegacyBareIDResolvesOnlyWhenUnambiguous(t *testing.T) {
	db, repo := channelSessionRepo(t)
	ctx := context.Background()
	seedSession(t, db, "webchat", "w42-unique", `[{"role":"user","content":"only one"}]`)
	// Legacy links predate Link.Validate's form check; CollectItems takes the
	// stored link as it is.
	legacy := func(id string) []datasubject.Link { return []datasubject.Link{sessionLink(id)} }
	items, err := repo.CollectItems(ctx, "ds_w42_a", legacy("w42-unique"))
	if err != nil || len(items) != 1 || !strings.Contains(items[0].Content, "only one") {
		t.Fatalf("an unambiguous legacy id must resolve: items=%+v err=%v", items, err)
	}

	// A legacy bare GitHub id contains "/" but does not parse as <kind>/...
	// ("w42-o" is not a kind), so it falls back to the bare-id lookup.
	seedSession(t, db, "github", "w42-o/repo#issues/1", `[{"role":"user","content":"gh turn"}]`)
	items, err = repo.CollectItems(ctx, "ds_w42_a", legacy("w42-o/repo#issues/1"))
	if err != nil || len(items) != 1 || !strings.Contains(items[0].Content, "gh turn") {
		t.Fatalf("a legacy bare id with a slash must resolve through the fallback: items=%+v err=%v", items, err)
	}

	// No row with that bare id any more: the link is skipped, not an error.
	items, err = repo.CollectItems(ctx, "ds_w42_a", legacy("w42-gone"))
	if err != nil || len(items) != 0 {
		t.Fatalf("a legacy id with no row must be skipped: items=%+v err=%v", items, err)
	}

	seedSession(t, db, "webchat", "w42-dup", `[]`)
	seedSession(t, db, "slack", "w42-dup", `[]`)
	if _, err := repo.CollectItems(ctx, "ds_w42_a", legacy("w42-dup")); err == nil ||
		!strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("an ambiguous legacy id must be refused, got %v", err)
	}
}
