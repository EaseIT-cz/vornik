package agentbench

// GitHub #818: CheckComparableExcept paired fieldPairs by index. A bare arm
// carries one more pair (scaffold) than a swarm arm, so with the swarm arm
// first the scaffold pair was never compared (a swarm and a bare arm
// compared clean under any declared axis), and with the bare arm first the
// index ran past the end. Found at: review (the #809 design), observed at
// production (bench agent compare and the release gate). Regression test at:
// unit; Lower because: compare is a pure function of two journals' arm
// fields, so the unit is where both orders can be driven.

import (
	"strings"
	"testing"
)

func TestCompareByName818_SwarmAndBareRefusedInBothOrders(t *testing.T) {
	swarm := baseArm()
	bare := baseArm()
	bare.Scaffold = ScaffoldBarePass1
	bare.BinarySHA256 = "other-binary"
	for _, pair := range [][2]ArmFields{{swarm, bare}, {bare, swarm}} {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("CheckComparableExcept panicked: %v", r)
				}
			}()
			err := CheckComparableExcept(pair[0], pair[1], []string{"binary_sha256"})
			if err == nil || !strings.Contains(err.Error(), "scaffold") {
				t.Fatalf("a swarm and a bare arm under a declared binary_sha256: %v, want a refusal on scaffold", err)
			}
		}()
	}
	// The declared axis alone still compares.
	other := baseArm()
	other.BinarySHA256 = "other-binary"
	if err := CheckComparableExcept(swarm, other, []string{"binary_sha256"}); err != nil {
		t.Fatalf("two swarm arms differing only on the declared axis: %v", err)
	}
}
