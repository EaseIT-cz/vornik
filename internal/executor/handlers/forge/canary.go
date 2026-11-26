package forge

// The review's planted canary (GitHub #873; github-app-channel design, "The
// review measures itself: a planted canary"). A task of type canary with no
// forge job runs the App review on a generated diff of fake secrets and
// decoys: fetch_diff generates and masks it, fetch_ci notes there is no CI,
// and post_review scores instead of posting. No step calls GitHub. The raw
// corpus lives in this file's memory only; only the masked diff and a
// value-free manifest leave fetch_diff.

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"math/big"
	"strings"

	"vornik.io/vornik/internal/executor"
	forgeapi "vornik.io/vornik/internal/forge"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/secrets"
)

const (
	canaryTaskType = "canary"
	canaryRepo     = "canary/planted"

	canaryKindRule  = "rule"  // the mask must catch it
	canaryKindModel = "model" // only a model can
	canaryKindDecoy = "decoy" // nothing should flag it

	// canaryMaxDraws bounds each plant's redraw. A rule plant the detector
	// never matches means the mask stopped catching its kind (design C2).
	canaryMaxDraws = 50
)

// isCanaryTask reports whether t runs in canary mode: its type is canary and
// it carries no forge job. A task with a pull request's job is a review,
// whatever its type, so a type can never suppress a real review (design C1).
func isCanaryTask(t *persistence.Task) bool {
	if t == nil {
		return false
	}
	var p struct {
		TaskType string `json:"taskType"`
	}
	if json.Unmarshal(t.Payload, &p) != nil || !IsCanaryTaskType(p.TaskType) {
		return false
	}
	_, err := loadForgeJob(t, "canary")
	return err != nil
}

// IsCanaryTaskType reports whether a task type names the review canary,
// ignoring case and surrounding space. The generic webhook refuses such a type
// (GitHub #873), with this same test.
func IsCanaryTaskType(taskType string) bool {
	return strings.EqualFold(strings.TrimSpace(taskType), canaryTaskType)
}

// canaryPlant is one planted value. value and lineText never leave this file.
type canaryPlant struct {
	ID, Kind, File string
	Line           int
	value          string
	lineText       string
}

type canaryFile struct {
	name  string
	lines []string
}

type canaryCorpus struct {
	diff   []byte
	plants []canaryPlant
}

// canaryManifestPlant is what the manifest says about a plant: never a value.
type canaryManifestPlant struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"`
	File   string `json:"file"`
	Line   int    `json:"line"`
	Masked bool   `json:"masked"`
}

func (c canaryCorpus) manifest() []canaryManifestPlant {
	out := make([]canaryManifestPlant, 0, len(c.plants))
	for _, p := range c.plants {
		out = append(out, canaryManifestPlant{ID: p.ID, Kind: p.Kind, File: p.File, Line: p.Line})
	}
	return out
}

const (
	alnum  = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	b64set = alnum + "+/"
)

func randomString(set string, n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		k, _ := rand.Int(rand.Reader, big.NewInt(int64(len(set))))
		b.WriteByte(set[k.Int64()])
	}
	return b.String()
}

// lineMatches reports whether d finds a value overlapping value in line.
func lineMatches(d secrets.Detector, line, value string) bool {
	at := strings.Index(line, value)
	if at < 0 {
		return false
	}
	for _, f := range d.Scan([]byte(line)) {
		if f.Start < at+len(value) && f.End > at {
			return true
		}
	}
	return false
}

// plantSpec draws one plant: its file, the line it is on, and its value.
type plantSpec struct {
	id, kind, file string
	draw           func() (lines []string, line int, value string)
}

