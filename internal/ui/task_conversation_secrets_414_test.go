package ui

// GitHub #414 (2), found by this lane's own adversarial pass over the writers:
// the UI's task-message forms take the author from a form field, which the
// caller types, and stored it unbounded; and uiPostMessage cut the content to
// 64 KiB with a byte slice BEFORE the seam scanned it, so a credential
// straddling the cut was stored as a head too short for its pattern (the #395
// bug class, left in the UI writer). Found at: review. A user observes it at
// integration: the stored row. The real UI handler writes through the decorator
// the container installs into real SQLite.
// Design: chat-audit retention and redaction design §10.3.

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"vornik.io/vornik/internal/auditredact"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite"
	"vornik.io/vornik/internal/persistence/sqlite/sqlitetest"
	"vornik.io/vornik/internal/secrets"
)

const (
	gh414ui  = "ghp_R7mK2pX9vQ4tL8wZ3nB6cJ1hF5yD0sGaE2uT"
	aws414ui = "AKIAQ3EGUZJ7RLW4XN2P"
)

func uiSecretsRig414(t *testing.T) (*Server, *sql.DB, *persistence.Task) {
	t.Helper()
	db := sqlitetest.Memory(t)
	det, err := secrets.NewMultiDetector(secrets.Config{Patterns: secrets.EffectivePatterns(nil, nil), Allowlist: secrets.DefaultAllowlist()})
	if err != nil {
		t.Fatal(err)
	}
	tasks := sqlite.NewTaskRepository(db.DB)
	wf := "wf"
	task := &persistence.Task{ID: "t414", ProjectID: "p414", WorkflowID: &wf, Status: persistence.TaskStatusRunning,
		Payload: []byte(`{}`), CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	if err := tasks.Create(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	msgs := auditredact.NewTaskMessages(sqlite.NewTaskMessageRepository(db.DB), det, nil, nil)
	return NewServer(WithTaskRepository(tasks), WithTaskMessageRepository(msgs)), db.DB, task
}

func noFragmentUI414(t *testing.T, where, stored, secret string) {
	t.Helper()
	for i := 0; i+10 <= len(secret); i++ {
		if strings.Contains(stored, secret[i:i+10]) {
			t.Fatalf("%s stores a fragment of the secret (%q)", where, secret[i:i+10])
		}
	}
}

func TestUIPostMessage414_AuthorAndCutAreSafe(t *testing.T) {
	srv, db, task := uiSecretsRig414(t)
	// The token starts before the 64 KiB cap and ends after it.
	content := strings.Repeat("a", uiInspectMaxBytes-20) + " " + gh414ui + " tail"
	form := url.Values{"content": {content}, "author": {"ops-" + aws414ui + strings.Repeat(" x", 100)}}
	req := httptest.NewRequest("POST", "/", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if got := srv.uiPostMessage(context.Background(), task, req, persistence.TaskMessageKindMessage, false); got == "message-failed" {
		t.Fatalf("uiPostMessage = %q", got)
	}

	var stored string
	var authorCol, meta sql.NullString
	if err := db.QueryRow(`SELECT content, author_id, metadata FROM task_messages WHERE task_id = $1`, task.ID).Scan(&stored, &authorCol, &meta); err != nil {
		t.Fatal(err)
	}
	// GitHub #432: the form's author is only the client-supplied label in
	// metadata (no principal on this request, so author_id stays empty); the
	// #414 bound and scan apply to the label.
	if authorCol.String != "" {
		t.Fatalf("#432: the form's author must not be the author of record: %q", authorCol.String)
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(meta.String), &m); err != nil {
		t.Fatalf("metadata %q: %v", meta.String, err)
	}
	author := sql.NullString{String: m["clientSuppliedAuthor"], Valid: true}
	noFragmentUI414(t, "content", stored, gh414ui)
	if !utf8.ValidString(stored) || len(stored) > uiInspectMaxBytes {
		t.Fatalf("content: %d bytes, valid=%v", len(stored), utf8.ValidString(stored))
	}
	noFragmentUI414(t, "author_id", author.String, aws414ui)
	// The bound runs at the writer, before the seam's scan, so a marker that
	// is longer than the finding it replaces may add a few bytes.
	if len(author.String) > auditredact.IdentityLimit+len("[REDACTED:aws_access_key]") || !strings.HasPrefix(author.String, "ops-") {
		t.Fatalf("author_id must be bounded with its handle kept: %q", author.String)
	}
}
