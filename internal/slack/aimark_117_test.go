package slack

// GitHub #117, Art 50(2) step 2 commit 4, row 15 (Art 50 design §14.8 B): a
// Slack message carries its signed mark in the message metadata.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"

	"vornik.io/vornik/internal/aimark"
	"vornik.io/vornik/internal/conversation"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/voice"
)

type slackMarkLedger struct{ rows int }

func (l *slackMarkLedger) Record(context.Context, persistence.AIOutputMark) error {
	l.rows++
	return nil
}
func (l *slackMarkLedger) RecordKey(context.Context, persistence.AIMarkKey) error { return nil }

func TestSlackSend117_MetadataCarriesTheMark(t *testing.T) {
	vts := newSlackVoiceTestServer(t, "audio/mp4", nil)
	defer vts.Close()
	ch := newSlackVoiceChannel(t, vts, VoiceProviders{})
	signer := aimark.NewSigner(make([]byte, 32))
	ledger := &slackMarkLedger{}
	ch.SetAIMarker(aimark.NewMarker(signer, ledger, func() string { return "t" }, nil, nil))
	if _, err := ch.Send(context.Background(), conversation.ChannelMessage{SessionID: "T123/C_test#1700000010.000200", Text: "hello back"}); err != nil {
		t.Fatal(err)
	}
	var body struct {
		Text     string `json:"text"`
		Metadata struct {
			EventType    string `json:"event_type"`
			EventPayload struct {
				Mark string `json:"mark"`
			} `json:"event_payload"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(vts.chatPostMessageBody, &body); err != nil {
		t.Fatal(err)
	}
	if body.Metadata.EventType != "vornik_ai_mark" || body.Text != "hello back" {
		t.Fatalf("posted %s", vts.chatPostMessageBody)
	}
	m, err := aimark.Verify(body.Metadata.EventPayload.Mark, signer.Keys())
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("hello back"))
	if m.SHA256 != hex.EncodeToString(sum[:]) || m.Surface != "slack" || ledger.rows != 1 {
		t.Errorf("mark %+v, ledger rows %d", m, ledger.rows)
	}
}

func TestSlackSend117_NoMarkerNoMetadata(t *testing.T) {
	vts := newSlackVoiceTestServer(t, "audio/mp4", nil)
	defer vts.Close()
	ch := newSlackVoiceChannel(t, vts, VoiceProviders{})
	if _, err := ch.Send(context.Background(), conversation.ChannelMessage{SessionID: "T123/C_test#1700000010.000200", Text: "hi"}); err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	_ = json.Unmarshal(vts.chatPostMessageBody, &body)
	if _, ok := body["metadata"]; ok {
		t.Fatalf("metadata without a marker: %s", vts.chatPostMessageBody)
	}
}

// Row 15 voice (opt-in): an uploaded spoken reply's bytes get a ledger row.
func TestSlackVoice117_RecordsTheAudio(t *testing.T) {
	clip := []byte("M4A-synth-117")
	tts := &fakeSlackTTS{resp: voice.Audio{Bytes: clip, MimeType: "audio/mp4", DurationMs: 3000}}
	vts := newSlackVoiceTestServer(t, "audio/mp4", nil)
	defer vts.Close()
	ch := newSlackVoiceChannel(t, vts, VoiceProviders{TTS: tts, SyntheticReplies: true})
	ledger := &slackHashLedger{}
	ch.SetAIMarker(aimark.NewMarker(aimark.NewSigner(make([]byte, 32)), ledger, func() string { return "t" }, nil, nil))
	sessionID := "T123/C_test#1700000010.000200"
	ch.voiceTracker.MarkVoice(sessionID)
	if _, err := ch.Send(context.Background(), conversation.ChannelMessage{SessionID: sessionID, Text: "hello back"}); err != nil {
		t.Fatal(err)
	}
	if vts.uploadPostHits != 1 {
		t.Fatalf("uploads %d, want the voice reply", vts.uploadPostHits)
	}
	sum := sha256.Sum256(clip)
	if !ledger.has(hex.EncodeToString(sum[:])) {
		t.Fatalf("no ledger row for the clip: %v", ledger.hashes)
	}
}

type slackHashLedger struct{ hashes []string }

func (l *slackHashLedger) Record(_ context.Context, m persistence.AIOutputMark) error {
	l.hashes = append(l.hashes, m.ContentSHA256)
	return nil
}
func (l *slackHashLedger) RecordKey(context.Context, persistence.AIMarkKey) error { return nil }
func (l *slackHashLedger) has(h string) bool {
	for _, x := range l.hashes {
		if x == h {
			return true
		}
	}
	return false
}