func canarySpecs() []plantSpec {
	return []plantSpec{
		{"rule-github-pat", canaryKindRule, "internal/config/github.go", func() ([]string, int, string) {
			v := "ghp_" + randomString(alnum, 36)
			return []string{"package config", "", `const githubToken = "` + v + `"`}, 3, v
		}},
		{"rule-aws-secret", canaryKindRule, "deploy/aws.ini", func() ([]string, int, string) {
			v := randomString(b64set, 40)
			return []string{"[default]", "aws_secret_access_key = " + v}, 2, v
		}},
		{"rule-api-token", canaryKindRule, "config/app.yaml", func() ([]string, int, string) {
			v := randomString(alnum, 40)
			return []string{"service: billing", "api_token: " + v}, 2, v
		}},
		{"rule-db-url", canaryKindRule, "config/db.env", func() ([]string, int, string) {
			v := randomString(alnum, 20)
			return []string{"DATABASE_URL=postgres://billing:" + v + "@db.internal:5432/billing"}, 1, v
		}},
		{"rule-pem-key", canaryKindRule, "deploy/tls.key", func() ([]string, int, string) {
			lines := []string{"-----BEGIN RSA PRIVATE KEY-----"}
			for i := 0; i < 4; i++ {
				lines = append(lines, randomString(b64set, 64))
			}
			lines = append(lines, "-----END RSA PRIVATE KEY-----")
			return lines, 1, lines[0]
		}},
		{"model-split-token", canaryKindModel, "internal/client/auth.go", func() ([]string, int, string) {
			v := randomString(alnum, 36)
			return []string{"package client", "", "func token() string {", `	return "ghp_" + "` + v + `"`, "}"}, 4, v
		}},
		// AWS's documentation key was the first choice and the default
		// detector matches it (aws_access_key, measured 2026-11-21), so a
		// checksum is the hex decoy, as in the case study.
		{"decoy-checksum", canaryKindDecoy, "release/checksums.yaml", func() ([]string, int, string) {
			v := randomString("0123456789abcdef", 64)
			return []string{"artifact: billing-linux-amd64", "checksum_sha256: " + v}, 2, v
		}},
		{"decoy-env-lookup", canaryKindDecoy, "internal/client/env.go", func() ([]string, int, string) {
			return []string{"package client", "", `import "os"`, "", `var apiToken = os.Getenv("API_TOKEN")`}, 5, `os.Getenv("API_TOKEN")`
		}},
		{"decoy-placeholder", canaryKindDecoy, "config/app.example.yaml", func() ([]string, int, string) {
			return []string{`password: "<changeme>"`}, 1, "<changeme>"
		}},
	}
}

// newCanaryCorpus draws every plant: a rule plant until d matches it, a model
// plant or decoy until d does not, each at most canaryMaxDraws times.
func newCanaryCorpus(d secrets.Detector) (canaryCorpus, error) {
	if d == nil {
		return canaryCorpus{}, errNoDetector
	}
	var c canaryCorpus
	var files []canaryFile
	for _, s := range canarySpecs() {
		var lines []string
		var line int
		var value string
		ok := false
		for i := 0; i < canaryMaxDraws && !ok; i++ {
			lines, line, value = s.draw()
			ok = lineMatches(d, lines[line-1], value) == (s.kind == canaryKindRule)
		}
		if !ok {
			if s.kind == canaryKindRule {
				return canaryCorpus{}, fmt.Errorf("canary: the mask no longer catches %s: %d draws, none matched", s.id, canaryMaxDraws)
			}
			return canaryCorpus{}, fmt.Errorf("canary: the detector now matches %s, which must reach a model unmasked", s.id)
		}
		files = append(files, canaryFile{name: s.file, lines: lines})
		c.plants = append(c.plants, canaryPlant{ID: s.id, Kind: s.kind, File: s.file, Line: line, value: value, lineText: lines[line-1]})
	}
	var b strings.Builder
	for _, f := range files {
		fmt.Fprintf(&b, "diff --git a/%s b/%s\nnew file mode 100644\n--- /dev/null\n+++ b/%s\n@@ -0,0 +1,%d @@\n", f.name, f.name, f.name, len(f.lines))
		for _, l := range f.lines {
			b.WriteString("+" + l + "\n")
		}
	}
	c.diff = []byte(b.String())
	return c, nil
}

// canaryDiffResult is fetch_diff in canary mode: the generated diff, masked
// through maskedResult exactly as a pull request's is, plus the manifest.
func (h *FetchDiffHandler) canaryDiffResult(in executor.SystemStepInput) (executor.SystemStepResult, error) {
	if in.Step == nil || !in.Step.MaskSecrets {
		return executor.SystemStepResult{}, fmt.Errorf("forge.fetch_diff: a canary run needs a mask_secrets step (the App review's)")
	}
	c, err := newCanaryCorpus(h.detector)
	if err != nil {
		return executor.SystemStepResult{}, err
	}
	res, err := h.maskedResult(c.diff, &forgeapi.ForgeJob{Repo: canaryRepo}, "canary", "")
	if err != nil {
		return executor.SystemStepResult{}, err
	}
	var out map[string]any
	if err := json.Unmarshal(res.Result, &out); err != nil {
		return executor.SystemStepResult{}, err
	}
	masked, ok := out["diff"].(string)
	if !ok {
		return executor.SystemStepResult{}, fmt.Errorf("forge.fetch_diff: a canary run's masked result has no diff")
	}
	m := c.manifest()
	for i, p := range c.plants {
		m[i].Masked = !strings.Contains(masked, p.value)
	}
	out["canary"] = map[string]any{"plants": m}
	b, _ := json.Marshal(out)
	return executor.SystemStepResult{Result: b}, nil
}

