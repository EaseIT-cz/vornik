package forge

// GitHub #801 C and F (github-app-channel design, "A review good by
// default"): keep only findings grounded in the diff and free of secrets,
// and the check run that carries them.

import (
	"fmt"
	"strings"

	"vornik.io/vornik/internal/secrets"
)

// reviewFinding is one finding of the adjudicator's verdict.
type reviewFinding struct {
	File     string `json:"file"`
	Line     int    `json:"line"`
	Severity string `json:"severity"` // blocker, major, minor, nit
	Title    string `json:"title"`
	Detail   string `json:"detail"`
}

// groundResult is what grounding kept and why it dropped the rest.
type groundResult struct {
	Kept       []reviewFinding
	Ungrounded int // the cited file:line is not an added line
	Echoed     int // the text holds a secret-shaped value
}

// groundFindings drops a finding whose file:line is not an added line of the
// diff, or whose text the detector flags. A nil detector drops every
// finding's text check as failed: nothing unscanned goes out.
func groundFindings(fs []reviewFinding, added addedLines, d secrets.Detector) groundResult {
	var r groundResult
	for _, f := range fs {
		if !added.has(f.File, f.Line) {
			r.Ungrounded++
			continue
		}
		// #838: one scan, over every model field the finding publishes (the
		// severity is written into its inline comment).
		if d == nil || len(publicationFindings(d, f.Severity+"\n"+f.Title+"\n"+f.Detail)) > 0 {
			r.Echoed++
			forgeReviewRedactions.WithLabelValues("grounded").Inc() // review 82a3 L5
			continue
		}
		r.Kept = append(r.Kept, f)
	}
	return r
}

// checkRunAnnotationCap is GitHub's limit per check-run request.
const checkRunAnnotationCap = 50

// checkRunConclusion is neutral by default; from_verdict fails on a kept
// blocker and succeeds otherwise.
func checkRunConclusion(mode string, kept []reviewFinding) string {
	if mode != "from_verdict" {
		return "neutral"
	}
	for _, f := range kept {
		if f.Severity == "blocker" {
			return "failure"
		}
	}
	return "success"
}

// checkRunAnnotations maps kept findings to annotations, up to the cap.
func checkRunAnnotations(kept []reviewFinding) []map[string]any {
	var out []map[string]any
	for _, f := range kept {
		if len(out) == checkRunAnnotationCap {
			break
		}
		level := "notice"
		switch f.Severity {
		case "blocker":
			level = "failure"
		case "major":
			level = "warning"
		}
		out = append(out, map[string]any{"path": f.File, "start_line": f.Line, "end_line": f.Line,
			"annotation_level": level, "title": f.Title, "message": f.Detail})
	}
	return out
}

// checkRunTitle is "N findings (B blockers)".
func checkRunTitle(kept []reviewFinding) string {
	blockers := 0
	for _, f := range kept {
		if f.Severity == "blocker" {
			blockers++
		}
	}
	return fmt.Sprintf("%d findings (%d blockers)", len(kept), blockers)
}

// renderGrounded is the review body: summary, kept findings by severity,
// then what was dropped and what was not read.
func renderGrounded(summary string, g groundResult, notRead []string) string {
	var b strings.Builder
	b.WriteString(strings.TrimSpace(summary))
	b.WriteString("\n")
	for _, sev := range []string{"blocker", "major", "minor", "nit"} {
		var lines []string
		for _, f := range g.Kept {
			if f.Severity == sev {
				lines = append(lines, fmt.Sprintf("- `%s:%d` **%s** %s", f.File, f.Line, f.Title, f.Detail))
			}
		}
		if len(lines) > 0 {
			b.WriteString("\n**" + strings.ToUpper(sev[:1]) + sev[1:] + "**\n" + strings.Join(lines, "\n") + "\n")
		}
	}
	if g.Ungrounded > 0 {
		fmt.Fprintf(&b, "\nVornik dropped %d findings that cited lines outside the diff.\n", g.Ungrounded)
	}
	if g.Echoed > 0 {
		fmt.Fprintf(&b, "\nVornik dropped %d findings whose text held a secret-shaped value.\n", g.Echoed)
	}
	if len(notRead) > 0 {
		fmt.Fprintf(&b, "\nNot reviewed: %d files (%s).\n", len(notRead), strings.Join(notRead, ", "))
	}
	return b.String()
}
