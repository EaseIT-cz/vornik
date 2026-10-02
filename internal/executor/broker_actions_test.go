package executor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/egressscan"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/secrets"
)

// Broker write-actions design §5.1: proposals are staged before COMPLETED,
// promoted after it, and a store error fails the step.

type fakeBrokerActions struct {
	persistence.BrokerActionRepository // unused methods panic
	staged                             []*persistence.BrokerAction
	promoted                           []string
	promoteN                           *int64
	promoteErr                         error
	stageErr                           error
	events                             *[]string
}

func (f *fakeBrokerActions) Stage(_ context.Context, a *persistence.BrokerAction) (bool, error) {
	if f.events != nil {
		*f.events = append(*f.events, "stage")
	}
	if f.stageErr != nil {
		return false, f.stageErr
	}
	cp := *a
	f.staged = append(f.staged, &cp)
	return true, nil
}

func (f *fakeBrokerActions) PromoteStaged(_ context.Context, taskID string) (int64, error) {
	if f.events != nil {
		*f.events = append(*f.events, "promote")
	}
	f.promoted = append(f.promoted, taskID)
	if f.promoteErr != nil {
		return 0, f.promoteErr
	}
	if f.promoteN != nil {
		return *f.promoteN, nil
	}
	return 1, nil
}

type brokerResolver struct{ wf *registry.Workflow }

func (r brokerResolver) GetProject(string) *registry.Project { return nil }
func (r brokerResolver) GetSwarm(string) *registry.Swarm     { return nil }
func (r brokerResolver) GetWorkflow(id string) *registry.Workflow {
	if r.wf != nil && r.wf.ID == id {
		return r.wf
	}
	return nil
}

type listArtifacts struct {
	MockArtifactRepo
	arts []*persistence.Artifact
}

func (l *listArtifacts) List(context.Context, persistence.ArtifactFilter) ([]*persistence.Artifact, error) {
	return l.arts, nil
}

func proposingWorkflow() *registry.Workflow {
	return &registry.Workflow{ID: "mail-reply", Broker: &registry.WorkflowBroker{
		Proposes: []registry.BrokerProposal{{
			Action: "gmail_reply", Tool: "mcp__w__send", Output: "proposal.json",
			ArgsSchema: map[string]any{
				"type": "object", "additionalProperties": false, "required": []any{"to"},
				"properties": map[string]any{"to": map[string]any{"type": "string", "format": "email", "maxLength": 254}},
			},
		}},
	}}
}

func stagingExecutor(t *testing.T, proposal string) (*Executor, *fakeBrokerActions) {
	t.Helper()
	e, _, _, _, _ := setup()
	e.workflows = brokerResolver{wf: proposingWorkflow()}
	var arts []*persistence.Artifact
	if proposal != "" {
		path := filepath.Join(t.TempDir(), "proposal.json")
		if err := os.WriteFile(path, []byte(proposal), 0o600); err != nil {
			t.Fatal(err)
		}
		arts = []*persistence.Artifact{{ID: "a1", Name: "proposal-20260930-ab12.json", ArtifactClass: persistence.ArtifactClassOutput, StoragePath: path, CreatedAt: time.Now()}}
	}
	e.artifactRepo = &listArtifacts{arts: arts}
	e.artifactStore = nil
	fake := &fakeBrokerActions{}
	e.brokerActions = fake
	return e, fake
}

func stageFor(t *testing.T, e *Executor) error {
	t.Helper()
	key := "akey-hermes"
	return e.stageBrokerActions(context.Background(),
		&persistence.Task{ID: "t1", ProjectID: "broker-p", CreatedByAPIKeyID: &key},
		&persistence.Execution{ID: "x1", WorkflowID: "mail-reply"})
}

