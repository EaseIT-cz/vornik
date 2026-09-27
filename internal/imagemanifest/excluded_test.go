package imagemanifest

import "testing"

// TestIsExcludedRequiresAnExplicitEntry: the exclusion list is empty today, so
// the parity walk's short-circuit means nothing else exercises this. Test it
// directly, because the moment someone vendors a Containerfile this function
// decides whether the parity guard fires or stays silent.
func TestIsExcludedRequiresAnExplicitEntry(t *testing.T) {
	if isExcluded("images/vornik-agent/Containerfile") {
		t.Error("an image we build must never read as excluded")
	}
	if isExcluded("some/vendored/Containerfile") {
		t.Error("an unknown path must NOT be excluded by default — defaulting to " +
			"excluded is how a new image ships with no builder and nothing says so")
	}

	// Prove the mechanism works when an entry exists, without mutating the
	// package-level map for other tests.
	saved := excluded
	t.Cleanup(func() { excluded = saved })
	excluded = map[string]string{"some/vendored/Containerfile": "third-party"}
	if !isExcluded("some/vendored/Containerfile") {
		t.Error("an explicit entry must exclude the path")
	}
}
