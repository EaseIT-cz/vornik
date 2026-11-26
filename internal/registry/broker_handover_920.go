package registry

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"vornik.io/vornik/internal/agentns"
)

// BrokerHandOver starts another workflow of the same agent namespace when
// this one COMPLETES, with chosen output files as that workflow's document
// inputs (agent-administered design part 24 §24.3, GitHub #920).
type BrokerHandOver struct {
	// Workflow is the target workflow's full ID (ns--project--slug).
	Workflow string `yaml:"workflow" json:"workflow"`
	// Documents maps a target document property to an artifacts/out/ path
	// among this workflow's harvested outputs.
	Documents map[string]string `yaml:"documents,omitempty" json:"documents,omitempty"`
	// Inputs are fixed values for the target's other inputs.
	Inputs map[string]any `yaml:"inputs,omitempty" json:"inputs,omitempty"`
}

// HandOverMaxDepth is the deepest a chain of hand-overs may go: a delegated
// or scheduled task is depth 0, so one delegation starts at most four tasks
// (§24.3 B3).
const HandOverMaxDepth = 3

var handOverPathRE = regexp.MustCompile(`^artifacts/out/[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// ValidHandOverPath reports whether p is a file directly under artifacts/out/,
// the only form a hand-over document may name. The admin verb uses it before
// the change is shown for approval (GitHub #925), the loader at load: one
// check.
func ValidHandOverPath(p string) bool {
	return handOverPathRE.MatchString(p) && !strings.Contains(p, "..")
}

// DocumentProperties returns the mapped properties, sorted.
func (h *BrokerHandOver) DocumentProperties() []string {
	if h == nil {
		return nil
	}
	out := make([]string, 0, len(h.Documents))
	for p := range h.Documents {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// validateHandOver checks the shape the registry can see alone: the target
// is another workflow of workflowID's namespace, and each document maps a
// well-formed property to a file path directly under artifacts/out/. That
// the target exists, declares the properties and closes no cycle is the
// admin verb's check, and the run-time check again (§24.3 B1, B2).
func validateHandOver(workflowID string, h *BrokerHandOver) error {
	if h == nil {
		return nil
	}
	ns, ok := agentns.FromID(workflowID)
	if !ok {
		return fmt.Errorf("broker.hand_over is for agent workflows only")
	}
	target := strings.TrimSpace(h.Workflow)
	tns, ok := agentns.FromID(target)
	if !ok || tns != ns {
		return fmt.Errorf("broker.hand_over.workflow must name a workflow of the namespace %q", ns)
	}
	if target == workflowID {
		return fmt.Errorf("broker.hand_over.workflow cannot be the workflow itself")
	}
	if len(h.Documents) > BrokerDocumentsMax {
		return fmt.Errorf("broker.hand_over.documents maps at most %d documents", BrokerDocumentsMax)
	}
	for _, p := range h.DocumentProperties() {
		if !brokerDocumentNameRE.MatchString(p) {
			return fmt.Errorf("broker.hand_over.documents: the property name must match %s", brokerDocumentNameRE.String())
		}
		if !ValidHandOverPath(h.Documents[p]) {
			return fmt.Errorf("broker.hand_over.documents.%s must be a file directly under artifacts/out/", p)
		}
	}
	return nil
}
