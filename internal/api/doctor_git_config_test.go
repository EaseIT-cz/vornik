package api

import (
	"strings"
	"testing"
)

// git_config_composition (process-spawn law S6-D4): every state says what it
// examined, and a dropped key is listed with its reason.
func TestCheckGitConfigComposition(t *testing.T) {
	h := &DoctorHandlers{}
	if got := h.checkGitConfigComposition(); got.Status != "SKIPPED" || !strings.Contains(got.Message, "NOT a statement") {
		t.Errorf("unwired: %+v", got)
	}

	h.SetGitConfigComposition(&GitConfigComposition{Err: "git config --global --list: exit 128"})
	if got := h.checkGitConfigComposition(); got.Status != "ERROR" || !strings.Contains(got.Message, "EMPTY global config") {
		t.Errorf("failed composition: %+v", got)
	}

	h.SetGitConfigComposition(&GitConfigComposition{Path: "/d/git/config", Examined: 3, Kept: 3})
	if got := h.checkGitConfigComposition(); got.Status != "OK" || !strings.Contains(got.Message, "examined 3") {
		t.Errorf("all kept: %+v", got)
	}

	h.SetGitConfigComposition(&GitConfigComposition{Path: "/d/git/config", Examined: 4, Kept: 2,
		Dropped: []DroppedGitConfigKey{{Key: "filter.lfs.clean", Reason: "a driver"}, {Key: "core.hookspath", Reason: "guards"}}})
	got := h.checkGitConfigComposition()
	if got.Status != "WARNING" {
		t.Fatalf("dropped keys: %+v", got)
	}
	for _, want := range []string{"examined 4", "dropped 2", "filter.lfs.clean (a driver)", "core.hookspath (guards)", "git lfs install --local"} {
		if !strings.Contains(got.Message, want) {
			t.Errorf("message lacks %q: %s", want, got.Message)
		}
	}
}
