//go:build integration

package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/datasubject"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/postgres"
	"vornik.io/vornik/internal/sessionstore"
)

// GitHub #344 slice 5d-2 (art17 redact design §15.18). Found at: review,
// observed at integration (§15.10.1): every channel-session erasure was
// deferred, because a stored-row delete was undone by the daemon's next
// save. This drives `vornikctl subject erase`'s own wiring on Postgres: the
// shared email session is masked, and a daemon persister that loaded the
// session before the erasure is refused when it tries to save over it.
func TestSubjectErase_SharedChannelSessionOnPostgres(t *testing.T) {
	ctx := context.Background()
	db := dbcovOpen(t)
	repo := postgres.NewDataSubjectRepository(db)
	n := time.Now().UnixNano()
	id := func(p string) string { return fmt.Sprintf("%s-%d", p, n) }
	anna, sid := id("anna"), id("<thread")+"@x>"
	addr := id("anna") + "@example.com"

	if err := repo.CreateSubject(ctx, datasubject.Subject{ID: anna, DisplayName: anna}); err != nil {
		t.Fatal(err)
	}
	if err := repo.AddIdentifier(ctx, anna, datasubject.Identifier{Kind: "email", Value: addr,
		Source: datasubject.SourceOperatorAsserted, Confidence: datasubject.ConfidenceCertain}); err != nil {
		t.Fatal(err)
	}
	daemon := sessionstore.New(postgres.NewChannelSessionRepository(db), "email", zerolog.Nop())
	if err := daemon.Save(ctx, sid, "p", []chat.Message{
		{Role: "user", Content: "From: Anna <" + addr + ">\n\nmy number is 777"},
		{Role: "user", Content: "From: bob@example.com\n\nI will write to " + addr},
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM channel_sessions WHERE session_id = $1`, sid) })
	if err := repo.AddLink(ctx, anna, datasubject.Link{Table: datasubject.TableChannelSessions,
		RowID: "email/" + sid, Source: datasubject.SourceOperatorAsserted,
		Confidence: datasubject.ConfidenceCertain, Exclusivity: datasubject.SharedRow}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	req := datasubject.Request{ID: id("req"), SubjectID: anna, Kind: datasubject.RequestErasure,
		State: datasubject.StateOpen, OpenedAt: now, ErasureGround: datasubject.GroundObjection}
	if err := repo.CreateRequest(ctx, req); err != nil {
		t.Fatal(err)
	}
	if err := req.Verify("operator", "test", now); err != nil {
		t.Fatal(err)
	}
	if err := repo.SaveRequest(ctx, req); err != nil {
		t.Fatal(err)
	}
	plan, err := buildErasurePlan(ctx, repo, req)
	if err != nil {
		t.Fatal(err)
	}
	oldApply := subjectEraseApply
	subjectEraseApply = true
	defer func() { subjectEraseApply = oldApply }()
	ex, err := newSubjectExecutor(&erasureDeps{Repo: repo, DB: db, Cfg: &config.Config{}, Root: t.TempDir()}, req.ID)
	if err != nil {
		t.Fatal(err)
	}
	res, err := ex.Execute(ctx, plan)
	if err != nil {
		t.Fatalf("execute: %v (%+v)", err, res)
	}
	if len(res.Redacted) != 1 || res.Redacted[0].Kind != datasubject.SessionTombstoned || len(res.Deferred) != 0 {
		t.Fatalf("result = %+v", res)
	}
	var history string
	if err := db.QueryRow(`SELECT history::text FROM channel_sessions WHERE kind = 'email' AND session_id = $1`,
		sid).Scan(&history); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(history, addr) || strings.Contains(history, "777") || !strings.Contains(history, "bob@example.com") {
		t.Fatalf("masked history = %s", history)
	}

	// The daemon's turn that began before the erasure ends now: refused.
	err = daemon.Save(ctx, sid, "p", []chat.Message{{Role: "user", Content: "From: Anna <" + addr + ">\n\nmy number is 777"}})
	if !errors.Is(err, sessionstore.ErrSessionMoved) {
		t.Fatalf("a save over the erased session = %v, want ErrSessionMoved", err)
	}
	var after string
	if err := db.QueryRow(`SELECT history::text FROM channel_sessions WHERE kind = 'email' AND session_id = $1`,
		sid).Scan(&after); err != nil || after != history {
		t.Fatalf("the refused save changed the row: %v %s", err, after)
	}
	if daemon.Fresh(ctx, sid) {
		t.Fatal("the daemon's cached copy reads as fresh after the erasure")
	}
}

// A registered node that does not advertise the fence would write an erased
// session back, so the erasure names it; a relay-only node holds no sessions.
// In this package so it runs in sequence with the erasure test above, which
// a foreign laggard row would defer.
func TestSubjectErase_ChannelSessionFenceLaggardNodes(t *testing.T) {
	ctx := context.Background()
	db := dbcovOpen(t)
	nodes := postgres.NewClusterNodeRepository(db)
	clean := func() {
		_, _ = db.ExecContext(ctx, `DELETE FROM cluster_nodes WHERE instance_id LIKE 'fence-node-%'`)
	}
	clean()
	t.Cleanup(clean)
	for _, n := range []persistence.ClusterNode{
		{InstanceID: "fence-node-new", Profile: "all", Version: "new",
			Capabilities: map[string]bool{persistence.ChannelSessionFenceCapability: true}},
		{InstanceID: "fence-node-old", Profile: "all", Version: "2026.10.5", Capabilities: map[string]bool{"RunWorkers": true}},
		{InstanceID: "fence-node-dmz", Profile: "webhook", Version: "2026.10.5", Capabilities: map[string]bool{"RelayMode": true}},
	} {
		n := n
		if err := nodes.Upsert(ctx, &n); err != nil {
			t.Fatal(err)
		}
	}
	got, err := postgres.NewDataSubjectRepository(db).ChannelSessionEraser().FenceLaggards(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var old, others int
	for _, l := range got {
		switch {
		case strings.Contains(l, "fence-node-old"):
			old++
		case strings.Contains(l, "fence-node-"):
			others++
		}
	}
	if old != 1 || others != 0 {
		t.Fatalf("laggards = %v", got)
	}
}

// #346 Q5 (art17 redact design §15.19): a shared Slack session records no
// speaker per message, so the erasure deletes it in full.
func TestSubjectErase_UnattributedSharedSessionIsDeletedOnPostgres(t *testing.T) {
	ctx := context.Background()
	db := dbcovOpen(t)
	repo := postgres.NewDataSubjectRepository(db)
	n := time.Now().UnixNano()
	anna, sid := fmt.Sprintf("anna-%d", n), fmt.Sprintf("T%d/C1#1.1", n)
	if err := repo.CreateSubject(ctx, datasubject.Subject{ID: anna, DisplayName: anna}); err != nil {
		t.Fatal(err)
	}
	if err := repo.AddIdentifier(ctx, anna, datasubject.Identifier{Kind: "name", Value: "Anna Novak",
		Source: datasubject.SourceOperatorAsserted, Confidence: datasubject.ConfidenceCertain}); err != nil {
		t.Fatal(err)
	}
	daemon := sessionstore.New(postgres.NewChannelSessionRepository(db), "slack", zerolog.Nop())
	if err := daemon.Save(ctx, sid, "p", []chat.Message{{Role: "user", Content: "Anna Novak here, my plan is X"}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM channel_sessions WHERE session_id = $1`, sid) })
	if err := repo.AddLink(ctx, anna, datasubject.Link{Table: datasubject.TableChannelSessions,
		RowID: "slack/" + sid, Source: datasubject.SourceOperatorAsserted,
		Confidence: datasubject.ConfidenceCertain, Exclusivity: datasubject.SharedRow}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	req := datasubject.Request{ID: fmt.Sprintf("req-%d", n), SubjectID: anna, Kind: datasubject.RequestErasure,
		State: datasubject.StateOpen, OpenedAt: now, ErasureGround: datasubject.GroundObjection}
	if err := repo.CreateRequest(ctx, req); err != nil {
		t.Fatal(err)
	}
	if err := req.Verify("operator", "test", now); err != nil {
		t.Fatal(err)
	}
	if err := repo.SaveRequest(ctx, req); err != nil {
		t.Fatal(err)
	}
	plan, err := buildErasurePlan(ctx, repo, req)
	if err != nil {
		t.Fatal(err)
	}
	if err := assessResidual(plan, "", "", now); err != nil {
		t.Fatalf("a plan that only deletes an unattributed session needs no assessment: %v", err)
	}
	ex, err := newSubjectExecutor(&erasureDeps{Repo: repo, DB: db, Cfg: &config.Config{}, Root: t.TempDir()}, req.ID)
	if err != nil {
		t.Fatal(err)
	}
	res, err := ex.Execute(ctx, plan)
	if err != nil {
		t.Fatalf("execute: %v (%+v)", err, res)
	}
	if len(res.Deleted) != 1 || res.Deleted[0].Kind != datasubject.SessionUnattributed {
		t.Fatalf("result = %+v", res)
	}
	var left int
	if err := db.QueryRow(`SELECT COUNT(*) FROM channel_sessions WHERE session_id = $1`, sid).Scan(&left); err != nil || left != 0 {
		t.Fatalf("the session is still stored (%d, %v)", left, err)
	}
}
