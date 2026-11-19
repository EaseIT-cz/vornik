package executor

// GitHub #751 (reviewable-output design §6.12): a companion review opens with
// its verdict and a numbered action list, or "No action: ship.", within its
// first 150 words, so the reader can act in the first minute and no review
// ends with evidence and no recommendation (the operator's timed reading
// sample, §6.11).

import (
	"context"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"

	"vornik.io/vornik/internal/pipeline"
)

const (
	reviewActionsWordBound = 150
	reviewActionsReadCap   = 64 << 10 // the opening is all the check reads
)

var (
	reviewVerdictLine  = regexp.MustCompile(`(?i)^\**\s*verdict\s*:`)
	reviewActionsHead  = regexp.MustCompile(`(?i)^#{1,3}\s+actions\s*$`)
	reviewNumberedItem = regexp.MustCompile(`^\s*\d+[.)]\s+\S`)
	// Anchored to its own line: a review that quotes the format in its
	// findings must not pass (review 6eb1).
	reviewNoAction = regexp.MustCompile(`(?i)^\**\s*no\s+action:\s*ship\.?\s*\**$`)
)

// reviewActions is where a review's action list sits: the line that opens it
// (an ## Actions heading, or "No action: ship.") and the line after it ends.
type reviewActions struct {
	start, end, items int
	noAction          bool
}

// findReviewActions returns the first action list in lines, or start == -1.
func findReviewActions(lines []string) reviewActions {
	for i, l := range lines {
		if reviewNoAction.MatchString(strings.TrimSpace(l)) {
			return reviewActions{start: i, end: i + 1, noAction: true}
		}
		if !reviewActionsHead.MatchString(strings.TrimSpace(l)) {
			continue
		}
		a := reviewActions{start: i, end: len(lines)}
		for j := i + 1; j < len(lines); j++ {
			if strings.HasPrefix(strings.TrimSpace(lines[j]), "#") {
				a.end = j
				break
			}
			if reviewNumberedItem.MatchString(lines[j]) {
				a.items++
			}
		}
		return a
	}
	return reviewActions{start: -1}
}

func wordCount(lines []string) int {
	n := 0
	for _, l := range lines {
		n += len(strings.Fields(l))
	}
	return n
}

// reviewActionsProblem says what the review's opening lacks, or "" when it
// has a Verdict: line and then either a numbered ## Actions list or "No
// action: ship.", ending within the first reviewActionsWordBound words.
func reviewActionsProblem(text string) string {
	lines := strings.Split(text, "\n")
	verdictAt := -1
	for i, l := range lines {
		if reviewVerdictLine.MatchString(strings.TrimSpace(l)) {
			verdictAt = i
			break
		}
	}
	a := findReviewActions(lines)
	switch {
	case a.start < 0 && verdictAt < 0:
		return "no Verdict: line"
	case a.start < 0:
		return "no action list (## Actions, or No action: ship.)"
	case verdictAt < 0 || verdictAt > a.start:
		return "no Verdict: line before the actions"
	case !a.noAction && a.items == 0:
		return "an ## Actions heading with no numbered item"
	case wordCount(lines[:a.end]) > reviewActionsWordBound:
		return fmt.Sprintf("the action list does not end within the first %d words", reviewActionsWordBound)
	}
	return ""
}

// reviewActionsParticipant enforces the format on a step that opted in with
// require_review_actions, on the file its output contract matched. A missing
// file is the output contract's refusal, not this one's.
func (e *Executor) reviewActionsParticipant(_ context.Context, in *StepOutcome) pipeline.Verdict {
	logUnread := func(err error) {
		e.logger.Warn().Err(err).Str("step", in.StepID).
			Msg("review actions: the review file could not be read; the step is not checked (#751)")
	}
	if in == nil || in.Step == nil || !in.Step.RequireReviewActions {
		return pipeline.Verdict{}
	}
	path := outputGlobMatch(in.WorkspaceDir, in.ProjectDir, in.Step.RequireOutputGlob, in.StepStart)
	if path == "" {
		return pipeline.Verdict{}
	}
	f, err := os.Open(path)
	if err != nil {
		logUnread(err)
		return pipeline.Verdict{}
	}
	defer func() { _ = f.Close() }() // read-only; a close error changes nothing
	head, err := io.ReadAll(io.LimitReader(f, reviewActionsReadCap))
	if err != nil {
		logUnread(err)
		return pipeline.Verdict{}
	}
	problem := reviewActionsProblem(string(head))
	if problem == "" {
		return pipeline.Verdict{}
	}
	return pipeline.Verdict{Refused: true, Reason: fmt.Sprintf(
		"schema violation: review contract for step %q not met: %s. Start the review with a line \"Verdict: …\" "+
			"(at most two sentences of why), then a heading \"## Actions\" with a numbered list of what to do, or the single line "+
			"\"No action: ship.\", all within the first %d words; put the findings and evidence after it.",
		in.StepID, problem, reviewActionsWordBound)}
}
