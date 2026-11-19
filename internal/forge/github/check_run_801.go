package github

// GitHub #801 F: a completed check run beside the review, through the
// installation token the review uses.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"unicode/utf8"

	"vornik.io/vornik/internal/forge"
)

// checkRunSummaryMax is GitHub's limit on output.summary.
const checkRunSummaryMax = 65535

// CreateCheckRun implements forge.CheckRunPoster.
func (p *Provider) CreateCheckRun(ctx context.Context, repo string, spec forge.CheckRunSpec) error {
	tok, err := p.token(ctx)
	if err != nil {
		return err
	}
	summary := p.withMark(ctx, spec.Summary)
	if len(summary) > checkRunSummaryMax {
		cut := checkRunSummaryMax - 3
		for cut > 0 && !utf8.RuneStart(summary[cut]) {
			cut-- // never split a rune
		}
		summary = summary[:cut] + "..."
	}
	output := map[string]any{"title": spec.Title, "summary": summary}
	if len(spec.Annotations) > 0 {
		output["annotations"] = spec.Annotations
	}
	reqBody, err := json.Marshal(map[string]any{
		"name": spec.Name, "head_sha": spec.HeadSHA, "status": "completed",
		"conclusion": spec.Conclusion, "output": output,
	})
	if err != nil {
		return fmt.Errorf("forge/github: encode check run: %w", err)
	}
	req, err := p.newReq(ctx, http.MethodPost, fmt.Sprintf("%s/repos/%s/check-runs", p.apiBaseURL, repo), reqBody, tok)
	if err != nil {
		return err
	}
	resp, respBody, err := p.do(req)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusCreated {
		return forge.NewStatusError("forge/github: create check run", resp.StatusCode, resp.Header, excerpt(respBody))
	}
	return nil
}
