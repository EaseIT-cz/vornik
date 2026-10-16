package datasubject_test

import (
	"sort"
	"strings"
	"testing"

	"vornik.io/vornik/internal/datasubject"
	"vornik.io/vornik/internal/persistence"
)

// TestDataSubjectLinkTablesWiped_MatchesTheWipe pins the project wipe's
// data_subject_links predicate to the subject axis (archival design section
// J, GitHub #370, 2026-10-16). Erasure's rule is that a link goes with its
// row; the wipe removes the links whose row it removed, for each linkable
// table in persistence.DataSubjectLinkTablesWiped. That list must be exactly
// the linkable tables the wipe deletes rows from: listed in
// persistence.ProjectDataTables, or a CASCADE child (SQLiteCascadeGraph) of a
// listed table. A linkable table missing from it would leave dangling links
// after a wipe; an extra one would delete links to rows that survive.
func TestDataSubjectLinkTablesWiped_MatchesTheWipe(t *testing.T) {
	listed := map[string]bool{}
	for _, tb := range persistence.ProjectDataTables {
		listed[tb] = true
	}
	removed := func(tb string) bool {
		if listed[tb] {
			return true
		}
		for _, e := range persistence.SQLiteCascadeGraph {
			if e.Child == tb && e.Action == persistence.FKCascade && listed[e.Parent] {
				return true
			}
		}
		return false
	}
	var want []string
	for _, tb := range datasubject.LinkableTables() {
		if removed(tb) {
			want = append(want, tb)
		}
	}
	got := append([]string{}, persistence.DataSubjectLinkTablesWiped...)
	sort.Strings(got)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("DataSubjectLinkTablesWiped = %v, want the linkable tables the wipe removes rows from: %v", got, want)
	}
	t.Logf("linkable tables examined: %d; removed by the project wipe: %d", len(datasubject.LinkableTables()), len(want))
}
