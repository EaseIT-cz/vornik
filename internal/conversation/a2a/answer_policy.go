package a2a

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strings"
	"unicode"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/executor"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// Answer-only egress (architect design §4.2 C5 and C7, #614). A published
// workflow with an a2a answer policy sends its caller exactly one thing: the
// listed keys of one named JSON artifact of a COMPLETED task. Every check
// fails closed; a refusal sends no artifact frame (the caller still receives
// the terminal state) and is logged with the task and the rule, never the
// text.

// fencedBlock matches a fenced code block (``` or ~~~ at a line start)
// through its closing fence, or to the end of the text when unterminated.
var fencedBlock = regexp.MustCompile("(?s)(?:^|\n)[ \t]*(?:```|~~~)[^\n]*(?:\n.*?\n[ \t]*(?:```|~~~)[^\n]*|.*)")

const codeOmitted = "[code omitted]"

// lineEndings maps CRLF and a lone CR to LF.
var lineEndings = strings.NewReplacer("\r\n", "\n", "\r", "\n")

// answerPolicyFor returns the answer policy of the agent's workflow, or nil.
func (h *Handler) answerPolicyFor(agent *PublishedAgent) *registry.WorkflowA2AAnswer {
	if h == nil || h.Registry == nil || agent == nil {
		return nil
	}
	wf := h.Registry.GetWorkflow(agent.WorkflowID)
	if wf == nil {
		return nil
	}
	return wf.A2A.Answer
}

// citationPrefixesFor returns the memorySearch.sourcePrefixes of the roles
// that run the workflow's agent steps in the agent's project (#614 C5 check
// 5). A role with no prefixes contributes none; nil means no citation can
// pass, which fails closed.
func (h *Handler) citationPrefixesFor(agent *PublishedAgent) []string {
	if h == nil || h.Registry == nil || agent == nil {
		return nil
	}
	wf, p := h.Registry.GetWorkflow(agent.WorkflowID), h.Registry.GetProject(agent.ProjectID)
	if wf == nil || p == nil {
		return nil
	}
	sw := h.Registry.GetSwarm(p.SwarmID)
	if sw == nil {
		return nil
	}
	var out []string
	for _, step := range wf.Steps {
		if step.Type != "agent" {
			continue
		}
		for i := range sw.Roles {
			if sw.Roles[i].Name == step.Role && sw.Roles[i].MemorySearch != nil {
				out = append(out, sw.Roles[i].MemorySearch.SourcePrefixes...)
			}
		}
	}
	return out
}

// emitPolicedAnswer is emitAnswerArtifact under a policy: the checks of
// §4.2 C5, in order.
func emitPolicedAnswer(ctx context.Context, w http.ResponseWriter, taskID string, p *registry.WorkflowA2AAnswer, prefixes []string, logger zerolog.Logger) {
	refuse := func(rule string) {
		logger.Warn().Str("task_id", taskID).Str("rule", rule).Msg("a2a: answer refused by the workflow's answer policy")
	}
	if streamDeps == nil || streamDeps.Tasks == nil {
		refuse("no task lookup")
		return
	}
	task, err := streamDeps.Tasks.Get(ctx, taskID)
	if err != nil || task == nil || task.Status != persistence.TaskStatusCompleted {
		refuse("task not completed")
		return
	}
	raw, rule := readAnswerArtifact(ctx, taskID, p.Artifact)
	if rule != "" {
		refuse(rule)
		return
	}
	payload, rule := projectAnswer(raw, p, prefixes)
	if rule != "" {
		refuse(rule)
		return
	}
	writeSSEArtifact(w, taskID, json.RawMessage(payload))
}

// readAnswerArtifact returns the content of the task's newest OUTPUT artifact
// named name (before disambiguation), read under A2AAnswerReadBound.
func readAnswerArtifact(ctx context.Context, taskID, name string) ([]byte, string) {
	if streamDeps == nil || streamDeps.Artifacts == nil || streamDeps.ArtifactOpener == nil {
		return nil, "no artifact store"
	}
	arts, err := streamDeps.Artifacts.List(ctx, persistence.ArtifactFilter{TaskID: &taskID, PageSize: 200})
	if err != nil {
		return nil, "artifact list failed"
	}
	var pick *persistence.Artifact
	for _, a := range arts {
		if a == nil || a.ArtifactClass != persistence.ArtifactClassOutput || executor.OriginalArtifactName(a.Name) != name {
			continue
		}
		if pick == nil || a.CreatedAt.After(pick.CreatedAt) {
			pick = a
		}
	}
	if pick == nil {
		return nil, "answer artifact missing"
	}
	rc, err := streamDeps.ArtifactOpener.Open(ctx, pick.ID)
	if err != nil {
		return nil, "answer artifact unreadable"
	}
	defer func() { _ = rc.Close() }()
	raw, err := io.ReadAll(io.LimitReader(rc, registry.A2AAnswerReadBound+1))
	if err != nil {
		return nil, "answer artifact unreadable"
	}
	if len(raw) > registry.A2AAnswerReadBound {
		return nil, "answer artifact over the read bound"
	}
	return raw, ""
}

