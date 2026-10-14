package datasubject

import (
	"context"
	"strings"
	"testing"
)

// audit 2026-10-12: a channel_sessions link keyed on session_id alone is
// ambiguous, because the primary key is (kind, session_id). The row id is
// "<kind>/<session_id>", split on the FIRST slash (GitHub session ids contain
// slashes, kinds do not) — design 2026-07-29-art17-redact-and-reembed §15.10
// item 2, GitHub #344.
func TestParseChannelSessionRowID(t *testing.T) {
	for _, tc := range []struct {
		in, kind, id string
		ok           bool
	}{
		{"slack/C1.123", "slack", "C1.123", true},
		{"github/owner/repo#issues/1", "github", "owner/repo#issues/1", true},
		{"webchat/abc", "webchat", "abc", true},
		{"email/<a@b>", "email", "<a@b>", true},
		{"telegram/-100123", "telegram", "-100123", true},
		{"C1.123", "", "", false},
		{"nope/x", "", "", false},
		{"slack/", "", "", false},
		{"slack/   ", "", "", false},
		{"/x", "", "", false},
		{"", "", "", false},
	} {
		kind, id, err := ParseChannelSessionRowID(tc.in)
		if (err == nil) != tc.ok || kind != tc.kind || id != tc.id {
			t.Errorf("%q: got (%q, %q, %v), want (%q, %q, ok=%v)", tc.in, kind, id, err, tc.kind, tc.id, tc.ok)
		}
	}
}

func TestLinkValidate_ChannelSessionNeedsKindQualifiedRowID(t *testing.T) {
	base := Link{Table: TableChannelSessions, Source: SourceOperatorAsserted,
		Confidence: ConfidenceCertain, Exclusivity: SharedRow}
	bad := base
	bad.RowID = "C1.123"
	if err := bad.Validate(); err == nil || !strings.Contains(err.Error(), "<kind>/<session_id>") {
		t.Errorf("a bare session id must be refused with the expected form, got %v", err)
	}
	good := base
	good.RowID = "slack/C1.123"
	if err := good.Validate(); err != nil {
		t.Errorf("a kind-qualified id must validate: %v", err)
	}
	other := base
	other.Table = TableTaskMessages
	other.RowID = "tm-1"
	if err := other.Validate(); err != nil {
		t.Errorf("other tables keep their own row ids: %v", err)
	}
}

// audit 2026-10-12 (5d-0): applyRedact's own table check, exercised directly —
// the Execute-level test cannot tell whether this inner guard exists, and the
// collision second pass enters applyRedact without going through Execute's.
func TestApplyRedact_DefersANonChunkRowWithoutTouchingTheStore(t *testing.T) {
	r := &countingRedactor{}
	e := wiredExecutor(r, &fakeRewriter{out: "x"}, &recordingDeleter{})
	res := &ErasureResult{}
	a := Action{Table: TableChatAuditLog, RowID: "cal-1", Disposition: DispositionRedact}
	if retry := e.applyRedact(context.Background(), a, []string{"jane@example.com"}, &ErasurePlan{}, res); retry != nil {
		t.Fatalf("no retry expected, got %+v", retry)
	}
	if len(r.loads) != 0 {
		t.Fatalf("the chunk store must never see a transcript row id, got loads %v", r.loads)
	}
	if len(res.Failed) != 0 || len(res.Deferred) != 1 || res.Deferred[0].Reason != reasonNoRedactionPath {
		t.Fatalf("want one deferral with the record-type reason, got deferred=%+v failed=%+v", res.Deferred, res.Failed)
	}
}

// Art 15(4) default by kind (§15.10 item 4): only a web chat session is one
// person's; the rest are multi-party by construction.
func TestChannelSessionSingleSpeaker(t *testing.T) {
	if !ChannelSessionSingleSpeaker("webchat") {
		t.Error("webchat is one browser session, one person")
	}
	for _, k := range []string{"slack", "email", "github", "telegram", "unknown", ""} {
		if ChannelSessionSingleSpeaker(k) {
			t.Errorf("%q must be treated as multi-party", k)
		}
	}
}
