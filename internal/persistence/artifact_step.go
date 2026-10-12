package persistence

import "context"

type artifactStepKey struct{}

// WithArtifactStep stamps internal executor provenance, never client input.
func WithArtifactStep(ctx context.Context, step string) context.Context {
	return context.WithValue(ctx, artifactStepKey{}, step)
}

// ArtifactStep returns the step ID attached to ctx, if the executor is
// harvesting an artifact from a workflow step.
func ArtifactStep(ctx context.Context) string {
	step, _ := ctx.Value(artifactStepKey{}).(string)
	return step
}
