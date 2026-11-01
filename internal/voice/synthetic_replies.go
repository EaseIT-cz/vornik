package voice

import "strings"

// ResolveSyntheticReplies decides whether a channel may answer with
// synthesised speech (GitHub #543; Art 50 disclosure design §13). It is true
// only when voice.synthetic_replies, voice.synthetic_replies_ack and a
// non-empty voice.synthetic_replies_ack_basis are all set: until an audio
// watermark ships, synthetic audio carries metadata and the ledger only, and
// an operator must accept that. Asking for it without the acknowledgement is
// ignored, with a warning naming what is missing, as
// retention.ResolveToolAuditFloor treats its override.
func ResolveSyntheticReplies(enabled, ack bool, basis string) (bool, string) {
	if !enabled {
		return false, ""
	}
	if !ack {
		return false, "voice.synthetic_replies ignored: set voice.synthetic_replies_ack: true to accept that synthetic audio " +
			"carries metadata and the ledger only (no watermark yet), and voice.synthetic_replies_ack_basis to say why; replies stay text"
	}
	if strings.TrimSpace(basis) == "" {
		return false, "voice.synthetic_replies ignored: voice.synthetic_replies_ack_basis is empty — state who accepted " +
			"single-layer marking for synthetic audio and why; replies stay text"
	}
	return true, ""
}
