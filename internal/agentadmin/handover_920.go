package agentadmin

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"vornik.io/vornik/internal/registry"
)

// HandOverInput is define_workflow's hand_over (agent-administered design
// part 24 §24.3 B1, GitHub #920): when the workflow completes, start another
// workflow of the namespace with chosen output files as its documents.
type HandOverInput struct {
	Project   string            `json:"project"`
	Workflow  string            `json:"workflow"`
	Documents map[string]string `json:"documents,omitempty"`
	Inputs    json.RawMessage   `json:"inputs,omitempty"`
}

const maxHandOverInputBytes = 4096

// renderHandOver checks a hand_over against the namespace and renders it as
// a JSON flow mapping for broker.hand_over; "" for none. id is the workflow
// being defined.
func renderHandOver(st *State, id string, in *HandOverInput) (string, string) {
	if in == nil {
		return "", ""
	}
	ns := st.Namespace
	target := WorkflowID(ns, projectSlug(ns, in.Project), strings.TrimSpace(in.Workflow))
	if target == id {
		return "", "hand_over cannot start the workflow itself"
	}
	tw, ok := st.Workflows[target]
	if !ok || tw.Loaded == nil || tw.Loaded.Broker == nil {
		return "", fmt.Sprintf("hand_over names %q, which is not a loaded workflow of this namespace; define it first", target)
	}
	declared := map[string]bool{}
	for _, d := range tw.Loaded.Broker.Documents() {
		declared[d.Property] = true
	}
	props := make([]string, 0, len(in.Documents))
	for p := range in.Documents {
		props = append(props, p)
	}
	sort.Strings(props)
	docs := make(map[string]string, len(props))
	for _, p := range props {
		if !declared[p] {
			return "", fmt.Sprintf("hand_over.documents.%s: %q declares no document input named %q (declare it in its inputs with x-untrusted-document)", p, target, p)
		}
		// A bare name is the form a workflow's artifacts list uses; complete
		// it. Any other path the loader would refuse is refused here, before
		// the change reaches the phone (GitHub #925).
		path := strings.TrimSpace(in.Documents[p])
		if path != "" && !strings.Contains(path, "/") {
			path = "artifacts/out/" + path
		}
		if !registry.ValidHandOverPath(path) {
			return "", fmt.Sprintf("hand_over.documents.%s: %q is not a file directly under artifacts/out/; name an output file, such as \"enrich.md\" or \"artifacts/out/enrich.md\"", p, in.Documents[p])
		}
		docs[p] = path
	}
	if len(in.Inputs) > maxHandOverInputBytes {
		return "", fmt.Sprintf("hand_over.inputs are over %d bytes", maxHandOverInputBytes)
	}
	inputs := map[string]any{}
	if len(bytes.TrimSpace(in.Inputs)) > 0 {
		dec := json.NewDecoder(bytes.NewReader(in.Inputs))
		dec.UseNumber()
		if err := dec.Decode(&inputs); err != nil {
			return "", "hand_over.inputs must be a JSON object"
		}
	}
	if why := handOverInputsFit(tw.Loaded, inputs, docs); why != "" {
		return "", why
	}
	if cycleFrom(st, id, target) {
		return "", fmt.Sprintf("hand_over to %q would close a loop: that workflow, or one it hands over to, hands back to %q", target, id)
	}
	h := map[string]any{"workflow": target}
	if len(docs) > 0 {
		h["documents"] = docs
	}
	if len(inputs) > 0 {
		h["inputs"] = inputs
	}
	out, err := canonicalOf(h)
	if err != nil {
		return "", "hand_over could not be encoded"
	}
	return string(out), ""
}

// handOverInputsFit validates the fixed inputs, with a placeholder for each
// mapped document, against the target's input schema, so a hand-over the
// target would refuse is refused at define time.
func handOverInputsFit(target *registry.Workflow, inputs map[string]any, docs map[string]string) string {
	schema, err := registry.CompileBrokerSchema(target.ID+"-inputs", target.Broker.InputSchema)
	if err != nil {
		return fmt.Sprintf("the target %q has an invalid input schema", target.ID)
	}
	doc := map[string]any{}
	for k, v := range inputs {
		doc[k] = v
	}
	for p := range docs {
		doc[p] = "x"
	}
	// Decode through JSON so numbers have the validator's types.
	b, _ := json.Marshal(doc)
	var v any
	_ = json.Unmarshal(b, &v)
	if err := schema.Validate(v); err != nil {
		return fmt.Sprintf("hand_over.inputs do not satisfy %q's input schema: %v", target.ID, err)
	}
	return ""
}

// cycleFrom reports whether a hand-over from id to target closes a loop,
// following every declared hand_over in the namespace with id's edge
// replaced by the new one (§24.3 B1: the whole graph, so either direction
// of defining a loop is refused).
func cycleFrom(st *State, id, target string) bool {
	next := func(w string) string {
		if w == id {
			return target
		}
		ws, ok := st.Workflows[w]
		if !ok || ws.Loaded == nil || ws.Loaded.Broker == nil || ws.Loaded.Broker.HandOver == nil {
			return ""
		}
		return ws.Loaded.Broker.HandOver.Workflow
	}
	seen := map[string]bool{}
	for w := target; w != ""; w = next(w) {
		if w == id {
			return true
		}
		if seen[w] {
			return false // a loop that does not pass through id was refused when it was defined
		}
		seen[w] = true
	}
	return false
}

// handOverSentence is the approval sentence's hand-over clause. It names
// both projects' connections, so the person sees what flows where
// (review-20261125-99e0 B4).
func handOverSentence(st *State, sourceProject string, h *registry.BrokerHandOver) string {
	if h == nil {
		return ""
	}
	from, to := "no connections", "no connections"
	if p := st.Projects[sourceProject]; p != nil {
		from = connectionsPhrase(p)
	}
	if p := st.Projects[ProjectOfWorkflow(h.Workflow)]; p != nil {
		to = connectionsPhrase(p)
	}
	s := fmt.Sprintf(" When it finishes (this project: %s), it starts the workflow %q (its project: %s)", from, h.Workflow, to)
	if props := h.DocumentProperties(); len(props) > 0 {
		parts := make([]string, 0, len(props))
		for _, p := range props {
			parts = append(parts, fmt.Sprintf("%s from %s", p, h.Documents[p]))
		}
		s += " and hands it " + strings.Join(parts, ", ")
	}
	return s + "."
}
