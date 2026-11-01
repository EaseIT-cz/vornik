package persistence

import (
	"context"
	"time"
)

// InstanceMilestoneRepository holds the instance's once-only milestones
// (GitHub #587, operations-api-contract-design.md §7.2.2): the first recall
// hit and the first assistant answer. The first mark of a name is kept and
// never moved, so a gauge read from it never changes once set. No retention
// or erasure prunes it: it holds a fixed name and a time, nothing else.
type InstanceMilestoneRepository interface {
	// MarkMilestone records name at at unless name is already recorded,
	// and reports whether this call recorded it.
	MarkMilestone(ctx context.Context, name string, at time.Time) (bool, error)
	// Milestones returns every recorded name and its time.
	Milestones(ctx context.Context) (map[string]time.Time, error)
}
