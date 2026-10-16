package api

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCompanionRecallDescription_CarriesRAGFirstSteering pins the RAG-first
// steering clause on the recall tool Description (LLD 2026-07-12-companion-rag-
// first-guidance §4 secondary lever). This is the cross-client signal — for
// Codex (no SessionStart hooks) the Description is the ONLY RAG-first nudge, so
// a refactor that drops it must fail loudly. The `| USAGE:` separator marks
// the functional-vs-policy boundary.
func TestCompanionRecallDescription_CarriesRAGFirstSteering(t *testing.T) {
	defs := companionToolDefs()
	var recall *mcpToolDef
	for i := range defs {
		if defs[i].Name == "recall" {
			recall = &defs[i]
			break
		}
	}
	require.NotNil(t, recall, "recall tool must be defined")
	assert.Contains(t, recall.Description, "| USAGE:",
		"recall Description must carry the functional|USAGE separator (cross-client steering layer)")
	assert.Contains(t, recall.Description, "BEFORE a deep code dive",
		"recall Description must steer toward recall-before-code on design questions")
	// GitHub #382, design §11.3: the unconditional "the LLDs in project
	// memory are the authoritative design record" misled every project with
	// no designs (review 97b7 B1). The clause is conditional on design_record.
	assert.False(t, strings.Contains(recall.Description, "are the authoritative design record"),
		"recall Description must not assert that project-memory designs exist")
	assert.Contains(t, recall.Description, "design_record",
		"recall Description must point at the per-recall design count")
	assert.Contains(t, recall.Description, "do not claim one",
		"recall Description must say what a zero means")
	// GitHub #390, design §11.14 (code review c12b N1): a review is not the
	// design it reviews, and the note carries the crowded-zero recount.
	assert.Contains(t, recall.Description, "not produced by a workflow",
		"recall Description must say workflow output does not count as a design")
	assert.Contains(t, recall.Description, "read its note",
		"recall Description must point at design_record's note")
}
