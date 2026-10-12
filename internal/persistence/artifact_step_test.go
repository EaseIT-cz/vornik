package persistence

import (
	"context"
	"testing"
)

func TestArtifactStep(t *testing.T) {
	ctx := context.Background()
	if ArtifactStep(ctx) != "" {
		t.Fatal("unexpected provenance")
	}
	if got := ArtifactStep(WithArtifactStep(ctx, "answer")); got != "answer" {
		t.Fatal(got)
	}
	if ArtifactStep(ctx) != "" {
		t.Fatal("parent changed")
	}
}
