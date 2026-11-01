package github

// GitHub #117, Art 50(2): the GitHub conversation channel's replies on issues
// and pull requests carry the hidden signed mark, as the forge provider's do.
// Found by the marking coverage law (Art 50 design §14.8, commit 6).

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"vornik.io/vornik/internal/aimark"
	"vornik.io/vornik/internal/conversation"
	"vornik.io/vornik/internal/persistence"
)

type ghChanLedger struct{ rows int }

func (l *ghChanLedger) Record(context.Context, persistence.AIOutputMark) error { l.rows++; return nil }
func (l *ghChanLedger) RecordKey(context.Context, persistence.AIMarkKey) error { return nil }

func TestGitHubChannel117_ReplyCarriesTheMark(t *testing.T) {
	stub := newGithubAPIStub()
	defer stub.Close()
	ch := outboundChannel(t, stub)
	signer := aimark.NewSigner(make([]byte, 32))
	ledger := &ghChanLedger{}
	ch.SetAIMarker(aimark.NewMarker(signer, ledger, func() string { return "t" }, nil, nil))
	if _, err := ch.Send(context.Background(), conversation.ChannelMessage{SessionID: "acme/api#issues/42", Text: "hello from vornik"}); err != nil {
		t.Fatal(err)
	}
	var v struct{ Body string }
	if err := json.Unmarshal([]byte(stub.lastCommentBody), &v); err != nil {
		t.Fatal(err)
	}
	text, compact, ok := strings.Cut(v.Body, "\n\n<!-- vornik-ai-mark: ")
	if !ok {
		t.Fatalf("no mark: %q", v.Body)
	}
	compact = strings.TrimSuffix(compact, " -->")
	m, err := aimark.Verify(compact, signer.Keys())
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(text))
	if text != "hello from vornik" || m.SHA256 != hex.EncodeToString(sum[:]) || ledger.rows != 1 {
		t.Errorf("text %q, mark %+v, rows %d", text, m, ledger.rows)
	}
}
