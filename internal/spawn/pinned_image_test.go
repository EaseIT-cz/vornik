package spawn

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// The EaseIT-cz migration (2026-10-02-easeit-org-migration-design.md §5.2):
// the pinned agent image is the canonical repository (and its local build
// name). The legacy name is NOT pinned: every spawn passes through
// runtime.QualifyAgentImage, which maps it, so a legacy name reaching argv
// means a seam was missed, and refusing it is louder than running the stale
// image the old repository still holds.
func TestPinnedAgentImageIsTheCanonicalRepository(t *testing.T) {
	for ref, want := range map[string]bool{
		"ghcr.io/easeit-cz/vornik-agent:latest":       true,
		"ghcr.io/easeit-cz/vornik-agent@sha256:aa":    true,
		"ghcr.io/easeit-cz/vornik-agent:sha-12345678": true,
		"localhost/vornik-agent:latest":               true,
		"ghcr.io/grinco/vornik-agent:latest":          false,
		"ghcr.io/easeit-cz/other:latest":              false,
		"alpine":                                      false,
	} {
		if got := IsPinnedAgentImage(ref); got != want {
			t.Errorf("IsPinnedAgentImage(%q) = %v, want %v", ref, got, want)
		}
	}
}

// Review 20261002-8004 F1: a legacy name reaching argv is refused by the
// spawn law itself, before any pull, and the refusal names the cause, a seam
// runtime.QualifyAgentImage did not see, so an operator is not left reading
// a registry error.
func TestPodmanAgent_LegacyImageRefusedWithTheMissedSeamNamed(t *testing.T) {
	c, err := PodmanAgent(context.Background(), "", []string{"run", "--detach", "ghcr.io/grinco/vornik-agent:latest"})
	if c != nil || !errors.Is(err, ErrRefused) {
		t.Fatalf("legacy image must be refused by the spawn law, got %v %v", c, err)
	}
	if !strings.Contains(err.Error(), "QualifyAgentImage") || !strings.Contains(err.Error(), "legacy") {
		t.Fatalf("refusal does not name the missed seam: %v", err)
	}
}
