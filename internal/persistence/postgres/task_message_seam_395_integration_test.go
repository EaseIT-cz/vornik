//go:build integration

package postgres

// GitHub #395 (found at review, #344 security follow-up; observed at
// integration): task_messages had no write-time secret scan. The SQLite lane
// covers the writers (internal/api transcript_secrets_395_test.go); this is the
// Postgres lane, because task_messages.metadata is jsonb here — the seam's
// re-encoded metadata must be accepted and round-trip, the taint latch must
// still parse, and clean metadata must not be rewritten
// (chat-audit retention and redaction design §9.2.3, §9.2.8).

import (
	"context"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/auditredact"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/secrets"
	"vornik.io/vornik/internal/taintlineage"
)

func TestTaskMessageSeam395_PostgresJSONB(t *testing.T) {
	db := newIntegrationDB(t)
	ctx := context.Background()
	det, err := secrets.NewMultiDetector(secrets.Config{Patterns: secrets.EffectivePatterns(nil, nil), Allowlist: secrets.DefaultAllowlist()})
	if err != nil {
		t.Fatal(err)
	}
	const (
		gh   = "ghp_R7mK2pX9vQ4tL8wZ3nB6cJ1hF5yD0sGaE2uT"
		pass = "correct-horse-battery-staple-395"
	)
	task := &persistence.Task{ID: uniqueRaceID("task"), ProjectID: uniqueRaceID("proj"), Priority: 50,
		Payload: []byte(`{}`), Status: persistence.TaskStatusRunning}
	if err := NewTaskRepository(db.DB).Create(ctx, task); err != nil {
		t.Fatalf("Create task: %v", err)
	}
	repo := auditredact.NewTaskMessages(NewTaskMessageRepository(db.DB), det, nil, nil)

	hash := strings.Repeat("0123456789abcdef", 4)
	// GitHub #414: author_id is the REST caller's string; the seam scans it.
	author := "ops-" + gh
	latch := &persistence.TaskMessage{TaskID: task.ID, AuthorKind: persistence.TaskMessageAuthorSystem, AuthorID: &author,
		MessageKind: persistence.TaskMessageKindSystem, Content: "use " + gh, CreatedAt: time.Now().UTC(),
		Metadata: []byte(`{"kind":"` + taintlineage.LatchMarkerKind + `","source_set_hash":"` + hash +
			`","sources":["https://x.test/q?api_key=` + pass + `"],"nested":{"password":"` + pass + `"},"n":12345678901234567890}`)}
	if err := repo.Insert(ctx, latch); err != nil {
		t.Fatalf("Insert (jsonb must accept the re-encoded metadata): %v", err)
	}
	if latch.ID == "" {
		t.Fatal("the decorator must copy the assigned id back")
	}
	clean := &persistence.TaskMessage{TaskID: task.ID, AuthorKind: persistence.TaskMessageAuthorSystem,
		MessageKind: persistence.TaskMessageKindNote, Content: "fine", CreatedAt: time.Now().UTC(),
		Metadata: []byte(`{"reason":"moved","from":"RUNNING"}`)}
	if err := repo.Insert(ctx, clean); err != nil {
		t.Fatalf("Insert clean: %v", err)
	}

	got, err := repo.List(ctx, persistence.TaskMessageFilter{TaskID: task.ID})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var stored *persistence.TaskMessage
	for _, m := range got {
		if m.ID == latch.ID {
			stored = m
		}
	}
	if stored == nil {
		t.Fatal("latch message not found")
	}
	if stored.AuthorID == nil || strings.Contains(*stored.AuthorID, gh[:12]) || !strings.HasPrefix(*stored.AuthorID, "ops-") {
		t.Fatalf("author_id must be scanned with its prefix kept (#414): %v", stored.AuthorID)
	}
	all := stored.Content + string(stored.Metadata)
	for _, secret := range []string{gh, pass} {
		if strings.Contains(all, secret[:12]) {
			t.Fatalf("stored row holds the secret %q: %s / %s", secret[:12], stored.Content, stored.Metadata)
		}
	}
	if h, ok := taintlineage.ParseLatchHash(stored.Metadata); !ok || h != hash {
		t.Fatalf("taint latch lost: ok=%v hash=%q metadata=%s", ok, h, stored.Metadata)
	}
	if !strings.Contains(string(stored.Metadata), "12345678901234567890") {
		t.Errorf("number precision lost through jsonb: %s", stored.Metadata)
	}
}
