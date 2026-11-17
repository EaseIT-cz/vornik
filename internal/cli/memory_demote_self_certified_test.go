package cli

// GitHub #550 D2/D5 (design 22, amendment 2026-11-15): the verb on a real
// SQLite store. Never run against production: the verb is the operator's.

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence/sqlite"
	"vornik.io/vornik/internal/persistence/sqlite/sqlitetest"
)

func TestDemoteSelfCertified_DryRunApplyRestore(t *testing.T) {
	ctx := context.Background()
	db := sqlitetest.Memory(t)
	repo := sqlite.NewMemoryVerificationRepository(db.DB)
	for _, row := range []struct{ id, producer, validator, status, artifact string }{
		{"ca", "companion:claude-code", "companion:claude-code", "verified", ""},
		{"cb", "reviewer", "reviewer", "verified", ""},
		{"ck", "reviewer", "provenance:origin/main", "verified", "art"},
	} {
		if _, err := db.ExecContext(ctx, `INSERT INTO project_memory_chunks
			(id, project_id, content, content_hash, created_at, producer_role, validator_role, validation_status, artifact_id, confidence)
			VALUES (?, 'proj', 'x', ?, '2026-11-15T00:00:00Z', ?, ?, ?, NULLIF(?, ''), 0.9)`,
			row.id, "h-"+row.id, row.producer, row.validator, row.status, row.artifact); err != nil {
			t.Fatal(err)
		}
	}
	status := func(id string) string {
		var s string
		if err := db.QueryRowContext(ctx, `SELECT validation_status FROM project_memory_chunks WHERE id = ?`, id).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}

	var out bytes.Buffer
	ev, err := demoteSelfCertified(ctx, &out, repo, demoteOptions{Project: "proj", Actor: "operator"})
	if err != nil || ev != nil {
		t.Fatalf("dry run: %v %+v", err, ev)
	}
	for _, want := range []string{"3 verified chunks examined", "companion       1 chunks", "unknown_origin  1 chunks",
		"left alone      1 verified chunks", "e.g. ca", "e.g. cb", "dry run; nothing written"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("dry run output lacks %q:\n%s", want, out.String())
		}
	}
	if status("ca") != "verified" || status("cb") != "verified" {
		t.Fatal("the dry run changed a chunk")
	}

	out.Reset()
	ev, err = demoteSelfCertified(ctx, &out, repo, demoteOptions{Project: "proj", Actor: "operator", Reason: "GitHub #550", Apply: true})
	if err != nil || ev == nil || ev.Deverified != 2 || ev.Examined != 3 || ev.Unchanged != 1 {
		t.Fatalf("apply: %v %+v", err, ev)
	}
	if status("ca") != "unverified" || status("cb") != "unverified" || status("ck") != "verified" {
		t.Fatal("apply changed the wrong chunks")
	}

	// A restore reinstates self-certified stamps: refused without the flag.
	out.Reset()
	if _, err := demoteSelfCertified(ctx, &out, repo, demoteOptions{Project: "proj", Actor: "operator", Reason: "undo",
		Apply: true, Restore: ev.ID}); err == nil || !strings.Contains(err.Error(), "--allow-self-certified") {
		t.Fatalf("restore without --allow-self-certified: %v", err)
	}
	rs, err := demoteSelfCertified(ctx, &out, repo, demoteOptions{Project: "proj", Actor: "operator", Reason: "undo",
		Apply: true, Restore: ev.ID, AllowSelfCertified: true})
	if err != nil || rs == nil || rs.Verified != 2 {
		t.Fatalf("restore: %v %+v", err, rs)
	}
	if status("ca") != "verified" || status("cb") != "verified" {
		t.Fatal("the restore did not put the stamps back")
	}
}

func TestDemoteSelfCertified_ApplyNeedsAReason(t *testing.T) {
	demoteApply, demoteReason = true, " "
	t.Cleanup(func() { demoteApply, demoteReason = false, "" })
	err := runMemoryDemoteSelfCertified(memoryDemoteSelfCertifiedCmd, nil)
	if err == nil || !strings.Contains(err.Error(), "--reason") {
		t.Fatalf("--apply without --reason: %v", err)
	}
}
