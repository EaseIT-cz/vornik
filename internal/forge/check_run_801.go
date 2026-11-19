package forge

import "context"

// CheckRunSpec is a completed check run (GitHub #801 F).
type CheckRunSpec struct {
	Name        string
	HeadSHA     string
	Conclusion  string // neutral, success or failure
	Title       string
	Summary     string
	Annotations []map[string]any // path, start_line, end_line, annotation_level, title, message
}

// CheckRunPoster is implemented by a provider that can post a check run
// beside a review. It is optional: a provider without it gets the review
// only.
type CheckRunPoster interface {
	CreateCheckRun(ctx context.Context, repo string, spec CheckRunSpec) error
}
