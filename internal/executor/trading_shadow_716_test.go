package executor

import (
	"testing"

	"vornik.io/vornik/internal/registry"
)

// GitHub #716: the shadow engine takes the regime threshold from the project.
func TestEnginePolicy_TakesTheProjectThreshold_716(t *testing.T) {
	p := &registry.Project{}
	if got := enginePolicy(p).BlockLongAtScore; got != -1 {
		t.Fatalf("unset: %d, want -1", got)
	}
	v := -2
	p.Trading.Regime.BlockLongAtScore = &v
	if got := enginePolicy(p).BlockLongAtScore; got != -2 {
		t.Fatalf("set: %d, want -2", got)
	}
}