func TestStageBrokerActions_Outcomes(t *testing.T) {
	cases := []struct {
		name, proposal, want string
	}{
		{"valid", `{"action":"gmail_reply","args":{"to":"jana@example.com"}}`, persistence.BrokerActionStaged},
		{"missing", "", persistence.BrokerActionProposalMissing},
		{"not json", `hello`, persistence.BrokerActionProposalInvalid},
		{"wrong action", `{"action":"other","args":{"to":"jana@example.com"}}`, persistence.BrokerActionProposalInvalid},
		{"fails schema", `{"action":"gmail_reply","args":{"to":"not-an-address"}}`, persistence.BrokerActionProposalInvalid},
		{"extra field", `{"action":"gmail_reply","args":{"to":"jana@example.com","bcc":"x@y.z"}}`, persistence.BrokerActionProposalInvalid},
		{"duplicate keys", `{"action":"gmail_reply","args":{"to":"a@b.c","to":"x@y.z"}}`, persistence.BrokerActionProposalInvalid},
		{"oversize", `{"action":"gmail_reply","args":{"to":"` + strings.Repeat("a", 9000) + `@b.c"}}`, persistence.BrokerActionProposalInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, fake := stagingExecutor(t, tc.proposal)
			if err := stageFor(t, e); err != nil {
				t.Fatalf("stage: %v", err)
			}
			if len(fake.staged) != 1 {
				t.Fatalf("staged %d rows, want 1", len(fake.staged))
			}
			row := fake.staged[0]
			if row.Status != tc.want {
				t.Fatalf("status = %s, want %s", row.Status, tc.want)
			}
			if row.APIKeyID != "akey-hermes" || row.Tool != "mcp__w__send" || row.ExpiresAt.Sub(row.CreatedAt) != 24*time.Hour {
				t.Fatalf("row = %+v", row)
			}
			if tc.want == persistence.BrokerActionStaged && string(row.ArgsJSON) != `{"to":"jana@example.com"}` {
				t.Fatalf("args not canonical: %s", row.ArgsJSON)
			}
			if tc.want != persistence.BrokerActionStaged && string(row.ArgsJSON) != `{}` {
				t.Fatalf("a terminal outcome must carry empty args, got %s", row.ArgsJSON)
			}
		})
	}
}

func TestStageBrokerActions_NoopWithoutProposes(t *testing.T) {
	e, fake := stagingExecutor(t, "")
	e.workflows = brokerResolver{wf: &registry.Workflow{ID: "mail-reply"}}
	if err := stageFor(t, e); err != nil || len(fake.staged) != 0 {
		t.Fatalf("a workflow without proposes stages nothing: %v, %d", err, len(fake.staged))
	}
}

func TestStageBrokerActions_StoreErrorIsRetryableClass(t *testing.T) {
	e, fake := stagingExecutor(t, `{"action":"gmail_reply","args":{"to":"jana@example.com"}}`)
	fake.stageErr = errors.New("database is locked")
	err := stageFor(t, e)
	if err == nil {
		t.Fatal("a store error must fail staging")
	}
	if got := ClassifyExecutionFailure(err, ""); got != "broker_action_store" {
		t.Fatalf("class = %q, want broker_action_store", got)
	}
	if persistence.IsTerminalFailureClass("broker_action_store") {
		t.Fatal("broker_action_store must be retryable")
	}
}

// completedWriteRE is a syntactic heuristic, not a semantic one: a
// COMPLETED write split across lines differently would not match. It fails
// closed either way, because the scan also requires exactly two matches.
var completedWriteRE = regexp.MustCompile(`UpdateStatus\([^)]*persistence\.TaskStatusCompleted\)|\}\s*,\s*persistence\.TaskStatusCompleted,`)

// Every COMPLETED transition in this package stages before and promotes
// after it, in the same function (design §5.1, review round 3 M2).
func TestEveryCompletedTransitionStagesAndPromotes(t *testing.T) {
	files, _ := filepath.Glob("*.go")
	funcRE := regexp.MustCompile(`(?m)^func `)
	checked := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, _ := os.ReadFile(f)
		src := string(b)
		starts := funcRE.FindAllStringIndex(src, -1)
		for i, s := range starts {
			end := len(src)
			if i+1 < len(starts) {
				end = starts[i+1][0]
			}
			fn := src[s[0]:end]
			// A COMPLETED *write*: UpdateStatus to COMPLETED, or a
			// TransitionConditional whose target (the argument after the
			// source-state slice) is COMPLETED. A COMPLETED in a switch or a
			// source set is a read and does not count.
			loc := completedWriteRE.FindStringIndex(fn)
			if loc == nil {
				continue
			}
			idx := loc[0]
			checked++
			stage := strings.Index(fn, "e.stageBrokerActions(")
			promote := strings.Index(fn, "e.promoteBrokerActions(")
			if stage < 0 || stage > idx || promote < idx {
				t.Errorf("%s: a COMPLETED transition must be preceded by stageBrokerActions and followed by promoteBrokerActions", f)
			}
		}
	}
	if checked != 2 {
		t.Fatalf("found %d COMPLETED writes; expected exactly handleSuccess and the closure request. A new one must call the seam, and this count must be raised with it", checked)
	}
}

func TestPromoteBrokerActions(t *testing.T) {
	e, fake := stagingExecutor(t, "")
	e.promoteBrokerActions(context.Background(), &persistence.Task{ID: "t9"})
	if len(fake.promoted) != 1 || fake.promoted[0] != "t9" {
		t.Fatalf("promoted = %v", fake.promoted)
	}
	e.brokerActions = nil
	e.promoteBrokerActions(context.Background(), &persistence.Task{ID: "t9"}) // no store: no-op
}

