package projectarchive

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// 2026.10.6 delta audit (#334), follow-up to section H: a reload-only
// residue was worded "artifact files may remain" on every surface. The
// wording is per step (section H.5); these pin each branch, including a
// residue joined and wrapped the way deleteProject and sweepOnce carry it.
func TestResidueWarning_PerStep(t *testing.T) {
	art := &ResidueError{Step: ResidueArtifacts, Err: errors.New("artifact wipe: denied")}
	rel := &ResidueError{Step: ResidueReload, Err: errors.New("reload: denied")}
	cases := []struct {
		name      string
		err       error
		steps     []ResidueStep
		want      string
		forbidden string
	}{
		{"artifacts", art, []ResidueStep{ResidueArtifacts}, "project removed; artifact files may remain: artifact wipe: denied", "reload failed"},
		{"reload", errors.Join(nil, rel), []ResidueStep{ResidueReload}, "the registry reload failed, so the archived project stays listed until the sweeper's next tick, which completes the deletion: reload: denied", "artifact"},
		{"both, wrapped", fmt.Errorf("outcome: %w", errors.Join(art, rel)), []ResidueStep{ResidueArtifacts, ResidueReload}, "artifact files may remain and the registry reload failed", ""},
		{"no step", errors.New("something"), nil, "a later deletion step was not done: something", "artifact"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ResidueSteps(tc.err)
			if fmt.Sprint(got) != fmt.Sprint(tc.steps) {
				t.Errorf("ResidueSteps = %v, want %v", got, tc.steps)
			}
			w := ResidueWarning(tc.err)
			if !strings.Contains(w, tc.want) {
				t.Errorf("ResidueWarning = %q, want it to contain %q", w, tc.want)
			}
			if tc.forbidden != "" && strings.Contains(w, tc.forbidden) {
				t.Errorf("ResidueWarning = %q must not contain %q", w, tc.forbidden)
			}
		})
	}
	// Error() keeps the wrapped text, so logs and audit rows read as before.
	if art.Error() != "artifact wipe: denied" || !errors.Is(art, ErrResidueArtifacts) || errors.Is(art, ErrResidueReload) {
		t.Errorf("ResidueError text/Is wrong: %q", art.Error())
	}
}
