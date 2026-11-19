package agentbench

// GitHub #809 (agent-benchmark design, Amendment 2026-11-18 (#809), change 1):
// the learning axis and the config hash without the three learning switches.

import (
	"strings"
	"testing"
)

const cfg809 = `server:
  address: 0.0.0.0:8093
instinct:
  enabled: %s
  cadence_seconds: 1800
bench:
  instance: true
  learning:
    trigger:
      enabled: %s
      threshold: %s
    auto: %s
`

func cfg809With(instincts, trigger, threshold, auto string) []byte {
	s := cfg809
	for _, v := range []string{instincts, trigger, threshold, auto} {
		s = strings.Replace(s, "%s", v, 1)
	}
	return []byte(s)
}

func TestLearning809_TheFourArmsShareOneConfigHash(t *testing.T) {
	var hashes []string
	var learnings []string
	for _, arm := range [][3]string{{"false", "false", "false"}, {"true", "false", "false"}, {"false", "true", "true"}, {"true", "true", "true"}} {
		h, l, err := ArmConfigIdentity(cfg809With(arm[0], arm[1], "0.3", arm[2]))
		if err != nil {
			t.Fatal(err)
		}
		hashes = append(hashes, h)
		learnings = append(learnings, l.String())
	}
	for _, h := range hashes[1:] {
		if h != hashes[0] {
			t.Fatalf("arms that differ only in the learning switches hash differently: %v", hashes)
		}
	}
	want := []string{"", "instincts=on,blackbox=off,auto=off", "instincts=off,blackbox=bench-trigger,auto=on", "instincts=on,blackbox=bench-trigger,auto=on"}
	for i := range want {
		if learnings[i] != want[i] {
			t.Fatalf("arm %d learning = %q, want %q", i, learnings[i], want[i])
		}
	}
}

func TestLearning809_ATuningDifferenceChangesTheHash(t *testing.T) {
	a, _, _ := ArmConfigIdentity(cfg809With("true", "true", "0.3", "true"))
	b, _, _ := ArmConfigIdentity(cfg809With("true", "true", "0.4", "true"))
	if a == b {
		t.Fatal("two arms with different trigger thresholds share a config hash")
	}
	// Key order and comments do not move it.
	c, _, _ := ArmConfigIdentity([]byte("# a comment\nbench:\n  learning:\n    auto: true\n    trigger:\n      threshold: 0.3\n      enabled: true\n  instance: true\ninstinct:\n  cadence_seconds: 1800\n  enabled: true\nserver:\n  address: 0.0.0.0:8093\n"))
	if c != a {
		t.Fatal("reordering keys or adding a comment changed the config hash")
	}
}

func TestLearning809_TheAxisComparesByNameAndDefaultsOff(t *testing.T) {
	a := baseArm()
	b := baseArm()
	b.Learning = Learning{Instincts: true}
	if err := CheckComparableExcept(a, b, []string{"learning"}); err != nil {
		t.Fatalf("A and B under a declared learning axis: %v", err)
	}
	if err := CheckComparableExcept(a, b, []string{"binary_sha256"}); err == nil || !strings.Contains(err.Error(), "learning") {
		t.Fatalf("A and B without declaring learning: %v, want a refusal on learning", err)
	}
	if baseArm().Key() != a.Key() {
		t.Fatal("an all-off arm's key moved")
	}
}
