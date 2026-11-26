package datasubject

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// GitHub #470 (GDPR design §4.22, §4.20.1 item 14): the Art 15 report carried
// none of §4.4(b)-(d). Found at: audit (2026-10-21 status audit; the level a
// subject observes is the `vornikctl subject export` artefact, tested in
// internal/cli). These pin the pure half: purposes from the ROPA mirror, and an
// export built without configuration stating its unknowns.

func verifiedAccess(t *testing.T) Request {
	t.Helper()
	now := time.Now().UTC()
	req := Request{ID: "dsr_470", SubjectID: "ds_470", Kind: RequestAccess, State: StateOpen, OpenedAt: now}
	if err := req.Verify("operator", "authenticated session", now); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	return req
}

func TestBuildExport_StatesPurposesFromROPA(t *testing.T) {
	exp, err := BuildExport(verifiedAccess(t), nil, nil)
	if err != nil {
		t.Fatalf("BuildExport: %v", err)
	}
	if len(exp.Purposes) != len(ROPAActivities) || len(exp.Purposes) == 0 {
		t.Fatalf("Art 15(1)(a): the report must state every ROPA purpose (%d), got %d", len(ROPAActivities), len(exp.Purposes))
	}
	for i, p := range exp.Purposes {
		if p.ID == "" || p.Purpose == "" || p.Source != ROPASource {
			t.Errorf("purpose %d must carry its id, purpose and source %q, got %+v", i, ROPASource, p)
		}
	}
	blob, _ := json.Marshal(exp)
	if !strings.Contains(string(blob), `"purposes":[`) {
		t.Errorf("the purposes must reach the artefact:\n%s", blob)
	}
}

func TestBuildExport_WithoutConfigStatesUnknown(t *testing.T) {
	exp, err := BuildExport(verifiedAccess(t), nil, nil)
	if err != nil {
		t.Fatalf("BuildExport: %v", err)
	}
	if exp.Retention.Known {
		t.Errorf("Art 15(1)(d): with no configuration read, retention must say it is unknown, got %+v", exp.Retention)
	}
	if !strings.HasPrefix(exp.Retention.Scope, Unknown) {
		t.Errorf("the retention statement must say why it is unknown, got %q", exp.Retention.Scope)
	}
	var unknownProvider bool
	for _, r := range exp.Recipients {
		if r.Category == CategoryModelProvider && r.Location == Unknown && r.Destination != "" {
			unknownProvider = true
		}
	}
	if !unknownProvider {
		t.Errorf("Art 15(1)(c): with no configuration read, the model providers must be stated as unknown, got %+v", exp.Recipients)
	}
	if len(exp.Recipients) < 3 {
		t.Errorf("the two static ROPA §5 categories must be listed beside the providers, got %+v", exp.Recipients)
	}
}

func TestSetRecipientsKeepsTheStaticCategories(t *testing.T) {
	exp, err := BuildExport(verifiedAccess(t), nil, nil)
	if err != nil {
		t.Fatalf("BuildExport: %v", err)
	}
	exp.SetRecipients([]Recipient{{Name: "http", Category: CategoryModelProvider, Location: LocationRemote,
		Destination: "api.example.org", ConfigKey: "chat.endpoint"}})
	if len(exp.Recipients) != 1+len(StaticRecipients()) {
		t.Fatalf("configured providers plus the static categories, got %+v", exp.Recipients)
	}
	for _, r := range exp.Recipients {
		if r.Location == Unknown && r.Category == CategoryModelProvider {
			t.Errorf("once the configuration is read, the unknown provider placeholder must go: %+v", r)
		}
	}
	exp.SetRecipients(nil)
	if len(exp.Recipients) != len(StaticRecipients())+1 || exp.Recipients[0].Destination == "" {
		t.Errorf("no provider configured is stated, not left empty: %+v", exp.Recipients)
	}
}
