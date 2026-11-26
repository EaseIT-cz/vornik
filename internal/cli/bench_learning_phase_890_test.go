package cli

// GitHub #890 (agent-benchmark design §12.37, part 2 and 3): a pass checks
// the freeze against its phase, and check 3c learns which promoted file it may
// accept. Found at: production, the #809 2x2 study on 2026-11-23: arm C ran
// 7.5 hours of warm-up frozen, and nothing checked the phase.

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestCheckLearningPhase_890(t *testing.T) {
	notFound := &APIError{StatusCode: http.StatusNotFound, Message: "not found"}
	broken := errors.New("connection refused")
	cases := []struct {
		name      string
		phase     string
		on        bool
		frozen    bool
		readErr   error
		wantError string // "" = pass
	}{
		{name: "baseline arm, no learning route", phase: "measure", on: false, readErr: notFound},
		{name: "baseline arm, warm-up never runs but is harmless", phase: "warmup", on: false, readErr: notFound},
		{name: "learning arm whose gate did not open", phase: "warmup", on: true, readErr: notFound, wantError: "gate did not open"},
		{name: "learning arm, state unreadable", phase: "measure", on: true, readErr: broken, wantError: "cannot read"},
		{name: "warm-up starts unfrozen", phase: "warmup", on: true},
		{name: "warm-up starts frozen (the #890 incident)", phase: "warmup", on: true, frozen: true, wantError: "frozen"},
		{name: "measured pass after the freeze", phase: "measure", on: true, frozen: true},
		{name: "measured pass before the freeze", phase: "measure", on: true, wantError: "freeze first"},
		{name: "unknown phase", phase: "cooldown", on: true, wantError: "phase"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := checkLearningPhase(c.phase, c.on, learnedStateWire{Frozen: c.frozen}, c.readErr)
			if c.wantError == "" {
				if err != nil {
					t.Fatalf("want a pass, got %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.wantError) {
				t.Fatalf("err = %v, want one saying %q", err, c.wantError)
			}
		})
	}
}

func TestLatestPromotedFiles_890(t *testing.T) {
	t1 := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)
	t2 := t1.Add(time.Hour)
	var st learnedStateWire
	st.Promoted = append(st.Promoted,
		promotedWire{WorkflowID: "dev-pipeline", FileSHA256: "old", PromotedAt: &t1},
		promotedWire{WorkflowID: "dev-pipeline", FileSHA256: "new", PromotedAt: &t2},
		promotedWire{WorkflowID: "simple-workflow", FileSHA256: "s1", PromotedAt: &t1},
		promotedWire{WorkflowID: "no-hash", FileSHA256: "", PromotedAt: &t2},
	)
	got := latestPromotedFiles(st)
	want := "dev-pipeline new\nno-hash -\nsimple-workflow s1\n"
	if got != want {
		t.Fatalf("got %q, want %q (the latest promotion per workflow; no hash prints -)", got, want)
	}
}
