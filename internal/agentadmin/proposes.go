package agentadmin

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"vornik.io/vornik/internal/agentns"
	"vornik.io/vornik/internal/registry"
)

// ProposeInput is one write a workflow may propose (plan P4.8): the person
// approves the capability with the workflow, and each proposed write on
// their phone before it is made.
type ProposeInput struct {
	Action     string          `json:"action"`
	Tool       string          `json:"tool"` // mcp__<name>-write__<tool> | api:<name>:<METHOD>:<path>
	ArgsSchema json.RawMessage `json:"args_schema"`
	// Standing declares the write eligible for standing grants (broker
	// write-actions design, tier 2): the person may then approve future
	// writes with the same key without seeing their text. Part of the
	// reach, so declaring or changing it goes back to the device.
	Standing *StandingInput `json:"standing,omitempty"`
}

// StandingInput is a proposal's standing declaration (registry
// BrokerStanding); the loader's rules apply to it.
type StandingInput struct {
	Key     []string `json:"key"`
	MaxDays int      `json:"max_days,omitempty"`
	MaxUses int      `json:"max_uses,omitempty"`
}

// proposeData is one rendered proposal.
type proposeData struct {
	Action, Tool, Output, ArgsSchema string
	// Standing is the declaration as a JSON flow mapping; "" for none.
	Standing string
}

const maxProposes = 8

var actionRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,40}$`)

// parseProposes validates define_workflow's proposes against the project's
// approved write sets. It returns the rendered proposals and one phrase per
// proposal for the sentence, or why it is refused.
func parseProposes(st *State, p *ProjectState, raw json.RawMessage) ([]proposeData, []string, string) {
	if s := strings.TrimSpace(string(raw)); s == "" || s == "null" || s == "[]" {
		return nil, nil, ""
	}
	if !st.WritesOn {
		return nil, nil, "proposed writes are off on this installation (the operator's broker.writes setting)"
	}
	var in []ProposeInput
	if err := decodeStrict(raw, &in); err != nil {
		return nil, nil, fmt.Sprintf("proposes: %v", err)
	}
	if len(in) > maxProposes {
		return nil, nil, fmt.Sprintf("at most %d proposes", maxProposes)
	}
	seen := map[string]bool{}
	var out []proposeData
	var phrases []string
	for _, pr := range in {
		if !actionRe.MatchString(pr.Action) || seen[pr.Action] {
			return nil, nil, fmt.Sprintf("action %q must be a-z, 0-9 or _ (starting with a letter) and unique", pr.Action)
		}
		seen[pr.Action] = true
		phrase, why := approvedWrite(st, p, pr.Tool)
		if why != "" {
			return nil, nil, why
		}
		schema, err := canonicalOf(pr.ArgsSchema)
		if err != nil || len(pr.ArgsSchema) == 0 {
			return nil, nil, fmt.Sprintf("action %q needs an args_schema object", pr.Action)
		}
		d := proposeData{Action: pr.Action, Tool: pr.Tool, Output: "propose-" + pr.Action + ".json", ArgsSchema: string(schema)}
		if pr.Standing != nil {
			raw, err := json.Marshal(pr.Standing)
			if err != nil {
				return nil, nil, fmt.Sprintf("action %q: standing: %v", pr.Action, err)
			}
			d.Standing = string(raw)
			phrase += standingPhrase(pr)
		}
		out = append(out, d)
		phrases = append(phrases, pr.Action+" ("+phrase+")")
	}
	return out, phrases, ""
}

// approvedWrite reports whether tool is a write of this project the device
// approved, and how to name it in the sentence.
func approvedWrite(st *State, p *ProjectState, tool string) (string, string) {
	if api, method, path, ok := (registry.BrokerProposal{Tool: tool}).APITool(); ok {
		a, approved := st.Approvals[p.ID][api]
		if !approved || !a.Live() || a.Kind != "api" || !contains(a.Write, method) {
			return "", fmt.Sprintf("%s is not an approved write of the API %q; add it with add_api (writes: true) first", method, api)
		}
		return api + ": " + method + " " + path, ""
	}
	server, name, ok := splitMCPTool(tool)
	if !ok || !strings.HasSuffix(server, agentns.WriteSuffix) {
		return "", fmt.Sprintf("%q is not a proposable write; name mcp__<server>-write__<tool> or api:<name>:<METHOD>:<path>", tool)
	}
	integration := agentns.IntegrationOf(server)
	a, approved := st.Approvals[p.ID][integration]
	if !approved || !a.Live() || a.ReadPending || !contains(a.Write, name) {
		return "", fmt.Sprintf("%q is not an approved write tool of the server %q", name, integration)
	}
	return integration + ": " + name, ""
}

// standingPhrase is what the approval sentence says of a standing
// declaration (broker write-actions design, tier 2 revised): it names the
// fields a covered write sends unseen, never "similar".
func standingPhrase(pr ProposeInput) string {
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	_ = json.Unmarshal(pr.ArgsSchema, &schema)
	inKey := map[string]bool{}
	for _, k := range pr.Standing.Key {
		inKey[k] = true
	}
	var unseen []string
	for name := range schema.Properties {
		if !inKey[name] {
			unseen = append(unseen, name)
		}
	}
	sort.Strings(unseen)
	what := "their text"
	if len(unseen) > 0 {
		what = "their " + strings.Join(unseen, ", ")
	}
	return fmt.Sprintf("; you may also let future ones to the same %s be sent without showing you their text (%s)",
		strings.Join(pr.Standing.Key, ", "), what)
}
