package steering

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/conversation"
	"vornik.io/vornik/internal/persistence"
)

func newOperatorAlert(t *testing.T, ch *fakeChannel, cfg OperatorAlertConfig, enabled bool) (*OperatorAlertNotifier, *fakeChannel) {
	t.Helper()
	return newOperatorAlertR(t, ch, nil, cfg, enabled)
}

// newOperatorAlertR builds a notifier with an explicit per-project recipient
// resolver (nil = fall back to cfg.Session).
func newOperatorAlertR(t *testing.T, ch *fakeChannel, recipients ProjectRecipients, cfg OperatorAlertConfig, enabled bool) (*OperatorAlertNotifier, *fakeChannel) {
	t.Helper()
	res := fakeResolver{byName: map[string]conversation.Channel{}}
	if ch != nil {
		res.byName[ch.name] = ch
	}
	return NewOperatorAlert(res, recipients, nil, "https://vornik.example", cfg, enabled, zerolog.Nop()), ch
}

// fakeRecipients maps projectID → session IDs for the per-project fan-out test.
type fakeRecipients map[string][]string

func (f fakeRecipients) RecipientsForProject(_, projectID string) []string { return f[projectID] }

func autonomyTask() *persistence.Task {
	return &persistence.Task{
		ID:             "task_auto",
		ProjectID:      "p1",
		CreationSource: persistence.TaskCreationSourceAutonomous,
		// no ChatTurnID — ownerless
	}
}

func TestOperatorAlert_NotifiesOwnerlessAutonomyApproval(t *testing.T) {
	ch := &fakeChannel{name: "telegram"}
	n, tg := newOperatorAlert(t, ch, OperatorAlertConfig{Channel: "telegram", Session: "555"}, true)

	n.NotifySteeringRequired(context.Background(), autonomyTask(), string(persistence.TaskStatusAwaitingApproval))

	if len(tg.sent) != 1 {
		t.Fatalf("want 1 send to the operator recipient, got %d", len(tg.sent))
	}
	m := tg.sent[0]
	if m.SessionID != "555" {
		t.Errorf("SessionID = %q, want the configured operator session 555", m.SessionID)
	}
	if !strings.Contains(m.Text, "task_auto") {
		t.Errorf("alert text missing task id: %q", m.Text)
	}
	// Deep link must be the canonical /ui/tasks/{id} route; the nested
	// /ui/projects/{p}/tasks/{id} form 404s in the browser (operator-reported
	// 2026-07-08).
	if !strings.Contains(m.Text, "https://vornik.example/ui/tasks/task_auto") {
		t.Errorf("alert missing canonical deep link /ui/tasks/task_auto: %q", m.Text)
	}
	if strings.Contains(m.Text, "/ui/projects/") {
		t.Errorf("alert deep link must not use the 404-ing nested form: %q", m.Text)
	}
}

func TestOperatorAlert_SkipsChatOriginatedTask(t *testing.T) {
	ch := &fakeChannel{name: "telegram"}
	n, tg := newOperatorAlert(t, ch, OperatorAlertConfig{Channel: "telegram", Session: "555"}, true)

	task := autonomyTask()
	task.ChatTurnID = turnID("chat_1") // has an originating chat → the chat notifier owns it
	n.NotifySteeringRequired(context.Background(), task, string(persistence.TaskStatusAwaitingApproval))

	if len(tg.sent) != 0 {
		t.Fatalf("chat-originated task must not get the operator fallback alert (double-notify)")
	}
}

func TestOperatorAlert_SkipsNonAutonomyOwnerless(t *testing.T) {
	ch := &fakeChannel{name: "telegram"}
	n, tg := newOperatorAlert(t, ch, OperatorAlertConfig{Channel: "telegram", Session: "555"}, true)

	task := autonomyTask()
	task.CreationSource = persistence.TaskCreationSourceUser // API-created, no chat
	n.NotifySteeringRequired(context.Background(), task, string(persistence.TaskStatusAwaitingApproval))

	if len(tg.sent) != 0 {
		t.Fatalf("non-autonomy ownerless task must not trigger the operator alert")
	}
}

