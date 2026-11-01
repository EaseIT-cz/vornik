package retention

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/datasubject"
)

// GitHub #344 slice 5d-1c (art17 redact design §15.16). Found at: review (the
// 5d design, reviews 8cfd F3 and 30f3): a transcript row a running task holds
// is deferred, and nothing told the operator before the Art 12(3) deadline.

type fakeEscSource struct {
	reqs     []datasubject.Request
	reports  map[string]string
	status   map[string]string
	raised   map[datasubject.EscalationKey]bool
	recorded int
}

func (f *fakeEscSource) ListLiveRequests(context.Context) ([]datasubject.Request, error) {
	return f.reqs, nil
}

func (f *fakeEscSource) GetRequestReport(_ context.Context, id string) (string, string, error) {
	r, ok := f.reports[id]
	if !ok {
		return "", "", errors.New("no report")
	}
	return r, "h", nil
}

func (f *fakeEscSource) EscalationRaised(_ context.Context, k datasubject.EscalationKey) (bool, error) {
	return f.raised[key(k)], nil
}

func (f *fakeEscSource) RecordEscalation(_ context.Context, k datasubject.EscalationKey, _ time.Time) error {
	f.raised[key(k)] = true
	f.recorded++
	return nil
}

func (f *fakeEscSource) TaskStatus(_ context.Context, id string) (string, error) {
	return f.status[id], nil
}

func key(k datasubject.EscalationKey) datasubject.EscalationKey {
	k.Deadline = k.Deadline.UTC()
	return k
}

type fakeEscalator struct {
	sent []string
	err  error
}

func (f *fakeEscalator) SendOperator(_ context.Context, subject, body string) error {
	if f.err != nil {
		return f.err
	}
	f.sent = append(f.sent, subject+"\n"+body)
	return nil
}

var opened = time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)

