package registry

import (
	"testing"
	"time"
)

// WallClockCap is the one maxWallClock rule every reader shares (actionable-
// proposals design §12, review 53a1 F3). Its semantics are the executor's, so
// a value the executor would not arm arms nothing anywhere.
func TestWallClockCap_IsTheExecutorsRule(t *testing.T) {
	cases := []struct {
		raw     string
		want    time.Duration
		wantErr bool
	}{
		{"", 0, false},
		{"20m", 20 * time.Minute, false},
		{"0s", 0, false},
		{"-5m", 0, false},
		{"soon", 0, true},
		// Not trimmed: the executor parses as written and does not arm this.
		{" 20m", 0, true},
	}
	for _, c := range cases {
		got, err := WallClockCap(c.raw)
		if got != c.want || (err != nil) != c.wantErr {
			t.Errorf("WallClockCap(%q) = %v, %v; want %v, err=%v", c.raw, got, err, c.want, c.wantErr)
		}
	}
}

// The validator used to trim before parsing, so it warned against a padded cap
// the executor never armed — the skew the shared rule removes.
func TestWallClockFindings_APaddedCapTheExecutorIgnoresWarnsNothing(t *testing.T) {
	wf := &Workflow{MaxWallClock: " 20m", Steps: map[string]WorkflowStep{"s": {Timeout: "30m"}}}
	var r WorkflowMDValidationReport
	appendWallClockFindings(&r, wf)
	if len(r.Findings) != 0 {
		t.Fatalf("warned against a cap the executor does not arm: %+v", r.Findings)
	}
}
