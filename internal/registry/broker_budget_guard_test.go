package registry

import (
	"math"
	"strings"
	"testing"
)

// Review 315a finding 2 (2026-10-08, GitHub #96): the walk's arithmetic
// assumes maxLength >= 1, as walkArray already requires maxItems >= 1.
// walkString refuses a smaller maxLength before any arithmetic, and the
// saturating helpers never return a negative figure even if a caller
// breaks that precondition.
func TestBrokerSchema_MaxLengthBelowOneRefusedBeforeArithmetic(t *testing.T) {
	for _, n := range []string{"0", "-1", "-9223372036854775807"} {
		src := brokerWithProps("    a: { type: string, x-untrusted: true, maxLength: " + n + " }")
		err := parseBroker(t, src).Validate()
		if err == nil || !strings.Contains(err.Error(), "maxLength") || strings.Contains(err.Error(), "budget is") {
			t.Fatalf("maxLength %s: %v", n, err)
		}
	}
}

func TestSaturatingHelpers_NeverNegative(t *testing.T) {
	cases := []struct {
		name string
		got  int
	}{
		{"mul negative left", saturatingMul(-2, 3)},
		{"mul negative right", saturatingMul(3, -2)},
		{"add negative", saturatingAdd(5, -10)},
		{"mul overflow", saturatingMul(math.MaxInt/2, 3)},
		{"add overflow", saturatingAdd(math.MaxInt, 1)},
	}
	for _, c := range cases {
		if c.got < 0 {
			t.Errorf("%s = %d, want a non-negative (saturated) figure", c.name, c.got)
		}
	}
	if saturatingMul(4, 5) != 20 || saturatingAdd(4, 5) != 9 {
		t.Fatal("ordinary arithmetic changed")
	}
}