// canaryScore is post_review's result in canary mode (design C3).
type canaryScore struct {
	Outcome       string   `json:"outcome"`
	Message       string   `json:"message"`
	Scope         string   `json:"scope"`
	Plants        int      `json:"plants"`
	Detected      int      `json:"detected"`
	ByMask        int      `json:"by_mask"`
	ByModel       int      `json:"by_model"`
	Missed        []string `json:"missed"`
	Decoys        int      `json:"decoys"`
	DecoysFlagged int      `json:"decoys_flagged"`
	FlaggedDecoys []string `json:"flagged_decoys"`
	Floor         *int     `json:"floor"`
	Ceiling       *int     `json:"ceiling"`
	Verdict       string   `json:"verdict"`
}

const canaryScope = "the review's reading of a planted diff: the mask and the models; not the webhook, the fetch or the posting"

// citedNear reports whether a kept finding cites file within one line of line.
func citedNear(fs []reviewFinding, file string, line int) bool {
	for _, f := range fs {
		if f.File == file && f.Line >= line-1 && f.Line <= line+1 {
			return true
		}
	}
	return false
}

// scoreCanary is post_review in canary mode: it posts nothing and routes by
// its outcome, never by an error, so a low score fails the task through the
// workflow's canary_failed terminal and is never retried.
func (h *PostReviewHandler) scoreCanary(in executor.SystemStepInput) (executor.SystemStepResult, error) {
	var diffStep struct {
		Canary struct {
			Plants []canaryManifestPlant `json:"plants"`
		} `json:"canary"`
	}
	if raw := in.StepResults[fetchDiffStepID]; len(raw) == 0 || json.Unmarshal(raw, &diffStep) != nil || len(diffStep.Canary.Plants) == 0 {
		return executor.SystemStepResult{}, fmt.Errorf("forge.post_review: a canary run has no manifest from %s", fetchDiffStepID)
	}
	r := extractReview(in.PrevResult, "")
	if r == nil {
		// A review that never arrived is a plumbing failure, not a review that
		// found nothing; scoring it would blame the review.
		return executor.SystemStepResult{}, fmt.Errorf("forge.post_review: a canary run has no review result to score")
	}
	kept := r.Findings
	s := canaryScore{Outcome: "canary", Scope: canaryScope, Missed: []string{}, FlaggedDecoys: []string{}}
	for _, p := range diffStep.Canary.Plants {
		at := fmt.Sprintf("%s (%s:%d)", p.ID, p.File, p.Line)
		if p.Kind == canaryKindDecoy {
			s.Decoys++
			if citedNear(kept, p.File, p.Line) {
				s.DecoysFlagged++
				s.FlaggedDecoys = append(s.FlaggedDecoys, at)
			}
			continue
		}
		s.Plants++
		switch {
		case p.Masked:
			s.Detected++
			s.ByMask++
		case citedNear(kept, p.File, p.Line):
			s.Detected++
			s.ByModel++
		default:
			s.Missed = append(s.Missed, at)
		}
	}
	var floors registry.ProjectForgeCanary
	if h.canaryFloors != nil && in.Task != nil {
		floors = h.canaryFloors(in.Task.ProjectID)
	}
	s.Floor, s.Ceiling = floors.FloorDetections, floors.CeilingDecoys
	switch {
	case s.Floor == nil && s.Ceiling == nil:
		s.Verdict = "measured: no floor set"
	case s.Floor != nil && s.Detected < *s.Floor:
		s.Outcome, s.Verdict = "canary-below-floor", "fail"
		s.Message = fmt.Sprintf("canary: below floor: %d of %d detected, floor %d; missed: %s", s.Detected, s.Plants, *s.Floor, strings.Join(s.Missed, ", "))
	case s.Ceiling != nil && s.DecoysFlagged > *s.Ceiling:
		s.Outcome, s.Verdict = "canary-above-ceiling", "fail"
		s.Message = fmt.Sprintf("canary: above ceiling: %d of %d decoys flagged, ceiling %d; flagged: %s", s.DecoysFlagged, s.Decoys, *s.Ceiling, strings.Join(s.FlaggedDecoys, ", "))
	default:
		s.Verdict = "pass"
	}
	if s.Message == "" {
		s.Message = fmt.Sprintf("canary: %s: %d of %d detected (%d by the mask, %d by a model), %d of %d decoys flagged", s.Verdict, s.Detected, s.Plants, s.ByMask, s.ByModel, s.DecoysFlagged, s.Decoys)
	}
	b, _ := json.Marshal(s)
	return executor.SystemStepResult{Result: b}, nil
}
