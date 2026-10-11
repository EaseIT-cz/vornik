//go:build integration

package memory_test

// GitHub #119 item 6 (2026-10-08): a forget made on the approver device
// (route device_forget) is never reinstated by an identical re-add from the
// Hermes mirror, and it leaves every read of the project, whichever key reads
// it (LLD 22, "Memory the user can see", change §2 and §3).

import (
	"context"
	"testing"

	"vornik.io/vornik/internal/memory"
	"vornik.io/vornik/internal/memoryfirewall"
	"vornik.io/vornik/internal/persistence"
)

func TestIntegration_DeviceForget_RouteIsNeverReinstated(t *testing.T) {
	db := openIngestRecallDB(t)
	p, s, repo := newReinstateStack(t, db)
	project := persistence.GenerateID("itest-gh119-devforget")
	t.Cleanup(func() { cleanupProjects(t, db, project) })

	note, token := mirrorNote("user", "My dentist is Dr Novak at the Vinohrady clinic in Prague, booked every spring.")
	if st := deposit(t, p, project, "hermes", note); st.Admitted != 1 {
		t.Fatalf("first deposit: %+v", st)
	}
	id, _, _ := onlyChunk(t, db, project)
	if n, err := repo.MarkRefutedByIDs(context.Background(), project, []string{id}, memory.RefuteRouteDeviceForget); err != nil || n != 1 {
		t.Fatalf("device forget: %d %v", n, err)
	}
	if st := deposit(t, p, project, "hermes", note); st.Admitted != 0 {
		t.Fatalf("an identical mirror re-add reinstated a device forget: %+v", st)
	}
	_, status, route := onlyChunk(t, db, project)
	if status != "refuted" || route.String != memory.RefuteRouteDeviceForget {
		t.Fatalf("after re-add: status %q route %q", status, route.String)
	}
	if recalled(t, s, project, token, id) {
		t.Fatal("a device-forgotten note is recalled")
	}
}

func TestIntegration_DeviceForget_RefutesForEveryKeyOfTheProject(t *testing.T) {
	db := openIngestRecallDB(t)
	p, s, repo := newReinstateStack(t, db)
	project := persistence.GenerateID("itest-gh119-everykey")
	t.Cleanup(func() { cleanupProjects(t, db, project) })
	ctx := context.Background()

	note, token := mirrorNote("user", "The user's daughter plays the cello in the Prague youth orchestra every Thursday.")
	if st := deposit(t, p, project, "hermes", note); st.Admitted != 1 {
		t.Fatalf("deposit: %+v", st)
	}
	id, _, _ := onlyChunk(t, db, project)
	if n, err := repo.MarkRefutedByIDs(ctx, project, []string{id}, memory.RefuteRouteDeviceForget); err != nil || n != 1 {
		t.Fatalf("device forget: %d %v", n, err)
	}
	// Two keys of the project, as their tools read it: the reads take the
	// project, never the key, so the forget holds for both. Each key's
	// request context is used where the read takes one.
	for _, key := range []struct{ role, id string }{{"companion:hermes", "akey-hermes"}, {"companion:claude-code", "akey-claude"}} {
		res, err := s.RecallWithContext(ctx, project, token, memory.SearchOptions{Limit: 20},
			memoryfirewall.RequestContext{Role: key.role, OperatorID: key.id, Purpose: memoryfirewall.PurposeOperational})
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range res {
			if r.ChunkID == id {
				t.Fatalf("recall from %s returned the forgotten note", key.role)
			}
		}
		recent, err := s.RecentWithContext(ctx, project, 20, "", false, false,
			memoryfirewall.RequestContext{Role: key.role, OperatorID: key.id, Purpose: memoryfirewall.PurposeOperational})
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range recent {
			if r.ChunkID == id {
				t.Fatalf("recent_memory from %s listed the forgotten note", key.role)
			}
		}
		page, err := s.ListPageWithContext(ctx, project, nil, 100,
			memoryfirewall.RequestContext{Role: key.role, OperatorID: key.id, Purpose: memoryfirewall.PurposeOperational})
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range page.Rows {
			if r.ChunkID == id {
				t.Fatalf("memory_list from %s listed the forgotten note", key.role)
			}
		}
	}
}
