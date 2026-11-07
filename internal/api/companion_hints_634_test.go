package api

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// GitHub #634 (companion hardening design §13 H1): the hint catalogue the
// daemon serves and both bundles embed. Before the change no catalogue
// existed: the only moment-of-need hint was the recall nudge.

func TestCompanionHints_CatalogueValid_634(t *testing.T) {
	cat, err := CompanionHints()
	if err != nil {
		t.Fatalf("the embedded catalogue does not load: %v", err)
	}
	if len(cat.Hints) < 3 {
		t.Fatalf("%d hints, want at least the three active seeds", len(cat.Hints))
	}
	active := map[string]bool{}
	for _, h := range cat.Hints {
		if h.Active {
			active[h.ID] = true
		}
	}
	// §13.1: these three ship active; the others are muted until measured.
	for _, id := range []string{"commit.review", "design_written.ingest", "test_failed.recall"} {
		if !active[id] {
			t.Errorf("hint %s must ship active", id)
		}
	}
}

func TestCompanionHints_ValidateRefusesBadEntries_634(t *testing.T) {
	ok := CompanionHint{ID: "commit.x", Version: 1, Trigger: HintTrigger{Kind: "commit"}, Follow: []string{"recall"}, Text: "short"}
	cases := map[string]func(h *CompanionHint){
		"unknown kind":                 func(h *CompanionHint) { h.Trigger.Kind = "pushed" },
		"design_written without globs": func(h *CompanionHint) { h.Trigger.Kind = "design_written" },
		"test_failed without runners":  func(h *CompanionHint) { h.Trigger.Kind = "test_failed" },
		"no follow":                    func(h *CompanionHint) { h.Follow = nil },
		"bad follow":                   func(h *CompanionHint) { h.Follow = []string{"delegate:"} },
		"unknown follow":               func(h *CompanionHint) { h.Follow = []string{"memory_list"} },
		"text over 300 bytes":          func(h *CompanionHint) { h.Text = strings.Repeat("é", 151) },
		"empty text":                   func(h *CompanionHint) { h.Text = " " },
		"no id":                        func(h *CompanionHint) { h.ID = "" },
		"version zero":                 func(h *CompanionHint) { h.Version = 0 },
		"unknown placeholder":          func(h *CompanionHint) { h.Text = "use {memory}" },
		"duplicate follow":             func(h *CompanionHint) { h.Follow = []string{"recall", "recall"} },
	}
	if err := (HintCatalogue{Hints: []CompanionHint{ok}}).Validate(); err != nil {
		t.Fatalf("a valid hint was refused: %v", err)
	}
	for name, mutate := range cases {
		h := ok
		h.Follow = append([]string(nil), ok.Follow...)
		mutate(&h)
		if err := (HintCatalogue{Hints: []CompanionHint{h}}).Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := (HintCatalogue{Hints: []CompanionHint{ok, ok}}).Validate(); err == nil {
		t.Error("a duplicate id was accepted")
	}
}

// §13 H3: both bundles embed the catalogue as hooks/hints.json, equal to the
// daemon's. VORNIK_UPDATE_HINTS=1 rewrites the copies.
func TestCompanionHints_BundleCopiesEqual_634(t *testing.T) {
	want, err := CompanionHintsJSON()
	if err != nil {
		t.Fatal(err)
	}
	for _, bundle := range []string{"claude-code-companion", "codex-companion"} {
		p := filepath.Join("..", "..", "contrib", bundle, "hooks", "hints.json")
		if os.Getenv("VORNIK_UPDATE_HINTS") == "1" {
			if err := os.WriteFile(p, want, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		got, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("%s: %v (run with VORNIK_UPDATE_HINTS=1 to write it)", p, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s differs from internal/api/companion_hints.yaml; run with VORNIK_UPDATE_HINTS=1", p)
		}
	}
}