// A staging store error in handleSuccess takes the ordinary failure path:
// the task is not COMPLETED, and the class is the retryable
// broker_action_store (design §5.1, review round 2 F4).
func TestHandleSuccess_StagingStoreErrorFailsTheAttempt(t *testing.T) {
	e, _, er, _, tr := setup()
	e.workflows = brokerResolver{wf: proposingWorkflow()}
	e.brokerActions = &fakeBrokerActions{stageErr: errors.New("database is locked")}
	task := &persistence.Task{ID: "t-stage", ProjectID: "broker-p", Status: persistence.TaskStatusRunning, Attempt: 1, MaxAttempts: 3, CreatedAt: time.Now()}
	tr.AddTask(task)
	exec := &persistence.Execution{ID: "x-stage", TaskID: task.ID, ProjectID: task.ProjectID, WorkflowID: "mail-reply", Status: persistence.ExecutionStatusRunning}
	if err := er.Create(context.Background(), exec); err != nil {
		t.Fatal(err)
	}
	e.handleSuccess(context.Background(), task, exec, "", []byte(`{}`))
	got, err := tr.Get(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status == persistence.TaskStatusCompleted {
		t.Fatal("a task whose proposals could not be staged must not be COMPLETED")
	}
	if got.LastErrorClass == nil || *got.LastErrorClass != "broker_action_store" {
		t.Fatalf("last error class = %v, want broker_action_store", got.LastErrorClass)
	}
}

// With staging succeeding, handleSuccess completes the task and promotes.
func TestHandleSuccess_StagesThenCompletesThenPromotes(t *testing.T) {
	e, _, er, _, tr := setup()
	e.workflows = brokerResolver{wf: proposingWorkflow()}
	var events []string
	fake := &fakeBrokerActions{events: &events}
	e.brokerActions = fake
	e.artifactRepo = &listArtifacts{}
	task := &persistence.Task{ID: "t-ok", ProjectID: "broker-p", Status: persistence.TaskStatusRunning, Attempt: 1, MaxAttempts: 3, CreatedAt: time.Now()}
	tr.AddTask(task)
	exec := &persistence.Execution{ID: "x-ok", TaskID: task.ID, ProjectID: task.ProjectID, WorkflowID: "mail-reply", Status: persistence.ExecutionStatusRunning}
	if err := er.Create(context.Background(), exec); err != nil {
		t.Fatal(err)
	}
	e.handleSuccess(context.Background(), task, exec, "", []byte(`{}`))
	got, _ := tr.Get(context.Background(), task.ID)
	if got.Status != persistence.TaskStatusCompleted {
		t.Fatalf("status = %s, want COMPLETED", got.Status)
	}
	if len(events) != 2 || events[0] != "stage" || events[1] != "promote" {
		t.Fatalf("events = %v, want [stage promote]", events)
	}
	if len(fake.staged) != 1 || fake.staged[0].Status != persistence.BrokerActionProposalMissing {
		t.Fatalf("no proposal file: expected one proposal_missing row, got %+v", fake.staged)
	}
}

// Design §5.1 (review round 2 F3): memory ingest never takes a broker
// workflow's declared egress or proposal outputs, whatever their extension.
// The loader already requires .json for both; the skip is by name so a
// workflow built outside the loader cannot leak a draft into memory either.
func TestIngestOutputArtifacts_SkipsDeclaredBrokerOutputs(t *testing.T) {
	dir := t.TempDir()
	body := writeArtifactFile(t, dir, "x.md", "# drafted reply to jana@example.com")
	wf := &registry.Workflow{ID: "mail-reply", Broker: &registry.WorkflowBroker{
		Egress:   registry.BrokerEgress{Output: "summary.md"},
		Proposes: []registry.BrokerProposal{{Action: "gmail_reply", Output: "proposal.md"}},
	}}
	ar := &inMemArtifactRepo{artifacts: []*persistence.Artifact{
		{ID: "a-egress", Name: "summary-20260930-ab12.md", ArtifactClass: persistence.ArtifactClassOutput, StoragePath: body},
		{ID: "a-proposal", Name: "proposal-20260930-ab12.md", ArtifactClass: persistence.ArtifactClassOutput, StoragePath: body},
		{ID: "a-notes", Name: "notes.md", ArtifactClass: persistence.ArtifactClassOutput, StoragePath: body},
	}}
	mi := &stubMemoryIndexer{}
	e := &Executor{memoryIndexer: mi, artifactRepo: ar, logger: zerolog.Nop(), workflows: brokerResolver{wf: wf}}
	e.ingestOutputArtifacts(context.Background(),
		&persistence.Task{ID: "t", ProjectID: "p", Status: persistence.TaskStatusCompleted},
		&persistence.Execution{ID: "x", WorkflowID: "mail-reply"})
	if len(mi.calls) != 1 || mi.calls[0].artifactID != "a-notes" {
		t.Fatalf("only the undeclared output may be ingested, got %+v", mi.calls)
	}
}

// review-20260930-ac20: an oversize proposal is decided from the recorded
// size, without reading the file (the path here does not even exist).
func TestStageBrokerActions_OversizeByRecordedSizeIsNotRead(t *testing.T) {
	e, fake := stagingExecutor(t, "")
	size := int64(1 << 20)
	e.artifactRepo = &listArtifacts{arts: []*persistence.Artifact{{
		ID: "big", Name: "proposal-20260930-ab12.json", ArtifactClass: persistence.ArtifactClassOutput,
		StoragePath: "/nonexistent/proposal.json", SizeBytes: &size, CreatedAt: time.Now(),
	}}}
	if err := stageFor(t, e); err != nil {
		t.Fatalf("an oversize proposal is an outcome, not a read error: %v", err)
	}
	if len(fake.staged) != 1 || fake.staged[0].Status != persistence.BrokerActionProposalInvalid {
		t.Fatalf("staged = %+v, want one proposal_invalid", fake.staged)
	}
}

// Design §5.3: promotion is when a person is needed, so it notifies — once,
// with the count, and only when rows were actually promoted.
func TestPromoteBrokerActions_NotifiesOncePerPromotion(t *testing.T) {
	e, fake := stagingExecutor(t, "")
	var got []string
	e.brokerActionNotify = func(_ context.Context, project, taskID string, n int) {
		got = append(got, fmt.Sprintf("%s|%s|%d", project, taskID, n))
	}
	two := int64(2)
	fake.promoteN = &two
	e.promoteBrokerActions(context.Background(), &persistence.Task{ID: "t9", ProjectID: "broker-p"})
	if len(got) != 1 || got[0] != "broker-p|t9|2" {
		t.Fatalf("notifications = %v", got)
	}
	zero := int64(0)
	fake.promoteN = &zero
	e.promoteBrokerActions(context.Background(), &persistence.Task{ID: "t9", ProjectID: "broker-p"})
	fake.promoteN, fake.promoteErr = nil, errors.New("db down")
	e.promoteBrokerActions(context.Background(), &persistence.Task{ID: "t9", ProjectID: "broker-p"})
	if len(got) != 1 {
		t.Fatalf("notified without a promotion: %v", got)
	}
}

// Plan P5.3: an agent project's proposed arguments are scanned at staging; a
// credential-shaped value stages the row proposal_invalid, so no approval is
// ever asked for a key. Without a scanner an agent proposal is refused
// (fail closed); operator projects are unchanged. Control: the egress branch
// of brokerActionRow.
func TestStageBrokerActions_AgentArgsScanned(t *testing.T) {
	d, err := secrets.NewMultiDetector(secrets.Config{})
	if err != nil {
		t.Fatal(err)
	}
	wf := proposingWorkflow()
	wf.Broker.Proposes[0].ArgsSchema["properties"].(map[string]any)["note"] = map[string]any{"type": "string", "maxLength": 200, "x-untrusted": true}
	keyed := `{"action":"gmail_reply","args":{"note":"AKIAQWERTYUIOPASDFGH","to":"jana@example.com"}}`
	stage := func(project string, withScan bool) *persistence.BrokerAction {
		e, fake := stagingExecutor(t, keyed)
		e.workflows = brokerResolver{wf: wf}
		var recorded []string
		if withScan {
			e.egressDetector = d
			e.egressRecord = func(surface, _, _ string, fs []egressscan.Finding, _ secrets.Action) {
				for _, f := range fs {
					recorded = append(recorded, surface+"|"+f.Path)
				}
			}
		}
		key := "k"
		if err := e.stageBrokerActions(context.Background(), &persistence.Task{ID: "t1", ProjectID: project, CreatedByAPIKeyID: &key},
			&persistence.Execution{ID: "x1", WorkflowID: "mail-reply"}); err != nil {
			t.Fatal(err)
		}
		if withScan && project != "broker-p" && (len(recorded) == 0 || recorded[0] != "action_args|$.note") {
			t.Fatalf("recorded %q", recorded)
		}
		return fake.staged[0]
	}
	if row := stage("hermes--comms", true); row.Status != persistence.BrokerActionProposalInvalid || strings.Contains(string(row.ArgsJSON), "AKIA") {
		t.Fatalf("an agent proposal with a key: %s %s", row.Status, row.ArgsJSON)
	}
	if row := stage("hermes--comms", false); row.Status != persistence.BrokerActionProposalInvalid {
		t.Fatalf("an agent proposal with no scanner: %s", row.Status)
	}
	if row := stage("broker-p", true); row.Status != persistence.BrokerActionStaged {
		t.Fatalf("an operator proposal changed: %s", row.Status)
	}
}
