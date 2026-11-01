package api

import (
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/voice"
)

// syntheticVoiceDoctor is the snapshot checkSyntheticVoiceReplies reads.
type syntheticVoiceDoctor struct {
	known    bool
	accepted bool
	warning  string
	basis    string
}

// ConfigureSyntheticVoice snapshots voice.synthetic_replies for the doctor
// (GitHub #543). SetServerConfig calls it.
func (h *DoctorHandlers) ConfigureSyntheticVoice(cfg *config.Config) {
	v := cfg.Voice
	accepted, warning := voice.ResolveSyntheticReplies(v.SyntheticReplies, v.SyntheticRepliesAck, v.SyntheticRepliesAckBasis)
	h.syntheticVoice = syntheticVoiceDoctor{known: true, accepted: accepted, warning: warning, basis: v.SyntheticRepliesAckBasis}
}

// checkSyntheticVoiceReplies reports whether channels may answer a voice
// message with synthesised speech (AI Act Art 50(2); Art 50 disclosure design
// §13). On means single-layer marking (metadata and the ledger, no audio
// watermark) that an operator accepted, so it stays a warning on every run.
func (h *DoctorHandlers) checkSyntheticVoiceReplies() DoctorCheck {
	const name = "synthetic_voice_replies"
	s := h.syntheticVoice
	switch {
	case !s.known:
		return DoctorCheck{Name: name, Status: "SKIPPED", Message: "voice configuration not wired into the doctor"}
	case s.accepted:
		return DoctorCheck{Name: name, Status: "WARNING",
			Message: "on, accepted single-layer marking (metadata and ledger only, no audio watermark): " + s.basis}
	case s.warning != "":
		return DoctorCheck{Name: name, Status: "WARNING", Message: "asked for but ignored, replies stay text: " + s.warning}
	default:
		return DoctorCheck{Name: name, Status: "OK", Message: "off (default): voice messages are answered in text"}
	}
}
