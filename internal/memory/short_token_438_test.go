package memory

import (
	"strings"
	"testing"

	"github.com/rs/zerolog"
)

// GitHub #438 (design 22, amendment 2026-11-08). Found at review, observable at
// production: a companion remember() of "the staging webhook token is
// <32-char random token>" was stored verbatim and served by recall.
const token438 = "3f9aQ2xK7mPwR4vT8bNc5dJh6sL1uYzA"

func TestSecretScanGate_438_ShortUnlabeledTokenIsRedacted(t *testing.T) {
	c := &IngestCandidate{Content: "the staging webhook token is " + token438 + ", rotate it monthly"}
	out := SecretScanGate(c, GateConfig{SecretsDetector: newSecretsDetector(t)})
	if out.Action != GateRedact || strings.Contains(out.NewContent, token438) {
		t.Fatalf("want the token redacted, got %+v", out)
	}
	if !strings.Contains(out.NewContent, "rotate it monthly") {
		t.Fatalf("the rest of the note must stay: %q", out.NewContent)
	}
}

func TestIndexer_438_ShortTokenRedactedOnIngest(t *testing.T) {
	idx := NewIndexer(Config{}, nil, nil, zerolog.Nop())
	idx.SetSecrets(newSecretsDetector(t), nil)
	got := idx.scanContentForSecrets("p", "t", "notes.md", "webhook token: see vault\nraw "+token438+"\n")
	if strings.Contains(got, token438) {
		t.Fatalf("the indexer stored the token: %q", got)
	}
}
