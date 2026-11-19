package forge

// GitHub #801 C and F (github-app-channel design, "A review good by
// default"): grounded findings in forge.post_review, and the check run.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/aidisclosure"
	"vornik.io/vornik/internal/executor"
	forgeapi "vornik.io/vornik/internal/forge"
	"vornik.io/vornik/internal/secrets"
)

var forgeCheckRuns = promauto.NewCounterVec(prometheus.CounterOpts{
	Namespace: "vornik", Name: "forge_check_runs_total",
	Help: "Check runs posted beside a forge review (GitHub #801), by outcome: posted, failed, unsupported.",
}, []string{"outcome"})

// fetchDiffStepID is the step whose masked diff grounds the findings.
const fetchDiffStepID = "fetch_diff"

// WithSecretsDetector attaches the detector the grounded findings are
// scanned with before they go out (GitHub #801 C).
func (h *PostReviewHandler) WithSecretsDetector(d secrets.Detector) *PostReviewHandler {
	if h != nil {
		h.detector = d
	}
	return h
}

// groundedBody renders the adjudicator's grounded findings. It fails when
// the fetch_diff result is missing or holds no masked diff: findings that
// cannot be grounded are not posted.
func (h *PostReviewHandler) groundedBody(ctx context.Context, in executor.SystemStepInput) (string, groundResult, error) {
	var fetched struct {
		Diff     string   `json:"diff"`
		NotInDif []string `json:"not_in_diff"`
		CutFile  string   `json:"cut_file"`
	}
	raw, ok := in.StepResults[fetchDiffStepID]
	if !ok || json.Unmarshal(raw, &fetched) != nil || fetched.Diff == "" {
		return "", groundResult{}, errors.New("ground_findings needs the fetch_diff step's masked diff; refusing to post ungrounded findings")
	}
	var prev reviewInput
	_ = json.Unmarshal(in.PrevResult, &prev)
	r := extractReview(in.PrevResult, firstNonEmpty(prev.Body, prev.Message, prev.Result, prev.Output))
	if r == nil {
		return "", groundResult{}, errors.New("ground_findings: the prior step gave no structured verdict")
	}
	added, anomalies := parseAddedLines([]byte(fetched.Diff))
	if anomalies > 0 {
		zerolog.Ctx(ctx).Warn().Int("hunks", anomalies).
			Msg("forge.post_review: hunk headers in the diff did not parse; findings on those hunks count as outside the diff")
	}
	g := groundFindings(r.Findings, added, h.detector)
	summary := r.Summary
	if h.detector == nil || len(publicationFindings(h.detector, summary)) > 0 { // #838: one scan
		summary = "(The summary was withheld: it held a secret-shaped value, or could not be scanned.)"
	}
	// Not-read names come from the masked diff, never from model text: an
	// adjudicator's entry counts only when it names a file in the diff
	// (review-20261118-befa F1).
	inDiff := map[string]bool{}
	for _, f := range splitDiffFiles([]byte(fetched.Diff)) {
		inDiff[f.path] = true
	}
	notRead := notReviewed(fetched.Diff, fetched.NotInDif, fetched.CutFile, in.StepResults) // #814 §5
	for _, p := range r.NotRead {
		if inDiff[p] {
			notRead = append(notRead, p)
		}
	}
	return renderGrounded(summary, g, dedupe(notRead)), g, nil
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// postCheckRun posts the check run beside the review: retried once, then
// logged and counted. It never fails the posted review.
func (h *PostReviewHandler) postCheckRun(ctx context.Context, provider forgeapi.ForgeProvider, job *forgeapi.ForgeJob,
	in executor.SystemStepInput, body string, g *groundResult) {
	poster, ok := provider.(forgeapi.CheckRunPoster)
	if !ok {
		forgeCheckRuns.WithLabelValues("unsupported").Inc()
		return
	}
	var kept []reviewFinding
	if g != nil {
		kept = g.Kept
	}
	spec := forgeapi.CheckRunSpec{
		Name: "Vornik review", HeadSHA: reviewedHead(in, job), Conclusion: checkRunConclusion(in.Step.CheckConclusion, kept),
		Title: checkRunTitle(kept), Summary: aidisclosure.WithFooter(strings.TrimSpace(body), h.disclosure.PublicationNotice()),
		Annotations: checkRunAnnotations(kept),
	}
	err := poster.CreateCheckRun(ctx, job.Repo, spec)
	if err != nil {
		err = poster.CreateCheckRun(ctx, job.Repo, spec)
	}
	if err != nil {
		forgeCheckRuns.WithLabelValues("failed").Inc()
		zerolog.Ctx(ctx).Warn().Err(err).Str("repo", job.Repo).Int("number", job.Number).
			Msg("forge.post_review: the check run could not be created; if branch protection requires it, the pull request stays blocked until the next review")
		return
	}
	forgeCheckRuns.WithLabelValues("posted").Inc()
}

// reviewedHead is the head fetch_diff read, else the job's.
func reviewedHead(in executor.SystemStepInput, job *forgeapi.ForgeJob) string {
	var fetched struct {
		HeadSHA string `json:"head_sha"`
	}
	if raw, ok := in.StepResults[fetchDiffStepID]; ok && json.Unmarshal(raw, &fetched) == nil && fetched.HeadSHA != "" {
		return fetched.HeadSHA
	}
	return job.HeadSHA
}

// inlineCommentCap is how many findings become inline comments; the rest
// stay in the body's list (GitHub #244).
const inlineCommentCap = 50

// inlineComments maps kept findings to inline comments, up to the cap.
func inlineComments(kept []reviewFinding) []forgeapi.ReviewLineComment {
	var out []forgeapi.ReviewLineComment
	for _, f := range kept {
		if len(out) == inlineCommentCap {
			break
		}
		out = append(out, forgeapi.ReviewLineComment{Path: f.File, Line: f.Line,
			Body: "**" + f.Severity + "**: " + f.Title + "\n\n" + f.Detail})
	}
	return out
}
