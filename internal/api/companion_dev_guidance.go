package api

import (
	_ "embed"
	"fmt"
	"strings"
	"time"
)

// designRecountTimeout caps design_record's strict recount, so a slow second
// search cannot stall a recall; a timeout reads as a failed recount (§11.14).
const designRecountTimeout = 5 * time.Second

// companionDevGuidance is how an agent should change code in a repository
// whose project memory is in Vornik (companion RAG-first guidance design §11,
// GitHub #382): recall first, design first, keep memory current, and build an
// as-built record surface by surface where memory holds no design. It is the
// one source: each bundle's develop-with-memory skill carries it between the
// markers below (a test compares them) and the Claude Code SessionStart hook
// prints that section. It is deliberately NOT in initialize (§11.3): that
// reaches personal-assistant memory keys, where coding rules are noise.
//
//go:embed companion_dev_guidance.md
var companionDevGuidance string

// The markers that delimit the guidance inside a develop-with-memory SKILL.md,
// and part B (design first) inside the text, which VORNIK_DESIGN_FIRST=off
// drops in the Claude Code hook.
const (
	companionDevGuidanceStart = "<!-- vornik-dev-guidance:start -->"
	companionDevGuidanceEnd   = "<!-- vornik-dev-guidance:end -->"
	companionDesignFirstStart = "<!-- vornik-design-first:start -->"
	companionDesignFirstEnd   = "<!-- vornik-design-first:end -->"
)

// CompanionDevGuidance returns the development guidance text.
func CompanionDevGuidance() string { return strings.TrimSpace(companionDevGuidance) }

// designRecord is recall's per-call count of the design the hits hold for the
// repository in hand (design §11.2, §11.14). It is computed from the returned
// hits, plus at most one strict-scope recount when the first count was
// crowded. It answers "none among the hits examined", never "none exists".
type designRecord struct {
	// RepoScope is the effective scope counted against; "" = project-wide.
	RepoScope string `json:"repo_scope"`
	// Examined is the number of hits returned: the denominator.
	Examined int `json:"examined"`
	// DesignHits counts spec/decision hits whose repo_scope equals RepoScope
	// exactly (NULL and "*" never match a non-empty scope) and which carry no
	// task_id: a workflow's output is not the design it reviews (#390).
	DesignHits int `json:"design_hits"`
	// WorkflowOutput counts the design-class, in-scope hits left out of
	// DesignHits because a task produced them.
	WorkflowOutput int `json:"workflow_output"`
	// Unscoped counts the examined hits with no repo scope (NULL), which a
	// non-strict scoped recall lets compete for the top N (#391).
	Unscoped int `json:"unscoped"`
	// StrictRecount is the strict-scope recount, run only on a crowded zero.
	StrictRecount *designRecount `json:"strict_recount,omitempty"`
	// Note is set only under a non-empty RepoScope (design §11.12).
	Note string `json:"note,omitempty"`
}

// designRecount is the count of a strict-scope second search (§11.14).
type designRecount struct {
	Examined       int `json:"examined"`
	DesignHits     int `json:"design_hits"`
	WorkflowOutput int `json:"workflow_output"`
}

// designRecordUndeterminedNote is the note for a recall that returned
// nothing: with nothing examined, nothing is ruled out (review eab1 F3).
const designRecordUndeterminedNote = "Recall returned no hits for this question, so whether memory holds a design for it is not determined; recall with other words before concluding there is none."

// designRecordWhere names the scope; the notes are built only under one.
func designRecordWhere(scope string) string { return "in repo scope " + scope }

// designRecordNoneNote says only what the count supports: not among the
// hits examined (review of §11.13: "memory holds no design" overclaimed).
func designRecordNoneNote(scope string, examined int) string {
	return fmt.Sprintf("No design-class (spec or decision) hit %s among the %d hits examined: no design for this question was found in this repository's memory. Do not treat other hits as its design; work from the code, and offer to write an as-built record of this part.",
		designRecordWhere(scope), examined)
}

func designRecordSomeNote(scope string, n, examined int) string {
	return fmt.Sprintf("%d design-class hit(s) %s among the %d hits examined: these are this repository's design record for the question; verify any file, flag or function they name against the code.",
		n, designRecordWhere(scope), examined)
}