// TestOperatorAlert_NotifiesRoutedSubtask is the reported gap: an autonomy
// task's routed CHILD (source=route, no ChatTurnID) checkpoints, and the chat
// notifier can't reach it. The operator-alert must now catch it.
func TestOperatorAlert_NotifiesRoutedSubtask(t *testing.T) {
	ch := &fakeChannel{name: "telegram"}
	n, tg := newOperatorAlert(t, ch, OperatorAlertConfig{Channel: "telegram", Session: "555"}, true)

	task := autonomyTask()
	task.ID = "task_routed_child"
	task.CreationSource = persistence.TaskCreationSourceRoute
	n.NotifySteeringRequired(context.Background(), task, string(persistence.TaskStatusAwaitingInput))

	if len(tg.sent) != 1 {
		t.Fatalf("routed ownerless sub-task checkpoint must alert the operator, got %d", len(tg.sent))
	}
}

// TestOperatorAlert_SkipsChatOriginatedViaAncestor is the 2026-07-08 fix: a
// ROUTE/CHECKPOINT child of a task that WAS scheduled from a chat carries only
// ParentTaskID. With the ancestry walk wired, the operator-alert must treat it
// as chat-owned and SKIP (the chat Notifier routes it back to the chat),
// instead of firing the generic "No chat originated it" operator alert.
func TestOperatorAlert_SkipsChatOriginatedViaAncestor(t *testing.T) {
	ch := &fakeChannel{name: "telegram"}
	res := fakeResolver{byName: map[string]conversation.Channel{"telegram": ch}}

	parentID := "parent-chat"
	parent := &persistence.Task{ID: parentID, ProjectID: "p1", ChatTurnID: turnID("chat_1")}
	child := autonomyTask()
	child.ID = "task_routed_child"
	child.CreationSource = persistence.TaskCreationSourceRoute
	child.ParentTaskID = &parentID // no own ChatTurnID
	getter := fakeTaskGetter{byID: map[string]*persistence.Task{parentID: parent}}

	n := NewOperatorAlert(res, nil, getter, "https://vornik.example",
		OperatorAlertConfig{Channel: "telegram", Session: "555"}, true, zerolog.Nop())
	n.NotifySteeringRequired(context.Background(), child, string(persistence.TaskStatusAwaitingInput))

	if len(ch.sent) != 0 {
		t.Fatalf("child of a chat-scheduled task must NOT get the generic operator alert (chat notifier owns it); got %d", len(ch.sent))
	}
}

// TestOperatorAlert_PerProjectFanOut: alerts route to the operators with
// access to the task's project (assistant → wildcard only; family → wildcard +
// family-scoped), not a single fixed session.
func TestOperatorAlert_PerProjectFanOut(t *testing.T) {
	ch := &fakeChannel{name: "telegram"}
	recips := fakeRecipients{
		"assistant": {"559741208"},
		"family":    {"559741208", "8019099642"},
	}
	n, tg := newOperatorAlertR(t, ch, recips, OperatorAlertConfig{Channel: "telegram", Session: "fallback"}, true)

	assistantTask := autonomyTask()
	assistantTask.ID, assistantTask.ProjectID = "task_a", "assistant"
	n.NotifySteeringRequired(context.Background(), assistantTask, string(persistence.TaskStatusAwaitingInput))

	familyTask := autonomyTask()
	familyTask.ID, familyTask.ProjectID = "task_j", "family"
	n.NotifySteeringRequired(context.Background(), familyTask, string(persistence.TaskStatusAwaitingInput))

	got := map[string]int{}
	for _, m := range tg.sent {
		got[m.SessionID]++
	}
	// assistant → 1 (wildcard only); family → 2 (wildcard + scoped).
	if got["559741208"] != 2 { // wildcard operator gets both
		t.Errorf("wildcard operator should get both projects' alerts, got %d", got["559741208"])
	}
	if got["8019099642"] != 1 { // family-scoped only for family
		t.Errorf("family-scoped operator should get exactly family's alert, got %d", got["8019099642"])
	}
	if got["fallback"] != 0 {
		t.Errorf("fallback session must not be used when per-project recipients resolve")
	}
}

