package a2a

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"vornik.io/vornik/internal/executor/livepubsub"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/repotest"
	"vornik.io/vornik/internal/registry"
)

// #614 C5/C7 (architect design §4.2): a published workflow with an a2a
// answer policy sends only the named artifact's listed keys, only for a
// COMPLETED task, under a raw read bound and a size cap, with fenced code
// refused or stripped. Before the change every OUTPUT artifact, every key,
// and the output of failed and canceled runs reached the caller (§3.1).

var testPrefixes = []string{"docs/architect/", "docs/public/"}

func answerPolicy() *registry.WorkflowA2AAnswer {
	return &registry.WorkflowA2AAnswer{Artifact: "result.json", Fields: []string{"answer", "citations"}, MaxBytes: 8192}
}

func TestProjectAnswer_614(t *testing.T) {
	strip := answerPolicy()
	strip.CodeBlocks = "strip"
	small := answerPolicy()
	small.MaxBytes = 40
	for _, tc := range []struct {
		name    string
		policy  *registry.WorkflowA2AAnswer
		raw     string
		want    string // exact projected JSON, when ok
		refused bool
	}{
		{"extra keys dropped", answerPolicy(), `{"answer":"a","citations":["docs/public/x.md"],"debug":"SECRET"}`, `{"answer":"a","citations":["docs/public/x.md"]}`, false},
		{"a missing listed key is just absent", answerPolicy(), `{"answer":"a"}`, `{"answer":"a"}`, false},
		{"not an object", answerPolicy(), `["answer"]`, "", true},
		{"not JSON", answerPolicy(), `answer: a`, "", true},
		{"no listed key at all", answerPolicy(), `{"debug":"x"}`, "", true},
		{"fenced code refused by default", answerPolicy(), "{\"answer\":\"see\\n```go\\nfunc x() {}\\n```\\n\"}", "", true},
		{"tilde fence refused too", answerPolicy(), "{\"answer\":\"~~~\\nrm -rf\\n~~~\"}", "", true},
		{"a fence inside citations is refused", answerPolicy(), "{\"answer\":\"a\",\"citations\":[\"```x```\"]}", "", true},
		{"strip replaces the block", strip, "{\"answer\":\"before\\n```go\\nfunc x() {}\\n```\\nafter\"}", `{"answer":"before\n[code omitted]\nafter"}`, false},
		{"strip an unterminated fence to the end", strip, "{\"answer\":\"before\\n```\\nsecret\"}", `{"answer":"before\n[code omitted]"}`, false},
		{"a fence after a lone CR is refused", answerPolicy(), "{\"answer\":\"see\\r```\\rrm -rf\\r```\"}", "", true},
		{"a fence after CRLF is refused", answerPolicy(), "{\"answer\":\"see\\r\\n```\\nx\\r\\n```\"}", "", true},
		{"line endings are normalised in what is sent", answerPolicy(), "{\"answer\":\"a\\r\\nb\\rc\"}", `{"answer":"a\nb\nc"}`, false},
		{"inline backticks are not a fence", answerPolicy(), "{\"answer\":\"set `limit` to 8\"}", "{\"answer\":\"set `limit` to 8\"}", false},
		{"duplicate key: the last value is judged", answerPolicy(), "{\"answer\":\"safe\",\"answer\":\"```x```\"}", "", true},
		{"numbers keep their precision", answerPolicy(), `{"answer":12345678901234567890123}`, `{"answer":12345678901234567890123}`, false},
		{"a citation outside the prefixes", answerPolicy(), `{"answer":"a","citations":["https://docs.vornik.io"]}`, "", true},
		{"a parent segment climbs out of a prefix", answerPolicy(), `{"answer":"a","citations":["docs/architect/../legal/dpa.md"]}`, "", true},
		{"a dot segment", answerPolicy(), `{"answer":"a","citations":["docs/public/./x.md"]}`, "", true},
		{"a backslash", answerPolicy(), `{"answer":"a","citations":["docs/public/..\\legal.md"]}`, "", true},
		{"a control character", answerPolicy(), "{\"answer\":\"a\",\"citations\":[\"docs/public/x.md\\r\"]}", "", true},
		{"an empty segment", answerPolicy(), `{"answer":"a","citations":["docs/public//x.md"]}`, "", true},
		{"a citation that is not a string", answerPolicy(), `{"answer":"a","citations":[{"source":"docs/public/x.md"}]}`, "", true},
		{"citations that are not an array", answerPolicy(), `{"answer":"a","citations":"docs/public/x.md"}`, "", true},
		{"empty citations pass", answerPolicy(), `{"answer":"Not found in the design record.","citations":[]}`, `{"answer":"Not found in the design record.","citations":[]}`, false},
		{"over maxBytes after projection", small, `{"answer":"` + strings.Repeat("x", 60) + `"}`, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, reason := projectAnswer([]byte(tc.raw), tc.policy, testPrefixes)
			if tc.refused {
				if reason == "" {
					t.Fatalf("want a refusal, got %s", got)
				}
				return
			}
			if reason != "" {
				t.Fatalf("unexpected refusal %q", reason)
			}
			if string(got) != tc.want {
				t.Fatalf("got %s\nwant %s", got, tc.want)
			}
		})
	}
}