func report(t *testing.T, deferred ...datasubject.DeferredAction) string {
	t.Helper()
	b, err := json.Marshal(struct {
		Result datasubject.ErasureResult `json:"result"`
	}{datasubject.ErasureResult{Deferred: deferred}})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// liveRow is a deferral held by task-1.
func liveRow(row string) datasubject.DeferredAction {
	return datasubject.DeferredAction{Table: datasubject.TableTaskMessages, RowID: row,
		Disposition: datasubject.DispositionRedact, Reason: "running", LiveTaskID: "task-1"}
}

func newFixture(t *testing.T, req datasubject.Request, deferred ...datasubject.DeferredAction) (*fakeEscSource, *fakeEscalator, *ErasureEscalation) {
	t.Helper()
	src := &fakeEscSource{reqs: []datasubject.Request{req}, reports: map[string]string{req.ID: report(t, deferred...)},
		status: map[string]string{"task-1": "RUNNING"}, raised: map[datasubject.EscalationKey]bool{}}
	esc := &fakeEscalator{}
	return src, esc, NewErasureEscalation(src, esc, zerolog.Nop())
}

func erasure(id string) datasubject.Request {
	return datasubject.Request{ID: id, SubjectID: "s", Kind: datasubject.RequestErasure,
		State: datasubject.StateVerified, OpenedAt: opened}
}

func TestErasureEscalation_AlertsOnceInsideTheMargin(t *testing.T) {
	src, esc, p := newFixture(t, erasure("r1"), liveRow("m1"))
	early := datasubject.FirstMonthEnd(opened).Add(-datasubject.EscalationMargin - time.Hour)
	if got, _ := p.Run(context.Background(), early); got.Raised != 0 || len(esc.sent) != 0 {
		t.Fatalf("8 days out = %+v", got)
	}
	inside := datasubject.FirstMonthEnd(opened).Add(-datasubject.EscalationMargin + time.Hour)
	got, err := p.Run(context.Background(), inside)
	if err != nil || got.Raised != 1 || len(esc.sent) != 1 || src.recorded != 1 {
		t.Fatalf("inside = %+v, %v, sent %d", got, err, len(esc.sent))
	}
	for _, want := range []string{"r1", "task_messages/m1", "task-1", "vornikctl subject erase", "vornikctl subject extend", "within one month"} {
		if !strings.Contains(esc.sent[0], want) {
			t.Errorf("alert lacks %q:\n%s", want, esc.sent[0])
		}
	}
	if got, _ := p.Run(context.Background(), inside.Add(6*time.Hour)); got.Raised != 0 || len(esc.sent) != 1 {
		t.Fatalf("second tick = %+v", got)
	}
}

func TestErasureEscalation_SettledTaskClosedRequestAndUnheldRowsRaiseNothing(t *testing.T) {
	inside := datasubject.FirstMonthEnd(opened).Add(-time.Hour)
	src, esc, p := newFixture(t, erasure("r1"), liveRow("m1"),
		datasubject.DeferredAction{Table: datasubject.TableTaskMessages, RowID: "m2", Reason: "changed"})
	src.status["task-1"] = "COMPLETED"
	if got, _ := p.Run(context.Background(), inside); got.Raised != 0 || len(esc.sent) != 0 {
		t.Fatalf("settled = %+v", got)
	}
	access := erasure("r2")
	access.Kind = datasubject.RequestAccess
	src.reqs = []datasubject.Request{access}
	src.reports["r2"] = report(t, liveRow("m1"))
	src.status["task-1"] = "RUNNING"
	if got, _ := p.Run(context.Background(), inside); got.Raised != 0 {
		t.Fatalf("access request = %+v", got)
	}
}

func TestErasureEscalation_ExtensionMovesTheDeadlineAndAlertsAgain(t *testing.T) {
	req := erasure("r1")
	src, esc, p := newFixture(t, req, liveRow("m1"))
	inside := datasubject.FirstMonthEnd(opened).Add(-time.Hour)
	if got, _ := p.Run(context.Background(), inside); got.Raised != 1 {
		t.Fatalf("original = %+v", got)
	}
	if err := req.Extend("complex", opened.Add(10*24*time.Hour), inside); err != nil {
		t.Fatal(err)
	}
	src.reqs = []datasubject.Request{req}
	if got, _ := p.Run(context.Background(), inside.Add(time.Hour)); got.Raised != 0 {
		t.Fatalf("just extended = %+v", got)
	}
	nearExtended := req.Deadline().Add(-time.Hour)
	if got, _ := p.Run(context.Background(), nearExtended); got.Raised != 1 || len(esc.sent) != 2 {
		t.Fatalf("extended = %+v", got)
	}
	if strings.Contains(esc.sent[1], "extension is available") {
		t.Error("an extended request's alert must not offer the extension again")
	}

	// Extended before the original window opens: no alert at the original deadline.
	early := erasure("r3")
	if err := early.Extend("complex", opened.Add(2*24*time.Hour), opened.Add(2*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	src2, esc2, p2 := newFixture(t, early, liveRow("m1"))
	if got, _ := p2.Run(context.Background(), inside); got.Raised != 0 || len(esc2.sent) != 0 || src2.recorded != 0 {
		t.Fatalf("pre-extended = %+v", got)
	}
}

func TestErasureEscalation_FailedSendRetriesAndUnsentIsAGauge(t *testing.T) {
	src, esc, p := newFixture(t, erasure("r1"), liveRow("m1"), liveRow("m2"))
	var gauge []int
	p.SetUnsent = func(n int) { gauge = append(gauge, n) }
	esc.err = errors.New("telegram down")
	inside := datasubject.FirstMonthEnd(opened).Add(-time.Hour)
	got, err := p.Run(context.Background(), inside)
	if err != nil || got.Raised != 0 || got.Unsent != 2 || src.recorded != 0 {
		t.Fatalf("failed send = %+v, %v", got, err)
	}
	logged := got.Logged
	got, _ = p.Run(context.Background(), inside.Add(6*time.Hour))
	if got.Unsent != 2 || got.Logged != 0 || logged != 2 {
		t.Fatalf("second failing tick logged %d (first %d), want once a day per row", got.Logged, logged)
	}
	esc.err = nil
	got, _ = p.Run(context.Background(), inside.Add(12*time.Hour))
	if got.Raised != 2 || got.Unsent != 0 || src.recorded != 2 {
		t.Fatalf("recovered = %+v", got)
	}
	if len(gauge) != 3 || gauge[0] != 2 || gauge[1] != 2 || gauge[2] != 0 {
		t.Fatalf("gauge = %v", gauge)
	}
}