// TestOperatorAlert_FallbackWhenNoProjectRecipients: with a resolver that
// returns nobody for the project, the configured fallback session is used.
func TestOperatorAlert_FallbackWhenNoProjectRecipients(t *testing.T) {
	ch := &fakeChannel{name: "telegram"}
	n, tg := newOperatorAlertR(t, ch, fakeRecipients{}, OperatorAlertConfig{Channel: "telegram", Session: "fallback"}, true)

	n.NotifySteeringRequired(context.Background(), autonomyTask(), string(persistence.TaskStatusAwaitingInput))

	if len(tg.sent) != 1 || tg.sent[0].SessionID != "fallback" {
		t.Fatalf("must fall back to the configured session when no project recipients resolve: %+v", tg.sent)
	}
}

func TestOperatorAlert_SkipsWhenNoRecipientConfigured(t *testing.T) {
	ch := &fakeChannel{name: "telegram"}
	n, tg := newOperatorAlert(t, ch, OperatorAlertConfig{}, true) // no channel/session

	n.NotifySteeringRequired(context.Background(), autonomyTask(), string(persistence.TaskStatusAwaitingApproval))

	if len(tg.sent) != 0 {
		t.Fatalf("no recipient configured must be a no-op, got %d sends", len(tg.sent))
	}
}

func TestOperatorAlert_Disabled(t *testing.T) {
	ch := &fakeChannel{name: "telegram"}
	n, tg := newOperatorAlert(t, ch, OperatorAlertConfig{Channel: "telegram", Session: "555"}, false)

	n.NotifySteeringRequired(context.Background(), autonomyTask(), string(persistence.TaskStatusAwaitingApproval))

	if len(tg.sent) != 0 {
		t.Fatalf("disabled notifier must not send")
	}
}

func TestOperatorAlert_Dedup(t *testing.T) {
	ch := &fakeChannel{name: "telegram"}
	n, tg := newOperatorAlert(t, ch, OperatorAlertConfig{Channel: "telegram", Session: "555"}, true)

	task := autonomyTask()
	n.NotifySteeringRequired(context.Background(), task, string(persistence.TaskStatusAwaitingApproval))
	n.NotifySteeringRequired(context.Background(), task, string(persistence.TaskStatusAwaitingApproval))

	if len(tg.sent) != 1 {
		t.Fatalf("duplicate (task,state) within the dedup window must send once, got %d", len(tg.sent))
	}
}

func TestNotifyOperator_SendsFreeFormAlert(t *testing.T) {
	ch := &fakeChannel{name: "telegram"}
	n, tg := newOperatorAlert(t, ch, OperatorAlertConfig{Channel: "telegram", Session: "555"}, true)
	n.NotifyOperator(context.Background(), "cluster: endpoint down", "public-webhook-ingress unreachable")
	if len(tg.sent) != 1 {
		t.Fatalf("want 1 operator alert, got %d", len(tg.sent))
	}
	if m := tg.sent[0]; m.SessionID != "555" || !strings.Contains(m.Text, "endpoint down") {
		t.Errorf("alert not addressed/worded correctly: %+v", m)
	}
}

func TestNotifyOperator_NoopWhenUnconfigured(t *testing.T) {
	ch := &fakeChannel{name: "telegram"}
	n, tg := newOperatorAlert(t, ch, OperatorAlertConfig{}, true) // no recipient
	n.NotifyOperator(context.Background(), "x", "y")
	if len(tg.sent) != 0 {
		t.Fatalf("unconfigured recipient must be a no-op")
	}
}

