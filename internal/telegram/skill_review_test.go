package telegram

import (
	"context"
	"strings"
	"testing"
	"time"
	"vornik.io/vornik/internal/persistence/sqlite"
	"vornik.io/vornik/internal/persistence/sqlite/sqlitetest"
	"vornik.io/vornik/internal/skills"

	"vornik.io/vornik/internal/persistence"
)

func TestIsSkillApprover(t *testing.T) {
	b := &Bot{config: BotConfig{AllowedUsers: map[int64]UserAccess{
		559741208: {Allowed: true},
		111:       {Allowed: false},
	}}}
	if !b.isSkillApprover(559741208) {
		t.Error("allowed operator must be an approver")
	}
	if b.isSkillApprover(111) {
		t.Error("disallowed user must not approve")
	}
	if b.isSkillApprover(999) {
		t.Error("unknown user must not approve")
	}
}

func TestBuildSkillReviewDigest(t *testing.T) {
	drafts := []*persistence.Skill{
		{ID: "skill-1", Name: "trace-hang", Description: "when a model hangs"},
		{ID: "skill-2", Name: "restart-flow", Description: "safe restart"},
	}
	text, markup := buildSkillReviewDigest(drafts, nil)
	if !strings.Contains(text, "2 skill(s)") || !strings.Contains(text, "trace-hang") || !strings.Contains(text, "restart-flow") {
		t.Fatalf("digest text missing content:\n%s", text)
	}
	// Two buttons (approve+reject) per draft = 4 buttons across the grid.
	count := 0
	for _, row := range markup.InlineKeyboard {
		count += len(row)
	}
	if count != 4 {
		t.Fatalf("expected 4 buttons (approve+reject x2), got %d", count)
	}
}

func TestBuildSkillReviewDigest_GlobalBlastRadius(t *testing.T) {
	drafts := []*persistence.Skill{
		{ID: "g", Name: "wide-skill", Description: "everywhere", IsGlobal: true},
		{ID: "l", Name: "local-skill", Description: "here only"},
	}
	text, _ := buildSkillReviewDigest(drafts, nil)
	if !strings.Contains(text, "GLOBAL — affects ALL projects") {
		t.Fatalf("global draft must carry the blast-radius label:\n%s", text)
	}
	// The local draft must NOT get the label.
	if strings.Count(text, "affects ALL projects") != 1 {
		t.Fatalf("only the global draft should be labelled:\n%s", text)
	}
}

// The rollup line on the review card
// (LLD 2026-09-08-execution-ratings-approval-paths-design §2.3). The card is
// where an operator taps Approve, so it is where the evidence has to be.
func TestSkillReviewDigestCarriesTheRatingLine(t *testing.T) {
	drafts := []*persistence.Skill{{ID: "s1", Name: "n", Description: "d"}}

	text, _ := buildSkillReviewDigest(drafts, map[string]string{
		"s1": "never injected in this window — no rating evidence",
	})
	if !strings.Contains(text, "never injected in this window") {
		t.Errorf("the card must carry the rollup line, got:\n%s", text)
	}

	// With nothing wired the card is exactly what it was before phase 3 —
	// the line is absent rather than rendered blank or as a false clean bill.
	plain, _ := buildSkillReviewDigest(drafts, nil)
	if strings.Contains(plain, "📊") {
		t.Errorf("with no rollup wired the card must not render an empty line, got:\n%s", plain)
	}
}

