package telegram

// GitHub #543 (Art 50 disclosure design §13): with synthetic replies not
// opted in, a chat whose last inbound was a voice note gets a text reply and
// nothing is synthesised. Found at: design (§12.6.1 M5), observed at
// production; tested through Channel.Send against the Telegram API stub.

import (
	"context"
	"testing"

	"vornik.io/vornik/internal/conversation"
	"vornik.io/vornik/internal/voice"
)

func TestChannelSend543_SyntheticRepliesOffSendsText(t *testing.T) {
	tts := &fakeTTS{resp: voice.Audio{Bytes: []byte("OggS-synth"), MimeType: "audio/ogg", DurationMs: 3000}}
	vts := newVoiceTestServer(t, nil)
	defer vts.Close()
	bot := newVoiceBot(t, vts, VoiceProviders{TTS: tts}) // SyntheticReplies not set: the default
	bot.voiceTracker = newVoiceInboundTracker()
	bot.voiceTracker.MarkVoice(100)

	if _, err := NewChannel(bot).Send(context.Background(), conversation.ChannelMessage{SessionID: "100", Text: "hello back"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if tts.calls != 0 || vts.sendVoiceHits != 0 {
		t.Errorf("synthesised or sent voice while off: TTS calls %d, sendVoice hits %d", tts.calls, vts.sendVoiceHits)
	}
	if vts.sendMsgHits != 1 {
		t.Errorf("sendMessage hits = %d, want 1 (the text reply)", vts.sendMsgHits)
	}
}
