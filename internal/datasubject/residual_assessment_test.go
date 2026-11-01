package datasubject

import (
	"testing"
	"time"
)

// #346 decision Q2 (art17 redact design §15.19). Found at: review, observed
// at integration (the stored erasure report): the ground for keeping what
// remains of the subject in others' messages was a fixed sentence, applied to
// every request without an assessment (EDPB 2026 erasure report: exceptions
// applied automatically).
func TestErasurePlan_MayLeaveResidual(t *testing.T) {
	cases := []struct {
		name string
		a    Action
		want bool
	}{
		{"shared transcript", Action{Table: TableChatAuditLog, Disposition: DispositionRedact}, true},
		{"shared task message", Action{Table: TableTaskMessages, Disposition: DispositionRedact}, true},
		{"email session", Action{Table: TableChannelSessions, RowID: "email/<m@x>", Disposition: DispositionRedact}, true},
		{"webchat session", Action{Table: TableChannelSessions, RowID: "webchat/w", Disposition: DispositionRedact}, true},
		{"legacy bare session id", Action{Table: TableChannelSessions, RowID: "bare", Disposition: DispositionRedact}, true},
		{"slack session is deleted", Action{Table: TableChannelSessions, RowID: "slack/T/C#1", Disposition: DispositionRedact}, false},
		{"a delete", Action{Table: TableChatAuditLog, Disposition: DispositionDelete}, false},
		{"a memory chunk", Action{Table: TableProjectMemoryChunks, Disposition: DispositionRedact}, false},
	}
	for _, c := range cases {
		p := &ErasurePlan{Actions: []Action{c.a}}
		if got := p.MayLeaveResidual(); got != c.want {
			t.Errorf("%s: MayLeaveResidual = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestErasurePlan_ResidualAssessmentReachesTheNarrative(t *testing.T) {
	at := time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)
	p := &ErasurePlan{ResidualAssessment: &ResidualAssessment{Text: "GDPR Art 17(3)(a): ...", By: "dpo", At: at}}
	res := &ErasureResult{Redacted: []RedactedAction{{Table: TableChatAuditLog, RowID: "c1", Kind: TranscriptMasked}}}
	if n := res.Narrative(p); n.ResidualGround != "GDPR Art 17(3)(a): ..." {
		t.Fatalf("narrative residual ground = %q", n.ResidualGround)
	}
}
