package persistence

import (
	"errors"
	"strings"
	"testing"
)

// GitHub #264 (2026-10-13), review 0df6 F1: the rule on a withdrawal's actor
// and reason lives with the repository, so the daemon and vornikctl are held
// to the same one.
func TestValidWithdrawal(t *testing.T) {
	if got, err := ValidWithdrawal("system:agent-admin-cover", "  Within the limit.  "); err != nil || got != "Within the limit." {
		t.Fatalf("a good withdrawal = %q, %v", got, err)
	}
	if _, err := ValidWithdrawal("system:x", strings.Repeat("é", MaxWithdrawReasonRunes)); err != nil {
		t.Fatalf("a reason at the rune limit was refused: %v", err)
	}
	for name, c := range map[string][2]string{
		"no actor":          {" ", "Reason."},
		"no reason":         {"system:x", ""},
		"long reason":       {"system:x", strings.Repeat("a", MaxWithdrawReasonRunes+1)},
		"newline in reason": {"system:x", "a\nb"},
		"control in actor":  {"system:\x00", "Reason."},
	} {
		if _, err := ValidWithdrawal(c[0], c[1]); !errors.Is(err, ErrBadWithdrawal) {
			t.Errorf("%s: %v, want ErrBadWithdrawal", name, err)
		}
	}
}
