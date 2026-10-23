package service

// GitHub #395 — the content-store redaction seams are installed in EVERY branch
// and counted (chat-audit retention and redaction design §9.2.1). Before the
// change, secrets.enabled=false or a detector build failure left
// chat_audit_log, chat_system_prompts and step_prompts undecorated and
// uncounted, and task_messages was never decorated at all.
// Found at: review (the #344 security follow-up). This is the wiring half;
// the writer-level regressions run against real SQLite in dispatcher and api.

import (
	"context"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/auditredact"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite"
	"vornik.io/vornik/internal/persistence/sqlite/sqlitetest"
	"vornik.io/vornik/internal/storage"
)

// contentSeamContainer has every store the seam covers, over real SQLite.
func contentSeamContainer(t *testing.T, enabled bool) *Container {
	t.Helper()
	db := sqlitetest.Memory(t)
	c := &Container{Logger: zerolog.Nop(), Config: &config.Config{}}
	c.Config.Secrets.Enabled = enabled
	c.repos = &storage.Repositories{
		ToolAudit:   &nopToolAudit{},
		ChatAudit:   sqlite.NewChatAuditRepository(db.DB),
		Messages:    sqlite.NewTaskMessageRepository(db.DB),
		StepPrompts: sqlite.NewStepPromptRepository(db.DB),
		Tasks:       sqlite.NewTaskRepository(db.DB),
	}
	return c
}

func TestContentSeams395_DecoratedAndCountedWhenScanningIsOff(t *testing.T) {
	c := contentSeamContainer(t, false)
	c.initToolAuditRedaction()

	ca, ok := c.repos.ChatAudit.(*auditredact.ChatAudit)
	require.True(t, ok, "chat audit left undecorated when scanning is off: %T", c.repos.ChatAudit)
	require.Equal(t, auditredact.ReasonSecretsDisabled, ca.BypassReason())
	tm, ok := c.repos.Messages.(*auditredact.TaskMessages)
	require.True(t, ok, "task_messages left undecorated: %T", c.repos.Messages)
	require.Equal(t, auditredact.ReasonSecretsDisabled, tm.BypassReason())
	sp, ok := c.repos.StepPrompts.(*auditredact.StepPrompts)
	require.True(t, ok, "step_prompts left undecorated: %T", c.repos.StepPrompts)
	require.Equal(t, auditredact.ReasonSecretsDisabled, sp.BypassReason())
	require.Nil(t, c.exchangeRedactor(), "a bypassed step-prompt store offers no redactor")

	// A write is counted as skipped, with the reason.
	ctx := context.Background()
	require.NoError(t, c.repos.Tasks.Create(ctx, &persistence.Task{ID: "t1", ProjectID: "p", Status: persistence.TaskStatusRunning, MaxAttempts: 1, Payload: []byte(`{}`)}))
	require.NoError(t, c.repos.Messages.Insert(ctx, &persistence.TaskMessage{TaskID: "t1", AuthorKind: persistence.TaskMessageAuthorOperator, MessageKind: persistence.TaskMessageKindMessage, Content: "hi"}))
	require.NoError(t, c.repos.ChatAudit.Insert(ctx, &persistence.ChatAuditEntry{ID: "c1", UserMessage: "hi", ToolCallsJSON: "[]"}))
	tot := c.auditRedactMetrics.SeamTotals()
	require.Equal(t, int64(1), tot[auditredact.StoreTaskMessages].Skipped)
	require.Equal(t, int64(1), tot[auditredact.StoreChatAuditLog].Skipped)

	// The doctor's snapshot names every store present.
	reason, stores := c.redactionSeamsState()
	require.Equal(t, auditredact.ReasonSecretsDisabled, reason)
	require.Len(t, stores, 4)
}

func TestContentSeams395_DetectorFailureIsAnError(t *testing.T) {
	c := contentSeamContainer(t, true)
	c.Config.Secrets.Patterns.Custom = []config.SecretsPatternConfig{{Name: "broken", Regex: "("}}
	c.initToolAuditRedaction()
	reason, _ := c.redactionSeamsState()
	require.Equal(t, auditredact.ReasonDetectorUnavailable, reason)
	require.Equal(t, auditredact.ReasonDetectorUnavailable, c.repos.Messages.(*auditredact.TaskMessages).BypassReason())
}

// The rebuild re-runs the wiring: a scanning decorator is not wrapped twice,
// and a bypass is upgraded by wrapping its Inner(), never nested.
func TestContentSeams395_IdempotentAndUpgradesABypass(t *testing.T) {
	c := contentSeamContainer(t, false)
	c.initToolAuditRedaction()
	firstChat := c.repos.ChatAudit
	c.initToolAuditRedaction()
	require.Same(t, firstChat, c.repos.ChatAudit, "a same-reason bypass is kept, not rebuilt")

	c.Config.Secrets.Enabled = true
	c.initToolAuditRedaction()
	for name, dec := range map[string]interface {
		BypassReason() auditredact.Reason
	}{
		"chat":     c.repos.ChatAudit.(*auditredact.ChatAudit),
		"messages": c.repos.Messages.(*auditredact.TaskMessages),
		"steps":    c.repos.StepPrompts.(*auditredact.StepPrompts),
	} {
		require.Equal(t, auditredact.ReasonNone, dec.BypassReason(), "%s bypass was not upgraded", name)
	}
	_, nested := c.repos.ChatAudit.(*auditredact.ChatAudit).Inner().(*auditredact.ChatAudit)
	require.False(t, nested, "the upgrade nested the seam over the bypass")
	_, nested = c.repos.Messages.(*auditredact.TaskMessages).Inner().(*auditredact.TaskMessages)
	require.False(t, nested)
	_, nested = c.repos.StepPrompts.(*auditredact.StepPrompts).Inner().(*auditredact.StepPrompts)
	require.False(t, nested)
	require.NotNil(t, c.exchangeRedactor())

	scanning := c.repos.Messages
	c.initToolAuditRedaction()
	require.Same(t, scanning, c.repos.Messages, "a scanning decorator must not be wrapped again")

	reason, stores := c.redactionSeamsState()
	require.Equal(t, auditredact.ReasonNone, reason)
	require.Equal(t, "chat_audit_log,chat_system_prompts,step_prompts,task_messages", strings.Join(stores, ","))
}

// The Postgres rebuild replaces every handle with an undecorated one; the
// re-run must decorate all of them again (review 12ae S4).
func TestContentSeams395_RebuildIsRedecorated(t *testing.T) {
	c := contentSeamContainer(t, true)
	c.initToolAuditRedaction()
	rebuilt := contentSeamContainer(t, true)
	c.repos = rebuilt.repos
	c.initToolAuditRedaction()
	_, ok := c.repos.Messages.(*auditredact.TaskMessages)
	require.True(t, ok)
	_, ok = c.repos.ChatAudit.(*auditredact.ChatAudit)
	require.True(t, ok)
	_, ok = c.repos.StepPrompts.(*auditredact.StepPrompts)
	require.True(t, ok)
}

func TestWorseReasonOrdersBySeverity(t *testing.T) {
	require.Equal(t, auditredact.ReasonDetectorUnavailable, worseReason(auditredact.ReasonSecretsDisabled, auditredact.ReasonDetectorUnavailable))
	require.Equal(t, auditredact.ReasonSecretsDisabled, worseReason(auditredact.ReasonSecretsDisabled, auditredact.ReasonNone))
	require.Equal(t, auditredact.ReasonDetectorNil, worseReason(auditredact.ReasonNone, auditredact.ReasonDetectorNil))
}
