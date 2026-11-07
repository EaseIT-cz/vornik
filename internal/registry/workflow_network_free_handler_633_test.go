package registry

import "testing"

// GitHub #633: text.readability makes no network call (its package refuses
// every network import, handlers/text/network_law_test.go), so a companion
// review that measures after its reviewer stays network-incapable in the
// catalogue. Every other system handler still makes a workflow
// possibly-capable.
func TestWorkflowIsNetworkIncapable_NetworkFreeSystemHandler(t *testing.T) {
	sw := swarmWithRole("reviewer", "file_read", "read_many_files")
	wf := agentWorkflow("reviewer")
	wf.Steps["measure"] = WorkflowStep{Type: "system", Handler: "text.readability", OnSuccess: "done"}
	if !WorkflowIsNetworkIncapable(wf, sw) {
		t.Fatal("a network-free system handler must not make the workflow capable")
	}
	wf.Steps["measure"] = WorkflowStep{Type: "system", Handler: "forge.fetch_diff", OnSuccess: "done"}
	if WorkflowIsNetworkIncapable(wf, sw) {
		t.Fatal("any other system handler keeps the workflow possibly-capable")
	}
	only := &Workflow{ID: "wf", Entrypoint: "measure", Steps: map[string]WorkflowStep{
		"measure": {Type: "system", Handler: "text.readability", OnSuccess: "done"},
	}}
	if WorkflowIsNetworkIncapable(only, sw) {
		t.Fatal("a workflow with no agent step has nothing proven, so it is not claimed incapable")
	}
}
