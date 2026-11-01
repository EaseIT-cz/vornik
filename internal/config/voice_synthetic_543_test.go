package config

// GitHub #543: voice.synthetic_replies and its acknowledgement load from
// YAML, and the default is off.

import (
	"testing"

	"gopkg.in/yaml.v3"
)

func TestVoiceSyntheticRepliesKeys543(t *testing.T) {
	var off Config
	if err := yaml.Unmarshal([]byte("voice:\n  tts:\n    provider: piper\n"), &off); err != nil {
		t.Fatal(err)
	}
	if off.Voice.SyntheticReplies || off.Voice.SyntheticRepliesAck || off.Voice.SyntheticRepliesAckBasis != "" {
		t.Fatalf("defaults must be off: %+v", off.Voice)
	}
	var on Config
	src := "voice:\n  synthetic_replies: true\n  synthetic_replies_ack: true\n  synthetic_replies_ack_basis: \"operator accepted\"\n"
	if err := yaml.Unmarshal([]byte(src), &on); err != nil {
		t.Fatal(err)
	}
	if !on.Voice.SyntheticReplies || !on.Voice.SyntheticRepliesAck || on.Voice.SyntheticRepliesAckBasis != "operator accepted" {
		t.Fatalf("keys did not load: %+v", on.Voice)
	}
}