// designRecordCrowdedNote is the crowded zero whose strict recount failed.
func designRecordCrowdedNote(scope string, examined, unscoped int) string {
	return fmt.Sprintf("No design-class hit %s among the %d hits examined, but %d of them carry no repo scope and may have pushed this repository's designs out; recall again with strict_scope=true before concluding there is none.",
		designRecordWhere(scope), examined, unscoped)
}

// designRecordRecountFoundNote is the crowded zero whose strict recount found
// designs the first search ranked out.
func designRecordRecountFoundNote(scope string, examined, unscoped, n int) string {
	return fmt.Sprintf("No design-class hit %s among the %d hits examined, because %d hits without a repo scope took their places; a strict-scope recount found %d: recall with strict_scope=true to read them.",
		designRecordWhere(scope), examined, unscoped, n)
}

// designRecordWorkflowSuffix is appended to any note when design-class hits
// were left out as workflow output (§11.14 decision 4).
func designRecordWorkflowSuffix(n int) string {
	return fmt.Sprintf("%d design-class hit(s) are reviews or other workflow output about this repository, not its design.", n)
}

// isDesignClass reports whether a content class holds designs.
func isDesignClass(class string) bool {
	return strings.EqualFold(class, "spec") || strings.EqualFold(class, "decision")
}

// countDesigns counts, for scope, the design hits, the design-class workflow
// output left out, and the NULL-scoped hits.
func countDesigns(scope string, hits []recallHit) (designs, workflow, unscoped int) {
	for _, h := range hits {
		if h.RepoScope == "" {
			unscoped++
		}
		if !isDesignClass(h.ContentClass) || (scope != "" && h.RepoScope != scope) {
			continue
		}
		if h.TaskID != "" {
			workflow++
			continue
		}
		designs++
	}
	return designs, workflow, unscoped
}

// buildDesignRecord counts the design hits for scope. It returns nil when the
// caller filtered recall to a class that cannot hold designs, because a zero
// there would be a false "none". recount, called at most once and only on a
// crowded zero (scoped, not strict, no design, some NULL-scoped hits), runs
// the same recall with strict_scope (§11.14).
func buildDesignRecord(scope, classFilter string, strict bool, hits []recallHit, recount func() ([]recallHit, error)) *designRecord {
	if classFilter != "" && !isDesignClass(classFilter) {
		return nil
	}
	dr := &designRecord{RepoScope: scope, Examined: len(hits)}
	dr.DesignHits, dr.WorkflowOutput, dr.Unscoped = countDesigns(scope, hits)
	// The notes are coding guidance about "this repository". An unscoped
	// recall is the Hermes memory provider's, where a personal question would
	// get it (#387, design §11.12): it keeps the counts and gets no note.
	if scope == "" {
		return dr
	}
	workflow := dr.WorkflowOutput
	switch {
	case dr.Examined == 0:
		dr.Note = designRecordUndeterminedNote
	case dr.DesignHits > 0:
		dr.Note = designRecordSomeNote(scope, dr.DesignHits, dr.Examined)
	case dr.Unscoped > 0 && !strict && recount != nil:
		strictHits, err := recount()
		if err != nil {
			dr.Note = designRecordCrowdedNote(scope, dr.Examined, dr.Unscoped)
			break
		}
		rc := &designRecount{Examined: len(strictHits)}
		rc.DesignHits, rc.WorkflowOutput, _ = countDesigns(scope, strictHits)
		dr.StrictRecount, workflow = rc, rc.WorkflowOutput
		if rc.DesignHits > 0 {
			dr.Note = designRecordRecountFoundNote(scope, dr.Examined, dr.Unscoped, rc.DesignHits)
		} else {
			dr.Note = designRecordNoneNote(scope, rc.Examined)
		}
	default:
		dr.Note = designRecordNoneNote(scope, dr.Examined)
	}
	if workflow > 0 && dr.Examined > 0 {
		dr.Note += " " + designRecordWorkflowSuffix(workflow)
	}
	return dr
}
