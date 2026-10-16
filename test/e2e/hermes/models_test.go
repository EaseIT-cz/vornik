package hermes

import (
	"encoding/hex"
	"encoding/json"
	"path"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// GitHub #381: gate candidates are added to laneModels as pins. A pin that
// cannot be downloaded or verified fails only when the real-model lane runs
// on the CI runner VM, an hour into a run; these checks fail it in the unit
// suite instead. They check a pin's SHAPE only: that the URL serves a file
// with this sha256 is checked by ensureModel when the lane runs, and the
// limit arithmetic by TestLaneModelLimitsCoverTheFile.
func TestLaneModels_PinsAreWellFormed(t *testing.T) {
	for _, id := range append([]string{defaultLaneModel, "qwen3.6-35b-a3b", "gemma-4-26b-a4b"}, revisionPinned...) {
		if _, ok := laneModels[id]; !ok {
			t.Errorf("no pin %q: the gate and its #381 candidates must stay pinned", id)
		}
	}
	for id, m := range laneModels {
		if m.Name == "" {
			t.Errorf("%s: no model name for Hermes", id)
		}
		if !strings.HasPrefix(m.URL, "https://huggingface.co/") || !strings.Contains(m.URL, "/resolve/") {
			t.Errorf("%s: URL %q is not a pinned Hugging Face file", id, m.URL)
		}
		if path.Base(m.URL) != m.File {
			t.Errorf("%s: File %q is not the URL's file %q", id, m.File, path.Base(m.URL))
		}
		if b, err := hex.DecodeString(m.SHA256); err != nil || len(b) != 32 || m.SHA256 != strings.ToLower(m.SHA256) {
			t.Errorf("%s: SHA256 %q is not a lower-case sha256", id, m.SHA256)
		}
	}
}

// llama.cpp b11610 parses --chat-template-kwargs as a JSON object and
// refuses a string-typed enable_thinking ("invalid type for
// \"enable_thinking\" (expected boolean, got string)",
// tools/server/server-common.cpp), on every request. A quoted "false" would
// fail every turn of the lane, so the pins are checked here.
func TestLaneModels_ChatTemplateKwargsAreAJSONObject(t *testing.T) {
	examined := 0
	for id, m := range laneModels {
		for i, a := range m.Args {
			if a != "--chat-template-kwargs" {
				continue
			}
			examined++
			if i+1 >= len(m.Args) {
				t.Errorf("%s: --chat-template-kwargs has no value", id)
				continue
			}
			var kw map[string]any
			if err := json.Unmarshal([]byte(m.Args[i+1]), &kw); err != nil {
				t.Errorf("%s: --chat-template-kwargs %q is not a JSON object: %v", id, m.Args[i+1], err)
				continue
			}
			if v, ok := kw["enable_thinking"]; ok {
				if _, isBool := v.(bool); !isBool {
					t.Errorf("%s: enable_thinking is %T, llama.cpp wants a boolean", id, v)
				}
			}
		}
	}
	if examined == 0 {
		t.Fatal("no --chat-template-kwargs examined")
	}
}

// revisionPinned are the #381 round-2 candidates (lane design, amendment
// 2026-10-15, "Round 2"). Their URLs name a Hugging Face commit, not main:
// a re-upload under main would change the bytes behind the pin, and a
// candidate is measured against one file.
var revisionPinned = []string{"nemotron-3.5-lightning-30b-a3b", "north-mini-code-1.0"}

var hfCommitURL = regexp.MustCompile(`^https://huggingface\.co/[^/]+/[^/]+/resolve/[0-9a-f]{40}/[^/]+$`)

func TestLaneModels_RoundTwoPinsNameACommit(t *testing.T) {
	for _, id := range revisionPinned {
		m, ok := laneModels[id]
		if !ok {
			t.Errorf("no pin %q (#381 round 2)", id)
			continue
		}
		if !hfCommitURL.MatchString(m.URL) {
			t.Errorf("%s: URL %q does not resolve a 40-hex commit", id, m.URL)
		}
	}
}

// Each candidate runs with thinking off, and the switch must be the variable
// ITS chat template reads: a server kwarg the template never reads leaves
// thinking on, and Hermes's request reasoning_effort "medium" then decides.
// Qwen3.6, Gemma 4 and Nemotron 3.5 read enable_thinking; North-Mini-Code's
// Cohere2 template reads reasoning, and falls back to reasoning_effort only
// when reasoning is undefined (lane design, amendment 2026-10-15, Round 2).
// Every candidate also bounds llama-server's host-memory caches, which its
// memory limit counts.
func TestLaneModels_CandidatesSwitchThinkingOffByTheirTemplatesVariable(t *testing.T) {
	want := map[string]string{
		"qwen3.6-35b-a3b":                "enable_thinking",
		"gemma-4-26b-a4b":                "enable_thinking",
		"nemotron-3.5-lightning-30b-a3b": "enable_thinking",
		"north-mini-code-1.0":            "reasoning",
	}
	for id, variable := range want {
		m, ok := laneModels[id]
		if !ok {
			t.Errorf("no pin %q", id)
			continue
		}
		i := slices.Index(m.Args, "--chat-template-kwargs")
		if i < 0 || i+1 >= len(m.Args) {
			t.Errorf("%s: no --chat-template-kwargs", id)
			continue
		}
		var kw map[string]any
		if err := json.Unmarshal([]byte(m.Args[i+1]), &kw); err != nil {
			t.Errorf("%s: kwargs %q: %v", id, m.Args[i+1], err)
			continue
		}
		if v, ok := kw[variable].(bool); !ok || v {
			t.Errorf("%s: kwargs %q do not set %s to false", id, m.Args[i+1], variable)
		}
		for k := range kw {
			if k != variable {
				t.Errorf("%s: kwarg %q is not the variable its template reads (%s)", id, k, variable)
			}
		}
		if n := len(boundedServerCaches); len(m.Args) < n || !slices.Equal(m.Args[len(m.Args)-n:], boundedServerCaches) {
			t.Errorf("%s: args %q do not end in boundedServerCaches; its memory limit counts them", id, m.Args)
		}
	}
	// Each candidate's OverheadBytes counts 8 checkpoints and a 2 GiB prompt
	// cache (models.go comments, lane design). A change to these values
	// without re-deriving every candidate's overhead would OOM a lane an hour
	// in (run gemma-r0oom); review 6c09 finding 1.
	if want := []string{"--ctx-checkpoints", "8", "--cache-ram", "2048"}; !slices.Equal(boundedServerCaches, want) {
		t.Errorf("boundedServerCaches is %q, want %q: re-derive every candidate's OverheadBytes before changing it", boundedServerCaches, want)
	}
}
