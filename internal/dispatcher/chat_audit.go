package dispatcher

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"vornik.io/vornik/internal/auditredact"
	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/hallucination"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/secrets"
)

// chatAuditTurn collects the per-turn signal during Process /
// ProcessStreaming and writes one chat_audit_log row at the end.
// nil-safe: when a.chatAuditRepo is unset every method on this
// struct is a no-op.
type chatAuditTurn struct {
	agent        *Agent
	startedAt    time.Time
	systemPrompt string
	userMessage  string
	model        string
	roleUsed     string
	toolCalls    []chatAuditToolCall
	iterations   int
	tokensIn     int
	tokensOut    int
	costUSD      float64
	// id is pre-allocated at turn start so tasks created mid-turn
	// (via the create_task tool) can stamp the same id on
	// tasks.chat_turn_id BEFORE the audit row exists. Insert at the
	// end of the turn uses this id rather than generating a fresh
	// one — keeping the task → audit-row link stable even when the
	// audit insert later fails (e.g. UTF-8 truncation).
	id string
	// hallucinationSignals accumulates EVERY scan the dispatcher
	// runs against the reply candidate, including the initial scan
	// + any retry scan. Persisted as JSON on the audit row when
	// non-empty so the /ui/admin/chat-audit page can surface what
	// the detector flagged. Empty (the common case) means no
	// detector fired on this turn.
	hallucinationSignals []hallucination.Signal
}

// chatAuditWriteTimeout bounds the detached chat-audit write in finish().
// Generous enough for a contended Postgres yet short enough that a deferred
// write cannot wedge the turn's teardown.
const chatAuditWriteTimeout = 10 * time.Second

// chatTurnIDContextKey is the context key under which the dispatcher
// stashes the current turn's chat_audit_log.id. Unexported — readers
// use ChatTurnIDFromContext.
type chatTurnIDContextKey struct{}

// WithChatTurnID returns a derived context carrying the given chat
// turn id. No-op when id is empty so non-chat call paths (API
// retention, autonomous scheduler) don't accidentally propagate a
// stale value.
func WithChatTurnID(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, chatTurnIDContextKey{}, id)
}

// ChatTurnIDFromContext returns the chat_audit_log.id stashed by
// WithChatTurnID, or empty string when this code path isn't running
// inside a dispatcher turn (API callers, autonomous tasks).
func ChatTurnIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	v, _ := ctx.Value(chatTurnIDContextKey{}).(string)
	return v
}

// chatAuditToolCall is the serialisable per-tool-call summary.
// Args + Result are cut to keep row size bounded — by the redaction seam,
// AFTER it scans them (auditredact.ToolFieldLimit; chat-audit design §9.2.5).
// Operators who need the full payload can correlate by ts+chat_id with
// tool_audit_log (full) when that's wired for chat surfaces.
type chatAuditToolCall struct {
	Name   string `json:"name"`
	Args   string `json:"args,omitempty"`
	Result string `json:"result,omitempty"`
	Error  string `json:"error,omitempty"`
}

func newChatAuditTurn(a *Agent) *chatAuditTurn {
	if a == nil || a.chatAuditRepo == nil {
		return nil
	}
	return &chatAuditTurn{
		agent:     a,
		startedAt: time.Now().UTC(),
		id:        persistence.GenerateID("chat"),
	}
}

// captureRequest stamps the request-side fields (system prompt,
// user message, role label). Called once per turn after the agent
// has resolved the prompt.
func (t *chatAuditTurn) captureRequest(systemPrompt, userMessage, roleUsed string) {
	if t == nil {
		return
	}
	t.systemPrompt = systemPrompt
	t.userMessage = userMessage
	t.roleUsed = roleUsed
}

// recordIteration bumps the iteration counter + accrues token /
// cost totals. Called after every LLM round-trip.
func (t *chatAuditTurn) recordIteration(resp *chat.ChatResponse, model string, costUSD float64) {
	if t == nil || resp == nil {
		return
	}
	t.iterations++
	if model != "" {
		t.model = model
	}
	t.tokensIn += resp.Usage.PromptTokens
	t.tokensOut += resp.Usage.CompletionTokens
	t.costUSD += costUSD
}

// recordHallucinationSignals appends one scan's signals to the
// turn's running list. The dispatcher's two scan sites (initial +
// retry) both call this so the audit row carries the FULL signal
// trace, not just the last scan. Nil-safe + empty-safe; called
// unconditionally from agent_process.go so a future addition of a
// third scan site doesn't need to remember to plumb through here.
func (t *chatAuditTurn) recordHallucinationSignals(signals []hallucination.Signal) {
	if t == nil || len(signals) == 0 {
		return
	}
	t.hallucinationSignals = append(t.hallucinationSignals, signals...)
}

