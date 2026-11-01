package persistence

import (
	"slices"
	"testing"
)

// GitHub #596 (commit security review, 2026-10-31): the entry-funnel table
// is project-scoped, so deleting a project must wipe it.
func TestProjectDataTables_IncludesTradingFunnel_596(t *testing.T) {
	if !slices.Contains(ProjectDataTables, "trading_funnel_candidates") {
		t.Fatal("trading_funnel_candidates must be wiped with its project")
	}
}
