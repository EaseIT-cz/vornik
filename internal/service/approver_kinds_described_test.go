package service

import (
	"context"
	"testing"
	"time"

	"vornik.io/vornik/internal/agentadmin"
	"vornik.io/vornik/internal/approverdevice"
	"vornik.io/vornik/internal/persistence"
)

// Agent-administered design §18.7 and Hermes approval transport design §4.1:
// every request kind a phone decides has a plain summary and a level on its
// page. The kinds come from persistence.ApprovalKinds, so a kind added there
// without a describer fails here. device_enrollment is the one exception: it
// is about the phone itself, and its sentence is the whole story.
func TestApproverKinds_EveryPhoneKindIsDescribed(t *testing.T) {
	f := newAgentAdminFixture(t)
	svc := f.c.approverDeviceService()
	now := time.Now().UTC()
	rendered := map[string]string{
		persistence.ApprovalKindMemoryRetention: `{"project_id":"companion-janka","chunk_id":"c70","source":"note","preview":"A stored preference","idle_days":365}`,
		persistence.ApprovalKindWideningChange:  `{"change":{"plain":{"summary":"Your assistant wants a project.","level":"Low","reasons":["it reaches nothing"]}}}`,
		persistence.ApprovalKindCredentialSlot:  `{"change":{"plain":{"summary":"You are asked for a key.","level":"Medium","reasons":["it stores a credential"]}}}`,
		persistence.ApprovalKindBrokerAction:    `{"action_id":"ba_1","workflow":"hermes--mail--reply","action":"send_reply"}`,
	}
	examined := 0
	for _, kind := range persistence.ApprovalKinds {
		if kind == persistence.ApprovalKindDeviceEnrollment {
			continue
		}
		var row persistence.AgentApprovalRequestRow
		if kind == persistence.ApprovalKindHostAction {
			if _, err := svc.FileHostAction(context.Background(), "hermes", approverdevice.HostActionRequest{
				RequestID: "ab12", Digest: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
				Command: "rm -rf /tmp/x", Description: "recursive delete", PatternKey: "rm", Surface: "cli",
				TimeoutSeconds: 300, AllowedChoices: []string{"once", "deny"}}); err != nil {
				t.Fatal(err)
			}
			r, err := svc.Request(context.Background(), approverdevice.HostActionID("hermes", "ab12"))
			if err != nil {
				t.Fatal(err)
			}
			row = *r
		} else {
			raw, ok := rendered[kind]
			if !ok {
				t.Fatalf("kind %s: no fixture; add one and a describer", kind)
			}
			row = persistence.AgentApprovalRequestRow{ID: "apr_" + kind, Kind: kind, Sentence: "s", Rendered: []byte(raw),
				Status: persistence.ApprovalPending, CreatedAt: now, ExpiresAt: now.Add(time.Hour)}
		}
		d := svc.Describe(row)
		if d == nil || d.Summary == "" || d.Level == "" || len(d.Reasons) == 0 {
			t.Errorf("kind %s has no plain summary and level: %+v", kind, d)
			continue
		}
		if kind == persistence.ApprovalKindHostAction && (d.Level != agentadmin.LevelHigh || d.Summary != agentadmin.ExplainHostAction().Summary) {
			t.Errorf("host_action: %+v, want the §18.7 phrase at High", d)
		}
		examined++
	}
	if examined != len(persistence.ApprovalKinds)-1 {
		t.Fatalf("examined %d of %d kinds", examined, len(persistence.ApprovalKinds)-1)
	}
	t.Logf("examined %d kinds", examined)
}