// recordToolCall captures one tool invocation's name + args/result/error,
// each capped at the redaction seam's scan WINDOW, not the stored limit: the
// seam scans the window and then cuts (chat-audit design §9.2.5, GitHub #395).
// result is what the MODEL saw — the output guard's rewrite (§9.2.4) — not the
// raw tool content. Called from the dispatcher's tool loop.
func (t *chatAuditTurn) recordToolCall(name, args, result, errStr string) {
	if t == nil {
		return
	}
	t.toolCalls = append(t.toolCalls, chatAuditToolCall{
		Name:   name,
		Args:   auditredact.CutBeforeScan(redactSensitiveToolArgs(args), auditredact.ToolWindow(auditredact.ToolFieldLimit)),
		Result: auditredact.CutBeforeScan(result, auditredact.ToolWindow(auditredact.ToolFieldLimit)),
		Error:  auditredact.CutBeforeScan(errStr, auditredact.ToolWindow(auditredact.ToolErrorLimit)),
	})
}

// redactSensitiveToolArgs replaces the value under every secret-NAMED key of a
// tool-call args JSON object, at any depth, with [REDACTED:secret_field] before
// the args reach the audit log. It runs whatever secrets.enabled says, because
// some of these are capabilities that must never render on /ui/admin/chat-audit
// (web_submit's approval_token and daemon_auth: "the raw token is never
// rendered in the UI"); the audit records the LLM's ORIGINAL tool-call args,
// so redaction happens here rather than in the tool.
//
// The key rule is secrets.IsSecretKeyName — the one the Art 15 export and the
// task_messages seam use — replacing a two-key list that matched nothing else
// (chat-audit design §9.2.5, GitHub #395). If args is not a JSON object it is
// returned unchanged (nothing to key on); the seam's detector still scans it.
func redactSensitiveToolArgs(args string) string {
	trimmed := strings.TrimSpace(args)
	if !strings.HasPrefix(trimmed, "{") {
		return args
	}
	var m map[string]any
	dec := json.NewDecoder(strings.NewReader(trimmed))
	dec.UseNumber()
	if err := dec.Decode(&m); err != nil {
		return args
	}
	if !redactSecretNamedKeys(m) {
		return args
	}
	out, err := json.Marshal(m)
	if err != nil {
		return args
	}
	return string(out)
}

// redactSecretNamedKeys replaces, in place, the whole value under every
// secret-named key of v at any depth, and reports whether it changed anything.
// An empty or null value is left alone: there is nothing to hide.
func redactSecretNamedKeys(v any) bool {
	changed := false
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			if secrets.IsSecretKeyName(k) && val != nil && val != "" {
				x[k] = "[REDACTED:secret_field]"
				changed = true
				continue
			}
			if redactSecretNamedKeys(val) {
				changed = true
			}
		}
	case []any:
		for _, val := range x {
			if redactSecretNamedKeys(val) {
				changed = true
			}
		}
	}
	return changed
}

