package registry

import (
	"strings"
	"testing"
)

// GitHub #869 (reasoning control design B2): a role may set reasoning to off,
// low, medium or high. Anything else is refused at load, so a typo cannot
// silently leave a model reasoning at its default.
//
// Found at: production (companion reviews failed on the output cap).
// Regression test at: unit. Lower because: the value is validated here.
func TestSwarmValidate_Reasoning_869(t *testing.T) {
	for _, tc := range []struct{ level, wantErr string }{
		{"", ""}, {"off", ""}, {"low", ""}, {"medium", ""}, {"high", ""},
		{"none", "reasoning"}, {"LOW", "reasoning"}, {"max", "reasoning"},
	} {
		s := &Swarm{ID: "s", Roles: []SwarmRole{{Name: "reviewer", Runtime: SwarmRoleRuntime{Image: "img"}, Reasoning: tc.level}}}
		err := s.Validate("s.md")
		switch {
		case tc.wantErr == "" && err != nil:
			t.Errorf("%q: want valid, got %v", tc.level, err)
		case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
			t.Errorf("%q: want an error mentioning %q, got %v", tc.level, tc.wantErr, err)
		}
	}
}
