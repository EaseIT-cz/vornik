package narrator

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/aidisclosure"
	"vornik.io/vornik/internal/conversation"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/repotest"
)

// newDegradedNarrator builds a Run-loop-free Narrator (tests call emitLine
// directly, as persist_publish_test.go does) with live metrics and a log
// buffer the test can read.
func newDegradedNarrator(t *testing.T) (*Narrator, *bytes.Buffer) {
	t.Helper()
	buf := &bytes.Buffer{}
	n := &Narrator{
		Sub:        newFakeSub(),
		Pub:        newFakePub(nil),
		Store:      newFakeStore(nil),
		Executions: newFakeExecutions(),
		Logger:     zerolog.New(buf),
		Metrics:    NewMetrics(prometheus.NewRegistry()),
	}
	n.ensureInit()
	return n, buf
}

// TestEmitLine_EveryDegradedLineIsCountedByItsCause — GitHub #369,
// outcome-quality LLD S8.11 row 11a. lines_total{degraded="true"} folded six
// causes together with no log and no reason; each now counts
// vornik_narration_degraded_total{reason}, and the llm_* causes also Warn.
func TestEmitLine_EveryDegradedLineIsCountedByItsCause(t *testing.T) {
	cases := []struct {
		name      string
		configure func(n *Narrator, st *executionState)
		reason    string
		warn      bool
	}{
		{"no_client", func(_ *Narrator, _ *executionState) {}, "no_client", false},
		{"llm_timeout", func(n *Narrator, _ *executionState) {
			n.Client = &fakeProvider{err: context.DeadlineExceeded}
		}, "llm_timeout", true},
		{"llm_cancelled", func(n *Narrator, _ *executionState) {
			n.Client = &fakeProvider{err: context.Canceled}
		}, "llm_cancelled", true},
		{"llm_error", func(n *Narrator, _ *executionState) {
			n.Client = &fakeProvider{err: errors.New("upstream 502")}
		}, "llm_error", true},
		{"no_choices", func(n *Narrator, _ *executionState) {
			n.Client = &fakeProvider{} // no replies → a response with no choices
		}, "no_choices", false},
		{"empty", func(n *Narrator, _ *executionState) {
			n.Client = &fakeProvider{replies: []string{"   "}}
		}, "empty", false},
		{"cost_capped", func(n *Narrator, st *executionState) {
			n.Client = &fakeProvider{replies: []string{"never asked"}}
			st.costCapped = true
		}, "cost_capped", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n, buf := newDegradedNarrator(t)
			st := newExecutionState("exec1", "p1", "t1", time.Now())
			n.states["exec1"] = st
			tc.configure(n, st)

			n.emitLine(context.Background(), "exec1", st, triggerStepStarted,
				templateInput{Role: "worker", StepIdx: 1}, "s1", "", persistence.ExecutionNarrationKindStep)

			if got := counterVecValue(t, n.Metrics.DegradedTotal, tc.reason); got != 1 {
				t.Fatalf("degraded_total{reason=%s} = %v, want 1", tc.reason, got)
			}
			// lines_total keeps its labels and still counts the line degraded.
			if got := counterVecValue(t, n.Metrics.LinesTotal, persistence.ExecutionNarrationKindStep, "true"); got != 1 {
				t.Fatalf("lines_total{kind=step,degraded=true} = %v, want 1", got)
			}
			warned := strings.Contains(buf.String(), `"level":"warn"`)
			if warned != tc.warn {
				t.Fatalf("warn logged = %v, want %v; log: %s", warned, tc.warn, buf.String())
			}
		})
	}
}

