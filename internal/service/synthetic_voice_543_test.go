package service

// GitHub #543 (Art 50 disclosure design §13): synthetic voice replies are on
// only when accepted, and the acceptance is recorded once in the admin audit
// at startup. Ignored or off, nothing is written. Found at: design (§12.6.1
// M5), observed at production.

import (
	"context"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/storage"
	"vornik.io/vornik/internal/voice"
)

type memAdminAudit543 struct {
	rows []*persistence.AdminAuditEntry
}

func (m *memAdminAudit543) Insert(_ context.Context, e *persistence.AdminAuditEntry) error {
	m.rows = append(m.rows, e)
	return nil
}

func (m *memAdminAudit543) List(context.Context, persistence.AdminAuditFilter) ([]*persistence.AdminAuditEntry, error) {
	return m.rows, nil
}

type nopTTS543 struct{}

func (nopTTS543) Synthesize(context.Context, string, voice.TTSOptions) (voice.Audio, error) {
	return voice.Audio{}, nil
}

func TestResolveSyntheticVoice543(t *testing.T) {
	cases := []struct {
		name    string
		v       config.VoiceConfig
		tts     voice.TTSProvider
		want    bool
		wantRow bool
	}{
		{"off by default", config.VoiceConfig{}, nopTTS543{}, false, false},
		{"asked without ack: ignored", config.VoiceConfig{SyntheticReplies: true, SyntheticRepliesAckBasis: "b"}, nopTTS543{}, false, false},
		{"accepted", config.VoiceConfig{SyntheticReplies: true, SyntheticRepliesAck: true, SyntheticRepliesAckBasis: "operator accepts metadata + ledger only"}, nopTTS543{}, true, true},
		{"accepted but no TTS: nothing to send", config.VoiceConfig{SyntheticReplies: true, SyntheticRepliesAck: true, SyntheticRepliesAckBasis: "b"}, nil, false, false},
	}
	for _, c := range cases {
		audit := &memAdminAudit543{}
		ctr := &Container{Logger: zerolog.Nop(), Config: &config.Config{Voice: c.v}, repos: &storage.Repositories{AdminAudit: audit}, voiceTTS: c.tts}
		ctr.resolveSyntheticVoice(context.Background())
		if ctr.syntheticVoice != c.want {
			t.Errorf("%s: syntheticVoice = %v, want %v", c.name, ctr.syntheticVoice, c.want)
		}
		if got := len(audit.rows) > 0; got != c.wantRow {
			t.Fatalf("%s: admin-audit rows = %d, want a row: %v", c.name, len(audit.rows), c.wantRow)
		}
		if c.wantRow {
			r := audit.rows[0]
			if r.Action != "synthetic_voice.accepted" || !strings.Contains(r.After, "operator accepts metadata + ledger only") {
				t.Errorf("%s: row = %+v", c.name, r)
			}
		}
	}
}