func TestOperatorAlert_EmailRecipientGetsToAndSubject(t *testing.T) {
	ch := &fakeChannel{name: "email"}
	n, em := newOperatorAlert(t, ch, OperatorAlertConfig{Channel: "email", Session: "ops-thread", Address: "ops@example.com"}, true)

	n.NotifySteeringRequired(context.Background(), autonomyTask(), string(persistence.TaskStatusAwaitingApproval))

	if len(em.sent) != 1 {
		t.Fatalf("want 1 email send, got %d", len(em.sent))
	}
	cs := em.sent[0].ChannelSpecific
	if cs["to"] != "ops@example.com" {
		t.Errorf("email To = %q, want ops@example.com", cs["to"])
	}
	if cs["subject"] == "" {
		t.Errorf("email alert must carry a subject")
	}
}

// TestOperatorAlert_EveryOutcomeIsCounted — GitHub #369, outcome-quality LLD
// S8.11 row 12. The package had no metrics: a missing outbound or recipient
// logged at Debug and a send failure Warned uncounted, so an operator alert
// meant to go that could not was invisible. Each outcome now counts
// vornik_steering_operator_alert_total{kind,outcome}, and no_outbound /
// no_recipients rise to Warn (a misconfiguration).
func TestOperatorAlert_EveryOutcomeIsCounted(t *testing.T) {
	delta := func(kind, outcome string, fn func()) float64 {
		before := testutil.ToFloat64(operatorAlertTotal.WithLabelValues(kind, outcome))
		fn()
		return testutil.ToFloat64(operatorAlertTotal.WithLabelValues(kind, outcome)) - before
	}
	approval := string(persistence.TaskStatusAwaitingApproval)
	build := func(ch *fakeChannel, cfg OperatorAlertConfig, buf *bytes.Buffer) *OperatorAlertNotifier {
		res := fakeResolver{byName: map[string]conversation.Channel{}}
		if ch != nil {
			res.byName[ch.name] = ch
		}
		return NewOperatorAlert(res, nil, nil, "", cfg, true, zerolog.New(buf))
	}
	steer := func(n *OperatorAlertNotifier) {
		n.NotifySteeringRequired(context.Background(), autonomyTask(), approval)
	}
	oper := func(n *OperatorAlertNotifier) { n.NotifyOperator(context.Background(), "s", "b") }
	tg := OperatorAlertConfig{Channel: "telegram", Session: "555"}

	cases := []struct {
		name, kind, outcome string
		ch                  *fakeChannel
		cfg                 OperatorAlertConfig
		call                func(*OperatorAlertNotifier)
		wantWarn            bool
	}{
		{"steering sent", "steering", "sent", &fakeChannel{name: "telegram"}, tg, steer, false},
		{"steering no outbound", "steering", "no_outbound", nil, tg, steer, true},
		{"steering no recipients", "steering", "no_recipients", &fakeChannel{name: "telegram"},
			OperatorAlertConfig{Channel: "telegram"}, steer, true},
		{"steering send failed", "steering", "send_failed",
			&fakeChannel{name: "telegram", sendErr: errors.New("429")}, tg, steer, true},
		{"operator sent", "operator", "sent", &fakeChannel{name: "telegram"}, tg, oper, false},
		{"operator no outbound", "operator", "no_outbound", nil, tg, oper, true},
		{"operator send failed", "operator", "send_failed",
			&fakeChannel{name: "telegram", sendErr: errors.New("429")}, tg, oper, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var buf bytes.Buffer
			n := build(c.ch, c.cfg, &buf)
			if got := delta(c.kind, c.outcome, func() { c.call(n) }); got != 1 {
				t.Errorf("operator_alert_total{kind=%q,outcome=%q} delta = %v, want 1", c.kind, c.outcome, got)
			}
			if c.wantWarn && !strings.Contains(buf.String(), `"level":"warn"`) {
				t.Errorf("outcome %q must log at Warn; got %q", c.outcome, buf.String())
			}
		})
	}
}
