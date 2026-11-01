package api

// GitHub #543 (Art 50 disclosure design §13): the doctor reports whether
// synthetic voice replies are off (the default), on with an accepted basis,
// or asked for and ignored.

import (
	"strings"
	"testing"

	"vornik.io/vornik/internal/config"
)

func TestDoctorSyntheticVoice543(t *testing.T) {
	cases := []struct {
		name, status, msg string
		v                 config.VoiceConfig
	}{
		{"off", "OK", "off (default)", config.VoiceConfig{}},
		{"on", "WARNING", "no audio watermark): operator accepted it", config.VoiceConfig{SyntheticReplies: true, SyntheticRepliesAck: true, SyntheticRepliesAckBasis: "operator accepted it"}},
		{"ignored", "WARNING", "ignored", config.VoiceConfig{SyntheticReplies: true}},
	}
	for _, c := range cases {
		h := NewDoctorHandlers(nil)
		h.ConfigureSyntheticVoice(&config.Config{Voice: c.v})
		got := h.checkSyntheticVoiceReplies()
		if got.Name != "synthetic_voice_replies" || got.Status != c.status || !strings.Contains(got.Message, c.msg) {
			t.Errorf("%s: got %+v", c.name, got)
		}
	}
	if got := NewDoctorHandlers(nil).checkSyntheticVoiceReplies(); got.Status != "SKIPPED" {
		t.Errorf("unwired: got %+v, want SKIPPED", got)
	}
}
