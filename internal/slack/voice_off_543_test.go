package slack

// GitHub #543 (Art 50 disclosure design §13): with synthetic replies not
// opted in, a session whose last inbound was an audio clip gets a text reply
// and nothing is synthesised. Found at: design (§12.6.1 M5), observed at
// production; tested through Channel.Send against the Slack API stub.

import (
	"context"
	"testing"

	"vornik.io/vornik/internal/conversation"
	"vornik.io/vornik/internal/voice"
)

func TestSlackSend543_SyntheticRepliesOffSendsText(t *testing.T) {
	tts := &fakeSlackTTS{resp: voice.Audio{Bytes: []byte("M4A-synth"), MimeType: "audio/mp4", DurationMs: 3000}}
	vts := newSlackVoiceTestServer(t, "audio/mp4", nil)
	defer vts.Close()
	ch := newSlackVoiceChannel(t, vts, VoiceProviders{TTS: tts}) // SyntheticReplies not set: the default
	sessionID := "T123/C_test#1700000010.000200"
	ch.voiceTracker.MarkVoice(sessionID)

	if _, err := ch.Send(context.Background(), conversation.ChannelMessage{SessionID: sessionID, Text: "hello back"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if tts.calls != 0 || vts.uploadPostHits != 0 {
		t.Errorf("synthesised or uploaded audio while off: TTS calls %d, uploads %d", tts.calls, vts.uploadPostHits)
	}
	if vts.chatPostMessageHits != 1 {
		t.Errorf("chat.postMessage hits = %d, want 1 (the text reply)", vts.chatPostMessageHits)
	}
}