// projectAnswer applies checks 3 to 7 of §4.2 C5 to the artifact's bytes: a
// JSON object, only the listed keys, citations within prefixes, the
// code-block rule on every string inside the kept keys, and the size cap on
// the result. It returns the payload to send, or the rule that refused it.
func projectAnswer(raw []byte, p *registry.WorkflowA2AAnswer, prefixes []string) ([]byte, string) {
	var obj map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&obj); err != nil || obj == nil {
		return nil, "answer is not a JSON object"
	}
	kept := make(map[string]any, len(p.Fields))
	for _, f := range p.Fields {
		if v, ok := obj[f]; ok {
			kept[f] = v
		}
	}
	if len(kept) == 0 {
		return nil, "answer holds none of the listed keys"
	}
	if c, ok := kept["citations"]; ok {
		if rule := checkCitations(c, prefixes); rule != "" {
			return nil, rule
		}
	}
	strip := p.CodeBlocks == "strip"
	for k, v := range kept {
		nv, fenced := applyCodeBlocks(v, strip)
		if fenced && !strip {
			return nil, "answer contains fenced code"
		}
		kept[k] = nv
	}
	out, err := json.Marshal(kept)
	if err != nil {
		return nil, "answer could not be encoded"
	}
	if len(out) > p.MaxBytes {
		return nil, "answer over maxBytes"
	}
	return out, ""
}

// applyCodeBlocks walks a decoded JSON value. It reports whether any string
// in it holds a fenced block and, when strip is set, returns the value with
// each block replaced by codeOmitted.
func applyCodeBlocks(v any, strip bool) (any, bool) {
	switch t := v.(type) {
	case string:
		// CommonMark ends a line at LF, CRLF or a lone CR. The fence rule
		// sees only LF, so normalise first, and send the normalised text:
		// what is checked is what leaves (push security review, a lone CR
		// before a fence passed).
		t = lineEndings.Replace(t)
		if !fencedBlock.MatchString(t) {
			return t, false
		}
		if !strip {
			return t, true
		}
		return fencedBlock.ReplaceAllStringFunc(t, func(m string) string {
			if len(m) > 0 && m[0] == '\n' {
				return "\n" + codeOmitted
			}
			return codeOmitted
		}), true
	case []any:
		found := false
		for i, e := range t {
			ne, f := applyCodeBlocks(e, strip)
			t[i], found = ne, found || f
		}
		return t, found
	case map[string]any:
		found := false
		for k, e := range t {
			ne, f := applyCodeBlocks(e, strip)
			t[k], found = ne, found || f
		}
		return t, found
	default:
		return v, false
	}
}

// answerEgress carries a stream's answer policy (nil = §3.1's behaviour) and
// the logger refusals go to.
type answerEgress struct {
	policy   *registry.WorkflowA2AAnswer
	prefixes []string // the citation prefixes of check 5
	logger   zerolog.Logger
}

// checkCitations is check 5 of §4.2 C5: citations is an array of strings,
// each beginning with one of the allowed source prefixes.
func checkCitations(v any, prefixes []string) string {
	list, ok := v.([]any)
	if !ok {
		return "citations is not an array"
	}
	for _, e := range list {
		c, ok := e.(string)
		if !ok {
			return "a citation is not a string"
		}
		// A prefix test alone passes "docs/architect/../legal/x" (code review
		// review-20261104-e78e F1): the path must be clean first.
		if !cleanCitationPath(c) {
			return "a citation is not a clean repository path"
		}
		allowed := false
		for _, p := range prefixes {
			if strings.HasPrefix(c, p) {
				allowed = true
				break
			}
		}
		if !allowed {
			return "a citation names a source outside the allowed prefixes"
		}
	}
	return ""
}

// cleanCitationPath reports whether c is a plain relative repository path:
// no backslash, no control character, no empty, "." or ".." segment.
func cleanCitationPath(c string) bool {
	if c == "" || strings.ContainsRune(c, '\\') || strings.IndexFunc(c, unicode.IsControl) >= 0 {
		return false
	}
	for _, seg := range strings.Split(c, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	return true
}
