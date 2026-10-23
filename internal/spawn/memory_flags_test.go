package spawn

import (
	"reflect"
	"testing"
)

// TestMemoryLimitFlags pins the one builder of an agent-image container's
// memory flags (GitHub #379, 2026.10.6 delta audit #334): with --memory alone
// podman gives the container swap up to the limit, so the limit and the swap
// total are always emitted together, and neither when there is no limit.
func TestMemoryLimitFlags(t *testing.T) {
	if got, want := MemoryLimitFlags(4110510080), []string{"--memory=4110510080", "--memory-swap=4110510080"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("MemoryLimitFlags(limit) = %v, want %v", got, want)
	}
	for _, n := range []int64{0, -1} {
		if got := MemoryLimitFlags(n); got != nil {
			t.Fatalf("MemoryLimitFlags(%d) = %v, want nil (unbounded emits no flag)", n, got)
		}
	}
}
