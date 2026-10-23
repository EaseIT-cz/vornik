package dispatcher

// GitHub #414 (3) — hallucination.truncate and findSentence cut by byte slice
// and split a multi-byte rune. Found at: review (#395's §9.2.7). A user
// observes it at integration: the stored hallucination_signals_json shows a
// U+FFFD where encoding/json replaced the broken tail. This test drives the
// real turn writer and the real signal constructors into a real SQLite
// chat_audit_log, through the decorator the container installs, and reads the
// stored row back. Design: chat-audit retention and redaction design §10.2.4.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/hallucination"
)

func TestChatAudit414_SignalCutsDoNotSplitARune(t *testing.T) {
	repo, inner := chatAuditDB395(t)
	a := &Agent{chatAuditRepo: repo, logger: zerolog.Nop()}
	turn := newChatAuditTurn(a)

	// A 4-byte rune straddling each cut: claim_value at 256 bytes, sentence
	// at 240.
	claim := strings.Repeat("c", hallucination.MaxClaimValueBytes-2) + "😀" + " tail"
	sentence := strings.Repeat("s", hallucination.MaxSentenceBytes-1) + "😀" + " tail"
	turn.recordHallucinationSignals([]hallucination.Signal{
		hallucination.NewSignal("tool_claim", hallucination.SeverityWarn, "url", claim, sentence, "", "detail"),
	})
	turn.finish(context.Background(), Request{Project: "p1"}, Result{Text: "ok"})

	row := storedRow395(t, inner, turn.id)
	require.NotEmpty(t, row.HallucinationSignalsJSON)
	require.NotContains(t, row.HallucinationSignalsJSON, "�",
		"a split rune reaches the stored signal as U+FFFD")
	require.NotContains(t, row.HallucinationSignalsJSON, `�`)
	var signals []hallucination.Signal
	require.NoError(t, json.Unmarshal([]byte(row.HallucinationSignalsJSON), &signals))
	require.Len(t, signals, 1)
	require.LessOrEqual(t, len(strings.TrimSuffix(signals[0].ClaimValue, "…")), hallucination.MaxClaimValueBytes)
	require.LessOrEqual(t, len(strings.TrimSuffix(signals[0].Sentence, "…")), hallucination.MaxSentenceBytes)
	require.True(t, strings.HasSuffix(signals[0].ClaimValue, "…"), "the cut is still marked")
}

// GitHub #414 (2): a mail turn's user_id is "<source>:<From address>", which an
// external sender chooses, and the seam never scanned it or chat_id (review
// a68b F7). The real turn writer, the real decorator, real SQLite.
func TestChatAudit414_UserIDAndChatIDAreScanned(t *testing.T) {
	repo, inner := chatAuditDB395(t)
	a := &Agent{chatAuditRepo: repo, logger: zerolog.Nop()}
	turn := newChatAuditTurn(a)
	turn.finish(context.Background(), Request{Project: "p1", OperatorID: "email:" + aws395 + "@x.test",
		OriginatingChannel: "email", OriginatingSessionID: "thread-" + gh395}, Result{Text: "ok"})

	var userID, chatID string
	require.NoError(t, inner.QueryRow(`SELECT user_id, chat_id FROM chat_audit_log WHERE id = ?`, turn.id).Scan(&userID, &chatID))
	assertNoFragment(t, "user_id", userID, aws395)
	assertNoFragment(t, "chat_id", chatID, gh395)
	require.True(t, strings.HasPrefix(userID, "email:"), "the channel prefix survives: %q", userID)
	require.True(t, strings.HasPrefix(chatID, "email:thread-"), "the channel prefix survives: %q", chatID)
}