// TestEmitLine_LineCappedDropIsCounted — GitHub #369, outcome-quality LLD
// S8.11 row 11b. After the line cap every later line was dropped with no
// count; each now counts dropped_events_total{kind="line_capped"}.
func TestEmitLine_LineCappedDropIsCounted(t *testing.T) {
	n, _ := newDegradedNarrator(t)
	st := newExecutionState("exec1", "p1", "t1", time.Now())
	n.states["exec1"] = st
	st.lineCapped = true
	for i := 0; i < 3; i++ {
		n.emitLine(context.Background(), "exec1", st, triggerStepStarted,
			templateInput{Role: "worker", StepIdx: 1}, "s1", "", persistence.ExecutionNarrationKindStep)
	}
	if got := counterVecValue(t, n.Metrics.DroppedEventsTotal, "line_capped"); got != 3 {
		t.Fatalf("dropped_events_total{kind=line_capped} = %v, want 3", got)
	}
}

// erroringTaskGetterCP fails the lookup of the one task the chat-push case
// uses ("t1"); any other id is a miss, as the production repository answers.
type erroringTaskGetterCP struct{ err error }

func (f erroringTaskGetterCP) Get(_ context.Context, id string) (*persistence.Task, error) {
	if id == "t1" {
		return nil, f.err
	}
	return nil, persistence.ErrNotFound
}

func TestErroringTaskGetterCP_HonoursTheMissContract(t *testing.T) {
	repotest.AssertMissRepo(t, "TaskRepository.Get", erroringTaskGetterCP{err: errors.New("db down")}.Get)
}

// TestChatPush_SkipsAndFailuresAreCounted — GitHub #369, outcome-quality LLD
// S8.11 row 11c. A task lookup error and a missing task returned silently,
// and a send failure Warned with no metric.
func TestChatPush_SkipsAndFailuresAreCounted(t *testing.T) {
	task := &persistence.Task{ID: "t1", ProjectID: "p1", ChatTurnID: strp("chat_1")}
	row := &persistence.ChatAuditEntry{ID: "chat_1", ChatID: "555", ProjectID: "p1"}

	cases := []struct {
		name      string
		configure func(n *Narrator, ch *fakeChannelCP)
		kind      string
		warn      bool
	}{
		{"lookup_error", func(n *Narrator, _ *fakeChannelCP) {
			n.Tasks = erroringTaskGetterCP{err: errors.New("db down")}
		}, "chatpush_lookup_error", true},
		{"task_missing", func(n *Narrator, _ *fakeChannelCP) {
			n.Tasks = fakeTaskGetterCP{byID: map[string]*persistence.Task{}}
		}, "chatpush_task_missing", false},
		{"send_error", func(_ *Narrator, ch *fakeChannelCP) {
			ch.sendErr = errors.New("telegram 500")
		}, "chatpush_send_error", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			buf := &bytes.Buffer{}
			ch := &fakeChannelCP{name: "telegram"}
			n := &Narrator{
				Sub:        newFakeSub(),
				Pub:        newFakePub(nil),
				Store:      newFakeStore(nil),
				Executions: newFakeExecutions(),
				Tasks:      fakeTaskGetterCP{byID: map[string]*persistence.Task{task.ID: task}},
				Audit:      fakeChatAuditCP{row: row},
				Resolver:   fakeResolverCP{byName: map[string]conversation.Channel{"telegram": ch}},
				Disclosure: aidisclosure.New(aidisclosure.Config{}, servedAllCP{}),
				ProjectSettings: func(string) ProjectNarratorSettings {
					return ProjectNarratorSettings{ChatPush: true}
				},
				Logger:  zerolog.New(buf),
				Metrics: NewMetrics(prometheus.NewRegistry()),
			}
			n.ensureInit()
			tc.configure(n, ch)
			st := newExecutionState("exec1", "p1", "t1", time.Now())
			n.states["exec1"] = st
			n.emitLine(context.Background(), "exec1", st, triggerStepCompleted,
				templateInput{Role: "worker", StepIdx: 1}, "s1", "", persistence.ExecutionNarrationKindStep)

			if got := counterVecValue(t, n.Metrics.DroppedEventsTotal, tc.kind); got != 1 {
				t.Fatalf("dropped_events_total{kind=%s} = %v, want 1", tc.kind, got)
			}
			if tc.warn && !strings.Contains(buf.String(), `"level":"warn"`) {
				t.Fatalf("want a Warn; log: %s", buf.String())
			}
		})
	}
}
