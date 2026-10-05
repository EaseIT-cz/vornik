package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"vornik.io/vornik/internal/agentadmin"
	"vornik.io/vornik/internal/approverdevice"
	"vornik.io/vornik/internal/persistence"
)

func TestAgentAdmin_ListSetupDrainsApprovalNotifications(t *testing.T) {
	f := newAgentAdminFixture(t)
	ctx := context.Background()
	f.do(agentadmin.VerbCreateProject, agentadmin.CreateProjectInput{Slug: "finance", Purpose: "Monthly finance"})
	res := f.do(agentadmin.VerbAddMCPServer, agentadmin.AddMCPServerInput{Project: "finance", Name: "bank",
		URL: "https://bank.invalid/mcp", Auth: agentadmin.MCPAuthInput{Mode: "none"}})
	id := requestIDOf(res)
	req, _ := f.c.repos.ApproverDevices.GetRequest(ctx, id)
	if err := f.svc.devices.Decide(ctx, f.device, id, req.RenderedSHA256, true); err != nil {
		t.Fatal(err)
	}
	other := *f.key
	other.AgentNamespace = "other"
	other.ProjectID = ""
	otherView, err := f.svc.ListSetup(ctx, &other)
	if err != nil {
		t.Fatal(err)
	}
	if len(otherView.Notifications) != 0 {
		t.Fatalf("other namespace saw notifications: %+v", otherView.Notifications)
	}
	v := setupOf(t, f)
	if len(v.Notifications) != 1 {
		t.Fatalf("notifications = %+v", v.Notifications)
	}
	if n := v.Notifications[0]; n.ChangeID != id || n.Kind != persistence.ApprovalKindWideningChange || n.Status != "approved" {
		t.Fatalf("notification = %+v", n)
	}
	raw, _ := json.Marshal(v.Notifications)
	if strings.Contains(string(raw), "bank.invalid") || strings.Contains(string(raw), "https://") {
		t.Fatalf("notification leaked rendered content: %s", raw)
	}
	if again := setupOf(t, f); len(again.Notifications) != 0 {
		t.Fatalf("notifications were not drained: %+v", again.Notifications)
	}
}

func TestAgentAdmin_ListSetupDrainsCredentialRejectionNotification(t *testing.T) {
	f, id := pendingCredentialSlot(t)
	ctx := context.Background()
	req, _ := f.c.repos.ApproverDevices.GetRequest(ctx, id)
	if err := f.svc.devices.Decide(ctx, f.device, id, req.RenderedSHA256, false); err != nil {
		t.Fatal(err)
	}
	v := setupOf(t, f)
	if len(v.Notifications) != 1 {
		t.Fatalf("notifications = %+v", v.Notifications)
	}
	if n := v.Notifications[0]; n.ChangeID != id || n.Kind != persistence.ApprovalKindCredentialSlot || n.Status != "rejected" {
		t.Fatalf("notification = %+v", n)
	}
	raw, _ := json.Marshal(v.Notifications)
	if strings.Contains(string(raw), "FIO") || strings.Contains(string(raw), credCanary) {
		t.Fatalf("notification leaked credential context: %s", raw)
	}
}

func TestAgentAdmin_ListSetupDrainsCredentialFailureNotification(t *testing.T) {
	f, id := pendingCredentialSlot(t)
	ctx := context.Background()
	req, _ := f.c.repos.ApproverDevices.GetRequest(ctx, id)
	f.c.secretStoreMu.Lock()
	f.c.secretStore, f.c.secretStoreLoaded, f.c.secretStoreErr = nil, true, errors.New("the key is unreadable")
	f.c.secretStoreMu.Unlock()
	err := f.svc.enterCredential(ctx, f.device, *req, req.RenderedSHA256, []byte("v"))
	if !errors.Is(err, approverdevice.ErrValueNotStored) {
		t.Fatalf("err = %v, want ErrValueNotStored", err)
	}
	v := setupOf(t, f)
	if len(v.Notifications) != 1 {
		t.Fatalf("notifications = %+v", v.Notifications)
	}
	if n := v.Notifications[0]; n.ChangeID != id || n.Kind != persistence.ApprovalKindCredentialSlot || n.Status != "failed" {
		t.Fatalf("notification = %+v", n)
	}
}

func TestAgentAdmin_ListSetupNotificationBufferEvictsOldest(t *testing.T) {
	f := newAgentAdminFixture(t)
	ctx := context.Background()
	for i := 0; i < 105; i++ {
		id := fmt.Sprintf("apr_%03d", i)
		hash := fmt.Sprintf("h-%03d", i)
		if err := f.c.repos.ApproverDevices.CreateRequest(ctx, persistence.AgentApprovalRequestRow{
			ID: id, Namespace: "hermes", Kind: persistence.ApprovalKindCredentialSlot,
			Sentence: "credential", Rendered: []byte(`{}`), RenderedSHA256: hash,
			Status: persistence.ApprovalPending, CreatedAt: f.svc.now(), ExpiresAt: f.svc.now().Add(approverdevice.RequestTTL),
		}); err != nil {
			t.Fatal(err)
		}
		if err := f.svc.devices.Decide(ctx, f.device, id, hash, false); err != nil {
			t.Fatal(err)
		}
	}
	v := setupOf(t, f)
	if len(v.Notifications) != 100 {
		t.Fatalf("notification count = %d, want 100", len(v.Notifications))
	}
	if v.Notifications[0].ChangeID != "apr_005" || v.Notifications[99].ChangeID != "apr_104" {
		t.Fatalf("eviction order first=%+v last=%+v", v.Notifications[0], v.Notifications[99])
	}
	if _, err := f.c.repos.ApproverDevices.GetRequest(ctx, "apr_000"); err != nil {
		t.Fatalf("evicted notification's row is no longer authoritative: %v", err)
	}
}

func pendingCredentialSlot(t *testing.T) (*agentAdminFixture, string) {
	t.Helper()
	f := newAgentAdminFixture(t)
	f.do(agentadmin.VerbCreateProject, agentadmin.CreateProjectInput{Slug: "finance", Purpose: "Monthly finance"})
	f.approve(f.do(agentadmin.VerbAddMCPServer, agentadmin.AddMCPServerInput{Project: "finance", Name: "bank",
		URL: "https://bank.invalid/mcp", Auth: agentadmin.MCPAuthInput{Mode: "static", Credential: "FIO"}}))
	_ = setupOf(t, f) // drain the server-approval notification; callers test the slot event.
	res := f.do(agentadmin.VerbRequestCredential, agentadmin.RequestCredentialInput{Project: "finance", Name: "FIO", Purpose: "read balances", Kind: "secret"})
	return f, requestIDOf(res)
}
