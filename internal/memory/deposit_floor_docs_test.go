package memory

import (
	"os"
	"strings"
	"testing"
)

// TestDepositFloorStatedInUserTexts pins every user- or model-facing text
// that states the min_content floor for a deliberate deposit (a companion or
// chat note) to the gate's own constants.
//
// #392 (found at review during #380, observed in the published docs): the
// Hermes README, the Hermes setup guide and the vornik_remember tool
// description said Vornik refuses notes "under about ten words", and the
// Claude Code /remember command said "<64 chars / 10 words". The deposit
// floor has been 3 words and 10 characters since 2026-10-01 (RAG ingest
// pipeline design, update 2026-10-01; design 24's per-gate table). A model
// told ten words pads a short fact, the restating #380 was about.
func TestDepositFloorStatedInUserTexts(t *testing.T) {
	spelled := map[int]string{3: "three", 10: "ten"}
	words, ok := spelled[DefaultDepositMinContentWords]
	if !ok {
		t.Fatalf("DefaultDepositMinContentWords is %d: spell it here and correct every text below", DefaultDepositMinContentWords)
	}
	chars, ok := spelled[DefaultDepositMinContentChars]
	if !ok {
		t.Fatalf("DefaultDepositMinContentChars is %d: spell it here and correct every text below", DefaultDepositMinContentChars)
	}
	want := words + " words and " + chars + " characters"

	for _, path := range []string{
		"../../contrib/hermes-companion/README.md",
		"../../contrib/hermes-companion/memory_provider.py",
		"../../contrib/claude-code-companion/commands/remember.md",
		"../../docs/public/guides/hermes-setup.md",
		"../../docs/public/features/memory-rag.md",
	} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		// Collapse line wrapping so a phrase split across lines still matches.
		text := strings.Join(strings.Fields(string(raw)), " ")
		if !strings.Contains(text, want) {
			t.Errorf("%s does not state the deposit floor %q", path, want)
		}
		for _, stale := range []string{"ten words", "64 chars / 10 words"} {
			if strings.Contains(text, stale) {
				t.Errorf("%s still states the old floor %q", path, stale)
			}
		}
	}
}

// TestDepositMinContentDefaultsAreTheConstants: a zero GateConfig falls back
// to the constants the user texts are pinned to, so the texts and the gate
// cannot drift apart through the literals.
func TestDepositMinContentDefaultsAreTheConstants(t *testing.T) {
	deposit := func(content string) *IngestCandidate {
		return &IngestCandidate{Content: content, ProducerRole: "companion:hermes"}
	}
	// Exactly at the floor: DefaultDepositMinContentWords words padded to
	// DefaultDepositMinContentChars runes passes.
	at := strings.TrimSpace(strings.Repeat("ab ", DefaultDepositMinContentWords))
	for len([]rune(at)) < DefaultDepositMinContentChars {
		at += "x"
	}
	if got := MinContentGate(deposit(at), GateConfig{}); got.Action != GateAllow {
		t.Fatalf("%q at the floor: got %v (%s), want allow", at, got.Action, got.Detail)
	}
	fewWords := strings.TrimSpace(strings.Repeat("abcdefghijk ", DefaultDepositMinContentWords-1))
	if got := MinContentGate(deposit(fewWords), GateConfig{}); got.Action != GateReject {
		t.Fatalf("%q under the word floor: got %v, want reject", fewWords, got.Action)
	}
	fewChars := at[:DefaultDepositMinContentChars-1]
	if got := MinContentGate(deposit(fewChars), GateConfig{}); got.Action != GateReject {
		t.Fatalf("%q under the character floor: got %v, want reject", fewChars, got.Action)
	}
}