// finish persists the audit row. Pass the Request (for chat_id +
// project) and Result (for the final response text). Safe to call
// even when the turn errored; the response field carries the
// error string in that case so operators see what went wrong.
func (t *chatAuditTurn) finish(ctx context.Context, req Request, result Result) {
	if t == nil || t.agent == nil || t.agent.chatAuditRepo == nil {
		return
	}
	// DETACH from the turn's context before writing (2026-08-05 incident).
	//
	// finish() is invoked from a `defer` on the SAME context the turn ran
	// under, so a turn that exhausted its deadline hands this function an
	// already-done context and every write fails with "context deadline
	// exceeded". The failure mode is worst for exactly the turns that most
	// need auditing: the slow ones.
	//
	// This row is not merely an audit trail — it is the ONLY durable record
	// of a task's originating channel. chatorigin.ResolveForTurn looks up
	// tasks.chat_turn_id in chat_audit_log to decide where to deliver a
	// finished task, so losing the row makes the deliverable permanently
	// unsendable: the narrator's completion push and the UI's "Send to
	// chat" button both conclude the task never came from a chat at all.
	// Nothing backfills it. A ten-minute Slack turn on family-companion lost
	// its report this way.
	//
	// WithoutCancel keeps the context's values (tracing spans, logger) while
	// dropping cancellation; the fresh timeout bounds the write so a
	// detached call cannot hang the deferred path forever. Same pattern as
	// api/chat_proxy.go's telemetryCtx.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), chatAuditWriteTimeout)
	defer cancel()

	// Persist the prompt body; the repository returns its sha256 hex digest,
	// and the row references that so the body is stored once across every
	// turn that used it.
	//
	// The hash used to be computed HERE, which put it ahead of the
	// repository's redaction seam — the stored body could be redacted while
	// the hash named the raw bytes. The repository hashes what it stores
	// (chat-audit retention and redaction design §3.1).
	hash := ""
	if t.systemPrompt != "" {
		// The hash is kept even when the write fails — see the chat proxy's
		// twin for why (it is the row's only reconciliation pointer).
		var perr error
		if hash, perr = t.agent.chatAuditRepo.SavePrompt(ctx, t.systemPrompt); perr != nil {
			t.agent.logger.Debug().Err(perr).Str("hash", hash).
				Msg("chat_audit: SavePrompt failed (best-effort; hash still recorded)")
		}
	}

	responseText := result.Text
	if result.Err != nil && responseText == "" {
		responseText = "error: " + result.Err.Error()
	}

	toolCallsJSON, _ := json.Marshal(t.toolCalls)

	// Hallucination signals — JSON-encode the accumulated list when
	// non-empty so the audit row carries the detector trace. Empty
	// list collapses to "" rather than "[]" because the column
	// renderer (chat_audit_log.hallucination_signals_json) uses
	// presence/absence as the badge gate; "[]" would render as a
	// false-positive badge ("signals fired" with zero rows).
	hallucinationJSON := ""
	if len(t.hallucinationSignals) > 0 {
		if b, err := json.Marshal(t.hallucinationSignals); err == nil {
			hallucinationJSON = string(b)
		}
	}

	entry := &persistence.ChatAuditEntry{
		ID:        t.id,
		Timestamp: time.Now().UTC(),
		ChatID:    resolveChatID(req),
		// WHO said it. Populated by the channel receiver as
		// "<source>:<speaker>" (telegram:42, slack:U123) and simply never
		// copied here, so every chat_audit_log row carried an empty user_id —
		// 54,537 of them in a single 30-day window on the reference
		// deployment. The column, the model field and the value all existed;
		// only the assignment was missing.
		//
		// That made every chat channel invisible to per-person reporting:
		// Telegram, Slack and email usage could be counted in total but never
		// attributed, which for an operator whose team works mostly in chat is
		// the majority of their engagement.
		//
		// Empty for synthesised turns (autonomy, retry-from-step, post-mortem
		// builders) because those have no speaker — correct, and distinct from
		// "a person spoke and we failed to record them".
		UserID:           req.OperatorID,
		ProjectID:        req.Project,
		RoleUsed:         t.roleUsed,
		Model:            t.model,
		SystemPromptHash: hash,
		// Capped at the scan window; the seam scans, then cuts to
		// auditredact.ChatTextLimit (chat-audit design §9.2.5, GitHub #395).
		UserMessage:              auditredact.CutBeforeScan(t.userMessage, auditredact.ChatTextWindow),
		ToolCallsJSON:            string(toolCallsJSON),
		Response:                 auditredact.CutBeforeScan(responseText, auditredact.ChatTextWindow),
		Iterations:               t.iterations,
		DurationMs:               time.Since(t.startedAt).Milliseconds(),
		CostUSD:                  t.costUSD,
		HallucinationSignalsJSON: hallucinationJSON,
	}
	// Best-effort: a failed audit insert must not kill the chat
	// reply. Log via the logger we already have so the operator
	// still sees the miss in journald.
	if err := t.agent.chatAuditRepo.Insert(ctx, entry); err != nil {
		t.agent.logger.Warn().
			Err(err).
			Str("chat_id", entry.ChatID).
			Msg("chat_audit: insert failed")
	}
}

// modelFromResponse extracts the model identifier from a chat
// response. The provider may surface it as Model on the response;
// some return empty when the upstream didn't echo it back.
func modelFromResponse(resp *chat.ChatResponse) string {
	if resp == nil {
		return ""
	}
	return resp.Model
}

// resolveChatID picks the most-specific identifier for the audit
// row's chat_id column. Three buckets, in priority order:
//
//  1. Telegram-style int64 ChatID — kept as a bare numeric string
//     for back-compat with existing UI filters and operator
//     muscle memory.
//  2. OriginatingChannel + OriginatingSessionID — formatted as
//     "<channel>:<sessionID>" (e.g. "email:<thread-msgid>" or
//     "web-chat:<cookie-uuid>"). Set by ChannelReceiver in commit
//     ebc304e for the per-channel followup wiring; reusing it
//     here gives every non-Telegram conversation a meaningful
//     chat_id without a new field on Request.
//  3. Empty string — synthesised internal turn (autonomy loop,
//     retry-from-step, post-mortem builder). Same as pre-fix
//     behaviour for these paths.
//
// Origin: 2026-05-21 defect report — email chats showed an empty
// chat_id column so operators couldn't tell which inbound thread
// produced which audit row.
func resolveChatID(req Request) string {
	if req.ChatID != 0 {
		return formatChatID(req.ChatID)
	}
	if req.OriginatingChannel != "" && req.OriginatingSessionID != "" {
		return req.OriginatingChannel + ":" + req.OriginatingSessionID
	}
	return ""
}

// formatChatID stringifies the chat ID for storage. Telegram chat
// IDs are int64; future channels may use opaque strings, in which
// case the dispatcher will need to accept a string ID directly.
func formatChatID(id int64) string {
	if id == 0 {
		return ""
	}
	var b strings.Builder
	if id < 0 {
		b.WriteString("-")
		id = -id
	}
	// Avoid strconv for the same reason fmt.Sprintf would work:
	// keep this file's import list narrow.
	digits := make([]byte, 0, 20)
	for id > 0 {
		digits = append(digits, byte('0'+id%10))
		id /= 10
	}
	if len(digits) == 0 {
		digits = []byte{'0'}
	}
	for i := len(digits) - 1; i >= 0; i-- {
		b.WriteByte(digits[i])
	}
	return b.String()
}
