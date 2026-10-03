package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/voice"
)

// Incident 2026-10-03 12:52 (voice-messages-design.md §8): an audio clip
// failed transcription and the daemon's log carried nothing about the
// clip, its format, size or duration, and its bytes were not kept, so
// the cause could not be found. A failure now logs the clip's format
// facts, and never its audio or any transcript.

// lockedBuffer is a bytes.Buffer safe for the logger's writes.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func voiceUpdateJSON(t *testing.T, msg string) *Update {
	t.Helper()
	var upd Update
	if err := json.Unmarshal([]byte(msg), &upd); err != nil {
		t.Fatal(err)
	}
	return &upd
}

// sendFailingAudio runs one inbound through a bot whose STT fails with
// sttErr, returning the log and the reply text.
func sendFailingAudio(t *testing.T, sttErr error, payload []byte, update string) (logged, reply string) {
	t.Helper()
	vts := newVoiceTestServer(t, payload)
	defer vts.Close()
	bot := newVoiceBot(t, vts, VoiceProviders{STT: &fakeSTT{err: sttErr}})
	logs := &lockedBuffer{}
	bot.logger = zerolog.New(logs)
	rcv := &handleMessageReceiver{done: make(chan struct{}, 1)}
	bot.SetReceiver(rcv)
	if err := bot.HandleUpdate(context.Background(), voiceUpdateJSON(t, update)); err != nil {
		t.Fatalf("HandleUpdate: %v", err)
	}
	if rcv.waitReceive(t, 100*time.Millisecond) {
		t.Fatal("the dispatcher must not run on an STT failure")
	}
	return logs.String(), vts.sendMsgText
}

func failureLine(t *testing.T, logged string) map[string]any {
	t.Helper()
	for _, line := range strings.Split(strings.TrimSpace(logged), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) == nil && m["message"] == "voice: STT.Transcribe failed" {
			return m
		}
	}
	t.Fatalf("no STT failure line in the log:\n%s", logged)
	return nil
}

func TestHandleVoice_TranscribeFailureLogsFormatFacts(t *testing.T) {
	payload := []byte("SECRET-AUDIO-BYTES-the-user-said-this")
	sttErr := fmt.Errorf("voice: whisper.cpp wrote no transcript (tool output: %q)", "error: failed to read audio file '/in/audio.wav'")
	logged, reply := sendFailingAudio(t, sttErr, payload, `{"message":{"message_id":12,"chat":{"id":100},"from":{"id":42},
		"audio":{"file_id":"audio-12","duration":37,"mime_type":"audio/mp4","file_size":4321,"file_name":"clip.m4a"}}}`)
	m := failureLine(t, logged)
	if m["level"] != "warn" {
		t.Errorf("level = %v, want warn", m["level"])
	}
	want := map[string]any{
		"kind":          "audio",
		"mime_type":     "audio/mp4",
		"file_name":     "clip.m4a",
		"bytes":         float64(len(payload)),
		"declared_size": float64(4321),
		"duration_s":    float64(37),
	}
	for k, v := range want {
		if m[k] != v {
			t.Errorf("%s = %v, want %v", k, m[k], v)
		}
	}
	if e, _ := m["error"].(string); !strings.Contains(e, "failed to read audio file") {
		t.Errorf("error = %q, want the tool's message", e)
	}
	if strings.Contains(logged, "SECRET-AUDIO-BYTES") {
		t.Error("audio bytes reached the log")
	}
	if !strings.Contains(reply, "couldn't make out") {
		t.Errorf("reply = %q, want the decode-failure reply", reply)
	}
}

// A recorded voice note has no file name from Telegram; the log says
// which kind it was.
func TestHandleVoice_TranscribeFailureLogsVoiceKind(t *testing.T) {
	logged, _ := sendFailingAudio(t, fmt.Errorf("boom"), []byte("OggS"), `{"message":{"message_id":13,"chat":{"id":100},"from":{"id":42},
		"voice":{"file_id":"voice-13","duration":4,"mime_type":"audio/ogg","file_size":4}}}`)
	m := failureLine(t, logged)
	if m["kind"] != "voice" || m["mime_type"] != "audio/ogg" || m["file_name"] != "voice.ogg" || m["duration_s"] != float64(4) {
		t.Errorf("voice failure line = %v", m)
	}
}

// A file with no audio stream gets a reply that says so, not "try again".
func TestHandleVoice_NoAudioStreamSaysSo(t *testing.T) {
	sttErr := fmt.Errorf("voice: ffmpeg normalise failed: %w", voice.ErrNoAudioStream)
	_, reply := sendFailingAudio(t, sttErr, []byte("\x89PNG"), `{"message":{"message_id":14,"chat":{"id":100},"from":{"id":42},
		"audio":{"file_id":"audio-14","duration":0,"mime_type":"audio/mpeg","file_name":"cover.mp3"}}}`)
	if !strings.Contains(reply, "no audio I can read") {
		t.Errorf("reply = %q, want the no-audio reply", reply)
	}
	if strings.Contains(reply, "couldn't make out") {
		t.Errorf("reply = %q must not be the decode-failure reply", reply)
	}
}

func TestVoiceAttachmentFacts(t *testing.T) {
	if k, d, s := voiceAttachmentFacts(&TelegramVoice{FileID: "v", Duration: 3, FileSize: 9}, &TelegramAudio{FileID: "a"}); k != "voice" || d != 3 || s != 9 {
		t.Errorf("voice wins: %s %d %d", k, d, s)
	}
	if k, d, s := voiceAttachmentFacts(nil, &TelegramAudio{FileID: "a", Duration: 5, FileSize: 7}); k != "audio" || d != 5 || s != 7 {
		t.Errorf("audio: %s %d %d", k, d, s)
	}
	if k, d, s := voiceAttachmentFacts(&TelegramVoice{}, nil); k != "" || d != 0 || s != 0 {
		t.Errorf("none: %s %d %d", k, d, s)
	}
}