func TestSkillReviewRevisionDateAndCallbackGate(t *testing.T) {
	ctx := context.Background()
	db := sqlitetest.Memory(t)
	repo := sqlite.NewSkillRepository(db.DB)
	sk, err := repo.Upsert(ctx, &persistence.Skill{ID: "revision", ProjectID: "p", Name: "deploy", Body: "one"})
	if err != nil {
		t.Fatal(err)
	}
	text, markup := buildSkillReviewDigest([]*persistence.Skill{sk}, nil)
	if !strings.Contains(text, "v1") || !strings.Contains(text, sk.ProposedAt.UTC().Format("2006-01-02 15:04:05 UTC")) {
		t.Fatal(text)
	}
	button := markup.InlineKeyboard[0][0].CallbackData
	if button != "skill:approve:revision:1" {
		t.Fatalf("unbound callback %q", button)
	}
	rig := newCallbackRig(t)
	rig.bot.skillRepo = repo
	rig.bot.config.AllowedUsers = map[int64]UserAccess{42: {Allowed: true}}
	_, err = repo.Upsert(ctx, &persistence.Skill{ID: "ignored", ProjectID: "p", Name: "deploy", Body: "two"})
	if err != nil {
		t.Fatal(err)
	}
	for _, payload := range []string{"revision", "revision:1", "revision:0", "revision:not-a-number"} {
		if err := rig.bot.handleSkillCallback(ctx, "cb", 42, "approve", payload); err != nil {
			t.Fatal(err)
		}
		got, _ := repo.GetByID(ctx, sk.ID)
		if got.Maturity != persistence.SkillMaturityDraft {
			t.Fatalf("%q approved newer revision", payload)
		}
	}
	acks := rig.callsTo("answerCallbackQuery")
	if !strings.Contains(string(acks[1].body), "Superseded by v2") {
		t.Fatalf("stale review not explained: %s", acks[1].body)
	}
	if err := rig.bot.handleSkillCallback(ctx, "cb", 42, "approve", "revision:2"); err != nil {
		t.Fatal(err)
	}
	got, _ := repo.GetByID(ctx, sk.ID)
	if got.Maturity != persistence.SkillMaturityActive {
		t.Fatal("current callback did not approve")
	}
	for _, tc := range []struct {
		user            int64
		action, payload string
	}{{99, "approve", "revision:2"}, {42, "unknown", "revision:2"}, {42, "approve", "missing:1"}} {
		if err := rig.bot.handleSkillCallback(ctx, "cb", tc.user, tc.action, tc.payload); err != nil {
			t.Fatal(err)
		}
	}
	if err := rig.bot.handleSkillCallback(ctx, "cb", 42, "reject", "revision:2"); err != nil {
		t.Fatal(err)
	}
	rig.bot.skillRepo = nil
	if err := rig.bot.handleSkillCallback(ctx, "cb", 42, "approve", "revision:2"); err != nil {
		t.Fatal(err)
	}
}

func TestSkillReviewNotifiesEachRevisionOnce(t *testing.T) {
	rig := newCallbackRig(t)
	db := sqlitetest.Memory(t)
	repo := sqlite.NewSkillRepository(db.DB)
	rig.bot.skillRepo = repo
	rig.bot.config.AllowedUsers = map[int64]UserAccess{42: {Allowed: true}, 99: {Allowed: false}}
	ctx := context.Background()
	rig.bot.sendSkillReviewDigest(ctx) // no drafts
	sk := &persistence.Skill{ID: "same", ProjectID: "p", Name: "repeat", Body: "one"}
	if _, err := repo.Upsert(ctx, sk); err != nil {
		t.Fatal(err)
	}
	rig.bot.sendSkillReviewDigest(ctx)
	rig.bot.sendSkillReviewDigest(ctx)
	if got := len(rig.callsTo("sendMessage")); got != 1 {
		t.Fatalf("v1 notifications %d", got)
	}
	sk.Body = "two"
	if _, err := repo.Upsert(ctx, sk); err != nil {
		t.Fatal(err)
	}
	rig.bot.sendSkillReviewDigest(ctx)
	rig.bot.sendSkillReviewDigest(ctx)
	calls := rig.callsTo("sendMessage")
	if len(calls) != 2 || !strings.Contains(string(calls[1].body), "v2") {
		t.Fatalf("v2 never notified: %v", calls)
	}
	rig.bot.skillRepo = nil
	rig.bot.sendSkillReviewDigest(ctx)
}

func TestSkillReviewCallbackLimit(t *testing.T) {
	_, markup := buildSkillReviewDigest([]*persistence.Skill{{ID: strings.Repeat("x", 60), Version: 123, ProposedAt: time.Now()}}, nil)
	if len(markup.InlineKeyboard) != 0 {
		t.Fatal("oversize callback must not be emitted")
	}
	for _, token := range []string{"", "id", "id:0", "id:-1", "id:no", "id:1:2", ":1"} {
		if _, _, err := skills.ParseReviewToken(token); err == nil {
			t.Fatalf("unsafe token accepted %q", token)
		}
	}
}
