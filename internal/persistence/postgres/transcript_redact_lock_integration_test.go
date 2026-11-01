//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"vornik.io/vornik/internal/datasubject"
	"vornik.io/vornik/internal/persistence/subjectstore"
)

// GitHub #344, slice 5d-1a (design 2026-07-29-art17-redact-and-reembed §15.14,
// review 0e4e F3 and e671 M1). Under READ COMMITTED the guarded UPDATE's
// live predicate reads tasks at its own snapshot, so a task change committing
// between that snapshot and the commit would be missed. The subtree is locked
// first; these drive each way a row becomes live from a second connection,
// while the erasure holds its locks, and require it to wait.

const lockWait = 300 * time.Millisecond

// blockedWhileLocked runs stmt on another connection from inside the Locked
// hook and reports whether it was still waiting when the hook returned. The
// returned channel yields the statement's error once the erasure commits.
func blockedWhileLocked(t *testing.T, db *sql.DB, stmt string, args ...any) (hook func(), blocked *bool, done chan error) {
	t.Helper()
	done = make(chan error, 1)
	blocked = new(bool)
	fired := false
	hook = func() {
		if fired {
			return
		}
		fired = true
		go func() {
			_, err := db.ExecContext(context.Background(), stmt, args...)
			done <- err
		}()
		select {
		case err := <-done:
			done <- err
		case <-time.After(lockWait):
			*blocked = true
		}
	}
	return hook, blocked, done
}

func seedLockTask(t *testing.T, db *sql.DB, id, parent, chatTurn, status string) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO tasks (id, project_id, status, parent_task_id, chat_turn_id, created_at, updated_at)
		VALUES ($1, 'lock-proj', $2, NULLIF($3, ''), NULLIF($4, ''), now(), now())`, id, status, parent, chatTurn); err != nil {
		t.Fatalf("seed task: %v", err)
	}
}

func TestTranscriptRedact_PostgresLocksBlockLiveness(t *testing.T) {
	ctx := context.Background()
	idb := newIntegrationDB(t)
	repo := NewDataSubjectRepository(idb.DB)
	n := time.Now().UnixNano()
	id := func(p string) string { return fmt.Sprintf("%s-%d", p, n) }

	apply := func(t *testing.T, msg string) datasubject.TurnResult {
		t.Helper()
		turn, err := repo.LoadTurn(ctx, datasubject.TableTaskMessages, msg)
		if err != nil {
			t.Fatal(err)
		}
		fields := map[string]*string{}
		for k, v := range turn.Fields {
			fields[k] = v
		}
		tomb := "[removed]"
		fields["content"] = &tomb
		res, err := repo.ApplyTurn(ctx, turn, datasubject.TurnWrite{Fields: fields, Author: "removed-participant-1"})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	seedMsg := func(t *testing.T, msg, task string) {
		t.Helper()
		if _, err := idb.DB.Exec(`INSERT INTO task_messages (id, task_id, author_kind, author_id, message_kind, content)
			VALUES ($1, $2, 'operator', 'telegram:1', 'message', 'hi')`, msg, task); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("a new child task waits for the erasure", func(t *testing.T) {
		parent, msg := id("lk-parent"), id("lk-msg")
		seedLockTask(t, idb.DB, parent, "", "", "COMPLETED")
		seedMsg(t, msg, parent)
		hook, blocked, done := blockedWhileLocked(t, idb.DB, `INSERT INTO tasks (id, project_id, status, parent_task_id, created_at, updated_at)
			VALUES ($1, 'lock-proj', 'RUNNING', $2, now(), now())`, id("lk-child"), parent)
		repo.SetTranscriptHooks(subjectstore.TranscriptHooks{Locked: hook})
		defer repo.SetTranscriptHooks(subjectstore.TranscriptHooks{})
		res := apply(t, msg)
		if !*blocked {
			t.Fatal("the child insert did not wait for the erasure's lock")
		}
		if err := <-done; err != nil {
			t.Fatalf("child insert after commit: %v", err)
		}
		// Committed order: the erasure, then the child. Applied is consistent.
		if res.Outcome != datasubject.TranscriptApplied {
			t.Fatalf("outcome = %+v", res)
		}
	})

	t.Run("resuming a settled task waits for the erasure", func(t *testing.T) {
		task, msg := id("lk-resume"), id("lk-msg2")
		seedLockTask(t, idb.DB, task, "", "", "COMPLETED")
		seedMsg(t, msg, task)
		hook, blocked, done := blockedWhileLocked(t, idb.DB, `UPDATE tasks SET status = 'RUNNING' WHERE id = $1`, task)
		repo.SetTranscriptHooks(subjectstore.TranscriptHooks{Locked: hook})
		defer repo.SetTranscriptHooks(subjectstore.TranscriptHooks{})
		res := apply(t, msg)
		if !*blocked {
			t.Fatal("the resume did not wait for the erasure's lock")
		}
		if err := <-done; err != nil {
			t.Fatalf("resume after commit: %v", err)
		}
		if res.Outcome != datasubject.TranscriptApplied {
			t.Fatalf("outcome = %+v", res)
		}
	})

	t.Run("a referrer's re-key holds its own subtree lock", func(t *testing.T) {
		body := "profile " + id("lk-prompt")
		var old string
		if err := idb.DB.QueryRow(`INSERT INTO chat_system_prompts (hash, body, created_at)
			VALUES (encode(sha256(convert_to($1, 'UTF8')), 'hex'), $1, now()) RETURNING hash`, body).Scan(&old); err != nil {
			t.Fatal(err)
		}
		row, ref, refTask := id("lk-row"), id("lk-ref"), id("lk-reftask")
		for _, r := range []string{row, ref} {
			if _, err := idb.DB.Exec(`INSERT INTO chat_audit_log (id, chat_id, user_id, system_prompt_hash, user_message)
				VALUES ($1, $1, 'telegram:1', $2, 'hi')`, r, old); err != nil {
				t.Fatal(err)
			}
		}
		seedLockTask(t, idb.DB, refTask, "", ref, "COMPLETED")
		// The first Locked call is the origin row's; the second is the
		// referrer's. Resume the referrer's task during the second.
		inner, blocked, done := blockedWhileLocked(t, idb.DB, `UPDATE tasks SET status = 'RUNNING' WHERE id = $1`, refTask)
		calls := 0
		repo.SetTranscriptHooks(subjectstore.TranscriptHooks{Locked: func() {
			calls++
			if calls == 2 {
				inner()
			}
		}})
		defer repo.SetTranscriptHooks(subjectstore.TranscriptHooks{})
		turn, err := repo.LoadTurn(ctx, datasubject.TableChatAuditLog, row)
		if err != nil {
			t.Fatal(err)
		}
		res, err := repo.ApplyTurn(ctx, turn, datasubject.TurnWrite{Fields: turn.Fields, Author: turn.Author,
			Prompt: &datasubject.PromptRekey{OldHash: old, NewBody: "profile [masked] " + id("x")}})
		if err != nil {
			t.Fatal(err)
		}
		if calls < 2 || !*blocked {
			t.Fatalf("the referrer's resume did not wait (calls %d)", calls)
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if res.Repointed != 1 || len(res.DeferredReferrers) != 0 {
			t.Fatalf("re-key = %+v", res)
		}
	})
}