// policedStream runs the A2A stream for a task in the given status, with the
// published "research" workflow carrying the answer policy.
func policedStream(t *testing.T, status persistence.TaskStatus, policy *registry.WorkflowA2AAnswer, arts []*persistence.Artifact, content map[string]string, envelope []byte) string {
	t.Helper()
	ch := make(chan livepubsub.LiveEvent)
	h, _ := newTestHandler()
	reg := h.Registry.(*fakeRegistry)
	reg.workflows[0].A2A.Answer = policy
	reg.projects[0].SwarmID = "demo-swarm"
	reg.swarms = []*registry.Swarm{{ID: "demo-swarm", Roles: []registry.SwarmRole{{Name: "researcher",
		MemorySearch: &registry.MemorySearchPolicy{RepoScope: "github.com/x/y", SourcePrefixes: testPrefixes}}}}}
	h.LiveSubscriber = &fakeSubscriber{ch: ch}
	wireSSEForTest(t, &SSEDeps{
		Tasks:          taskLookupStatusEnvelope{status: status, envelope: envelope},
		Executions:     fakeExecLookup{exec: &persistence.Execution{ID: "exec-1", TaskID: "demo-task"}},
		Artifacts:      fakeArtifactLister{arts: arts},
		ArtifactOpener: fakeArtifactOpener{content: content},
	})
	rec := httptest.NewRecorder()
	h.HandleAgentRoute(rec, httptest.NewRequest(http.MethodGet, "/a2a/v1/agents/demo/research/tasks/demo-task", nil))
	return rec.Body.String()
}

type taskLookupStatusEnvelope struct {
	status   persistence.TaskStatus
	envelope []byte
}

func (l taskLookupStatusEnvelope) Get(_ context.Context, taskID string) (*persistence.Task, error) {
	if taskID != "demo-task" {
		return nil, persistence.ErrNotFound // the miss contract of TaskRepository.Get
	}
	return &persistence.Task{ID: taskID, ProjectID: "demo", WorkflowID: researchWorkflow(), Status: l.status, ResultEnvelope: l.envelope}, nil
}

// The double states production's miss: (nil, ErrNotFound) for an absent task.
func TestTaskLookupStatusEnvelope_MissContract_614(t *testing.T) {
	repotest.AssertMiss(t, "TaskRepository.Get", func() (*persistence.Task, error) {
		return taskLookupStatusEnvelope{}.Get(context.Background(), "absent-id")
	})
}

func outputs(names ...string) []*persistence.Artifact {
	out := make([]*persistence.Artifact, 0, len(names))
	for i, n := range names {
		out = append(out, &persistence.Artifact{ID: "a" + string(rune('1'+i)), Name: n, ArtifactClass: persistence.ArtifactClassOutput})
	}
	return out
}

func TestPolicedAnswer_CompletedSendsOnlyTheListedKeys_614(t *testing.T) {
	body := policedStream(t, persistence.TaskStatusCompleted, answerPolicy(),
		outputs("result-20261104-ab12.json", "notes.md"),
		map[string]string{
			"a1": `{"answer":"the brief says so","citations":["docs/architect/a.md"],"debug":"SENTINEL-extra-key"}`,
			"a2": "SENTINEL-second-artifact",
		}, nil)
	if n := strings.Count(body, "event: artifact"); n != 1 {
		t.Fatalf("want one artifact frame, got %d:\n%s", n, body)
	}
	if !strings.Contains(body, "the brief says so") {
		t.Fatalf("the answer is missing:\n%s", body)
	}
	for _, s := range []string{"SENTINEL-extra-key", "SENTINEL-second-artifact"} {
		if strings.Contains(body, s) {
			t.Fatalf("%s reached the caller:\n%s", s, body)
		}
	}
}

