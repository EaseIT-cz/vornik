package api

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/egressscan"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/secrets"
)

const egressCanary = "AKIAQWERTYUIOPASDFGH"

type argsRecorder struct {
	mu   sync.Mutex
	args []string
}

func (a *argsRecorder) Tools(string) []chat.Tool { return nil }
func (a *argsRecorder) Execute(_ context.Context, _, _, argsJSON string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.args = append(a.args, argsJSON)
	return "ok", nil
}

func egressExecutor(t *testing.T, operator secrets.Action) (*ComposedMCPExecutor, *argsRecorder, *[]string) {
	t.Helper()
	d, err := secrets.NewMultiDetector(secrets.Config{})
	if err != nil {
		t.Fatal(err)
	}
	ext := &argsRecorder{}
	var seen []string
	c := &ComposedMCPExecutor{External: ext, Egress: &EgressScan{Detector: d,
		Policy: func(string) secrets.Action { return operator },
		Record: func(_, projectID, tool string, fs []egressscan.Finding, action secrets.Action) {
			for _, f := range fs {
				seen = append(seen, projectID+"|"+tool+"|"+f.Path+"|"+string(action))
			}
		}}}
	return c, ext, &seen
}

// Agent-administered Vornik plan P5.1 (Part A §3.2): every tool call's
// arguments are scanned before dispatch. An agent project's credential-shaped
// finding refuses the call, naming the path and never the value; an operator
// project follows its checkpoint (detect forwards, redact masks). Control:
// ComposedMCPExecutor.egressCheck.
func TestComposedExecutor_EgressScan(t *testing.T) {
	ctx := context.Background()
	c, ext, seen := egressExecutor(t, secrets.ActionDetect)
	_, err := c.Execute(ctx, "hermes--fin", "mcp__mail__send", `{"body":"my key `+egressCanary+`"}`)
	if err == nil || !strings.Contains(err.Error(), "$.body") || strings.Contains(err.Error(), egressCanary) {
		t.Fatalf("an agent call with a key: %v", err)
	}
	if len(ext.args) != 0 {
		t.Fatal("the agent call reached the server")
	}
	if len(*seen) != 1 || !strings.HasSuffix((*seen)[0], "|block") || strings.Contains((*seen)[0], egressCanary) {
		t.Fatalf("recorded %q", *seen)
	}
	// An opaque ID does not block an agent call.
	if _, err := c.Execute(ctx, "hermes--fin", "mcp__mail__get", `{"id":"msg-0f8a9c2e7b1d4e6f9a0b3c5d7e9f1a2b"}`); err != nil {
		t.Fatalf("an opaque ID blocked: %v", err)
	}
	// Operator, detect: forwarded unchanged.
	if _, err := c.Execute(ctx, "assistant", "mcp__mail__send", `{"body":"`+egressCanary+`"}`); err != nil || !strings.Contains(ext.args[len(ext.args)-1], egressCanary) {
		t.Fatalf("operator detect: %v %q", err, ext.args)
	}
	// Operator, redact: forwarded masked.
	r, extR, _ := egressExecutor(t, secrets.ActionRedact)
	if _, err := r.Execute(ctx, "assistant", "mcp__mail__send", `{"body":"`+egressCanary+`"}`); err != nil || strings.Contains(extR.args[0], egressCanary) {
		t.Fatalf("operator redact: %v %q", err, extR.args)
	}
	// Agent projects block whatever the operator policy says (review
	// 20261002-cb24 F2: "cannot be configured down").
	if _, err := r.Execute(ctx, "hermes--fin", "mcp__mail__send", `{"body":"`+egressCanary+`"}`); err == nil || len(extR.args) != 1 {
		t.Fatalf("an agent call under an operator redact policy: %v", err)
	}
	// Operator, block: refused.
	b, extB, _ := egressExecutor(t, secrets.ActionBlock)
	if _, err := b.Execute(ctx, "assistant", "mcp__mail__send", `{"body":"`+egressCanary+`"}`); err == nil || len(extB.args) != 0 {
		t.Fatalf("operator block: %v", err)
	}
}

// Fail closed for agent projects (plan P5 global rule): with no scanner, an
// agent call is refused; an operator call passes.
func TestComposedExecutor_EgressScanAbsent(t *testing.T) {
	ext := &argsRecorder{}
	c := &ComposedMCPExecutor{External: ext}
	if _, err := c.Execute(context.Background(), "hermes--fin", "mcp__mail__send", `{}`); err == nil {
		t.Fatal("an agent call ran without the egress scan")
	}
	if _, err := c.Execute(context.Background(), "assistant", "mcp__mail__send", `{}`); err != nil {
		t.Fatalf("an operator call was refused: %v", err)
	}
}

// Plan P5.4 (spec §10.2, round 2 M5): an agent workflow's egress document is
// scanned before the result returns it; a credential-shaped value fails the
// result with a reason naming the field, and no document is returned.
// Operator broker workflows are unchanged. Control: the agent branch of
// companionBrokerResult.
func TestBrokerResult_AgentEgressDocumentScanned(t *testing.T) {
	doc := `{"items":[{"one_line":"Invoice due"},{"one_line":"key ` + egressCanary + `"}]}`
	srv, _, taskRepo := newBrokerMCPServer(t)
	brokerTask(t, srv, taskRepo, persistence.TaskStatusCompleted, doc, "")
	d, _ := secrets.NewMultiDetector(secrets.Config{})
	srv.egress = &EgressScan{Detector: d}
	wf := srv.projectRegistry.GetWorkflow("mail-digest")
	result := func(project string) map[string]any {
		task := &persistence.Task{ID: "t1", ProjectID: project, WorkflowID: &wf.ID, Status: persistence.TaskStatusCompleted, UpdatedAt: time.Now()}
		text, err := srv.companionBrokerResult(context.Background(), task, wf)
		if err != nil {
			t.Fatal(err)
		}
		if project != "broker-acme" && strings.Contains(text, egressCanary) {
			t.Fatalf("the agent result carries the key: %s", text)
		}
		var out map[string]any
		_ = json.Unmarshal([]byte(text), &out)
		return out
	}
	out := result("hermes--fin")
	if out["egress_error"] != brokerErrEgressSecret || out["output"] != nil ||
		!strings.Contains(fmt.Sprint(out["egress_error_detail"]), "$.items[1].one_line") {
		t.Fatalf("agent result: %+v", out)
	}
	if out := result("broker-acme"); out["egress_error"] != nil {
		t.Fatalf("an operator result changed: %+v", out)
	}
}