func TestPolicedAnswer_NothingLeavesARunThatDidNotComplete_614(t *testing.T) {
	for _, st := range []persistence.TaskStatus{persistence.TaskStatusFailed, persistence.TaskStatusCancelled} {
		t.Run(string(st), func(t *testing.T) {
			body := policedStream(t, st, answerPolicy(), outputs("result.json"),
				map[string]string{"a1": `{"answer":"SENTINEL-partial"}`}, nil)
			if strings.Contains(body, "event: artifact") || strings.Contains(body, "SENTINEL-partial") {
				t.Fatalf("a %s run sent its partial answer:\n%s", st, body)
			}
			if !strings.Contains(body, `"final":true`) {
				t.Fatalf("the caller still gets the terminal state:\n%s", body)
			}
		})
	}
}

func TestPolicedAnswer_RefusalsSendNoArtifact_614(t *testing.T) {
	huge := `{"answer":"` + strings.Repeat("x", registry.A2AAnswerReadBound) + `"}`
	for _, tc := range []struct {
		name     string
		arts     []*persistence.Artifact
		content  map[string]string
		envelope []byte
	}{
		{"raw artifact over 64 KiB", outputs("result.json"), map[string]string{"a1": huge}, nil},
		{"fenced code", outputs("result.json"), map[string]string{"a1": "{\"answer\":\"```SENTINEL```\"}"}, nil},
		{"named artifact missing; the envelope is not a fallback", outputs("other.json"), map[string]string{"a1": `{"answer":"SENTINEL-other"}`}, []byte(`{"answer":"SENTINEL-envelope"}`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := policedStream(t, persistence.TaskStatusCompleted, answerPolicy(), tc.arts, tc.content, tc.envelope)
			if strings.Contains(body, "event: artifact") || strings.Contains(body, "SENTINEL") {
				t.Fatalf("a refused answer reached the caller:\n%s", body)
			}
		})
	}
}

// Without a policy the behaviour of §3.1 is unchanged.
func TestUnpolicedAnswer_Unchanged_614(t *testing.T) {
	body := policedStream(t, persistence.TaskStatusCompleted, nil, outputs("result.json"),
		map[string]string{"a1": `{"answer":"a","debug":"kept"}`}, nil)
	if !strings.Contains(body, `"debug":"kept"`) {
		t.Fatalf("an unpoliced workflow must still pass result.json through:\n%s", body)
	}
}

// Check 5 at the stream: a citation naming a private path fails the answer,
// whatever the answer text says. The prefixes come from the role that runs
// the workflow's agent step in the agent's project.
func TestPolicedAnswer_PrivateCitationIsRefused_614(t *testing.T) {
	body := policedStream(t, persistence.TaskStatusCompleted, answerPolicy(), outputs("result.json"),
		map[string]string{"a1": `{"answer":"clean text","citations":["https://docs.vornik.io"]}`}, nil)
	if strings.Contains(body, "event: artifact") || strings.Contains(body, "SENTINEL") {
		t.Fatalf("a private citation reached the caller:\n%s", body)
	}
	ok := policedStream(t, persistence.TaskStatusCompleted, answerPolicy(), outputs("result.json"),
		map[string]string{"a1": `{"answer":"clean text","citations":["docs/architect/memory.md"]}`}, nil)
	if !strings.Contains(ok, "docs/architect/memory.md") {
		t.Fatalf("an allowed citation must pass:\n%s", ok)
	}
}

// With no prefixes resolvable (the role has none), any citation fails
// closed; an answer without citations still passes.
func TestProjectAnswer_NoPrefixesRefusesAnyCitation_614(t *testing.T) {
	if _, reason := projectAnswer([]byte(`{"answer":"a","citations":["docs/public/x.md"]}`), answerPolicy(), nil); reason == "" {
		t.Fatal("a citation with no allowed prefixes must be refused")
	}
	if _, reason := projectAnswer([]byte(`{"answer":"a","citations":[]}`), answerPolicy(), nil); reason != "" {
		t.Fatalf("empty citations must pass: %s", reason)
	}
}
