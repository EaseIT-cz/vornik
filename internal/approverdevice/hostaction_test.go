package approverdevice

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// Hermes approval transport design
// (https://docs.vornik.io):
// the host_action kind, filed for a Hermes approval request and answered on
// the paired phone with a scope.

// secretCanary is GitHub-token shaped: Vornik's strong patterns mask it.
const secretCanary = "ghp_HOSTACTIONCANARY0123456789abcdefghij"

func hermesRequest(id string) HostActionRequest {
	return HostActionRequest{
		RequestID: id, Digest: strings.Repeat("a", 64),
		Command: "rm -rf /tmp/build && touch /tmp/marker", Description: "recursive delete",
		PatternKey: "rm_recursive", PatternKeys: []string{"rm_recursive"}, Surface: "cli",
		TimeoutSeconds: 300, AllowedChoices: []string{"once", "session", "always", "deny"},
	}
}

type outcomes struct {
	mu  sync.Mutex
	got []string
}

func (o *outcomes) record(harness, outcome string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.got = append(o.got, harness+":"+outcome)
}

func (o *outcomes) list() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.got...)
}

func hostFixture(t *testing.T) (*fixture, *outcomes) {
	t.Helper()
	o := &outcomes{}
	return newFixture(t, WithHostActionRecorder(o.record)), o
}

// §4.1: the id is derived from the namespace and Hermes's request id.
func TestHostAction_IDIsDerivedFromNamespaceAndRequest(t *testing.T) {
	a, b := HostActionID("hermes", "aa01"), HostActionID("other", "aa01")
	if !strings.HasPrefix(a, "apr_") || len(a) != len("apr_")+16 || a == b || a != HostActionID("hermes", "aa01") {
		t.Fatalf("ids %q %q", a, b)
	}
}

// §4.1 filing invariants: a refile with the same Hermes digest answers the
// row's state and writes nothing; one with another digest is a conflict
// and the page keeps the first text.
func TestHostAction_FilingIsIdempotentOnTheHermesDigest(t *testing.T) {
	f, o := hostFixture(t)
	ctx := context.Background()
	st, err := f.svc.FileHostAction(ctx, "hermes", hermesRequest("aa01"))
	if err != nil || st.Status != persistence.ApprovalPending {
		t.Fatalf("first filing: %+v %v", st, err)
	}
	id := HostActionID("hermes", "aa01")
	first, _ := f.repo.GetRequest(ctx, id)
	f.advance(10 * time.Second)
	again, err := f.svc.FileHostAction(ctx, "hermes", hermesRequest("aa01"))
	if err != nil || again.Status != persistence.ApprovalPending || !again.Deadline.Equal(st.Deadline) {
		t.Fatalf("refile: %+v %v (first deadline %v)", again, err, st.Deadline)
	}
	if r, _ := f.repo.GetRequest(ctx, id); r.RenderedSHA256 != first.RenderedSHA256 || !r.CreatedAt.Equal(first.CreatedAt) {
		t.Fatal("a refile rewrote the row")
	}
	other := hermesRequest("aa01")
	other.Digest = strings.Repeat("b", 64)
	other.Command = "rm -rf / --no-preserve-root"
	if _, err := f.svc.FileHostAction(ctx, "hermes", other); !errors.Is(err, ErrHostActionConflict) {
		t.Fatalf("another digest: %v, want ErrHostActionConflict", err)
	}
	if r, _ := f.repo.GetRequest(ctx, id); r.RenderedSHA256 != first.RenderedSHA256 || strings.Contains(string(r.Rendered), "no-preserve-root") {
		t.Fatal("a conflicting refile changed the page's text")
	}
	if got := strings.Join(o.list(), ","); got != "hermes:filed,hermes:conflict" {
		t.Fatalf("outcomes = %s", got)
	}
	if len(f.pushes) != 1 {
		t.Fatalf("pushes = %d, want one per request", len(f.pushes))
	}
}

// §8: a refile of an approved row returns its choice and leaves decided_*
// untouched; a refile of an expired row returns expired and never resets it.
func TestHostAction_RefileAfterDecisionOrExpiry(t *testing.T) {
	f, _ := hostFixture(t)
	ctx := context.Background()
	d, _ := f.firstDevice(t)
	if _, err := f.svc.FileHostAction(ctx, "hermes", hermesRequest("aa01")); err != nil {
		t.Fatal(err)
	}
	id := HostActionID("hermes", "aa01")
	r, _ := f.repo.GetRequest(ctx, id)
	if err := f.svc.DecideChoice(ctx, d, id, r.RenderedSHA256, "session"); err != nil {
		t.Fatal(err)
	}
	decided, _ := f.repo.GetRequest(ctx, id)
	f.advance(time.Second)
	st, err := f.svc.FileHostAction(ctx, "hermes", hermesRequest("aa01"))
	if err != nil || st.Status != persistence.ApprovalApproved || st.Choice != "session" {
		t.Fatalf("refile of an approved row: %+v %v", st, err)
	}
	if after, _ := f.repo.GetRequest(ctx, id); !after.DecidedAt.Equal(*decided.DecidedAt) || after.DecidedByDevice != d.ID || after.DecidedChoice != "session" {
		t.Fatalf("a refile touched decided_*: %+v", after)
	}

	if _, err := f.svc.FileHostAction(ctx, "hermes", hermesRequest("aa02")); err != nil {
		t.Fatal(err)
	}
	f.advance(10 * time.Minute)
	if err := f.svc.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	st, err = f.svc.FileHostAction(ctx, "hermes", hermesRequest("aa02"))
	if err != nil || st.Status != persistence.ApprovalExpired {
		t.Fatalf("refile of an expired row: %+v %v", st, err)
	}
	if r, _ := f.repo.GetRequest(ctx, HostActionID("hermes", "aa02")); r.Status != persistence.ApprovalExpired {
		t.Fatalf("an expired row was reset to %s", r.Status)
	}
}

// §4.1: expires_at = filing + timeout - 2 s; above 24 h the ceiling, and the
// page says Vornik's own deadline, never Hermes's.
func TestHostAction_ExpiryMarginAndCeiling(t *testing.T) {
	f, _ := hostFixture(t)
	ctx := context.Background()
	st, err := f.svc.FileHostAction(ctx, "hermes", hermesRequest("aa01"))
	if err != nil {
		t.Fatal(err)
	}
	if want := f.clock().Add(298 * time.Second); !st.Deadline.Equal(want) {
		t.Fatalf("deadline %v, want filing + 300 s - 2 s = %v", st.Deadline, want)
	}
	long := hermesRequest("aa02")
	long.TimeoutSeconds = 3 * 24 * 3600
	st, err = f.svc.FileHostAction(ctx, "hermes", long)
	if err != nil {
		t.Fatal(err)
	}
	if want := f.clock().Add(24 * time.Hour); !st.Deadline.Equal(want) {
		t.Fatalf("deadline %v, want the 24 h ceiling %v", st.Deadline, want)
	}
}

// §4.1: the sentence is a template around Hermes's rule text; the command is
// never in the sentence or the push. §3: Vornik's masking is applied on top
// of Hermes's, and the command and description are cut with a marker.
func TestHostAction_RenderedSentenceAndPush(t *testing.T) {
	f, _ := hostFixture(t)
	ctx := context.Background()
	req := hermesRequest("aa01")
	req.Command = "curl -H 'Authorization: token " + secretCanary + "' https://x.example && rm -rf /tmp/a " + strings.Repeat("x", 5000)
	req.Description = "recursive delete " + strings.Repeat("d", 600)
	if _, err := f.svc.FileHostAction(ctx, "hermes", req); err != nil {
		t.Fatal(err)
	}
	r, _ := f.repo.GetRequest(ctx, HostActionID("hermes", "aa01"))
	var doc map[string]any
	if err := json.Unmarshal(r.Rendered, &doc); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"kind", "harness", "request_id", "digest", "command", "description", "pattern_key", "surface", "allowed_choices", "deadline"} {
		if _, ok := doc[k]; !ok {
			t.Errorf("rendered lacks %q", k)
		}
	}
	if doc["kind"] != "host_action" || doc["harness"] != "hermes" {
		t.Fatalf("rendered %s", r.Rendered)
	}
	cmd, _ := doc["command"].(string)
	if strings.Contains(string(r.Rendered), secretCanary) || !strings.Contains(cmd, "[REDACTED:") {
		t.Fatalf("the secret is not masked in rendered: %.300s", cmd)
	}
	if len(cmd) > MaxHostCommandBytes+len(cutMarker(1000000)) || !strings.Contains(cmd, "cut by Vornik") {
		t.Fatalf("command not cut at 4 KiB with a marker: %d bytes", len(cmd))
	}
	desc, _ := doc["description"].(string)
	if len([]rune(desc)) > MaxHostDescriptionRunes+len([]rune(cutMarker(1000000))) || !strings.Contains(desc, "cut by Vornik") {
		t.Fatalf("description not cut at 512 characters: %d", len([]rune(desc)))
	}
	if !strings.HasPrefix(r.Sentence, "Hermes (hermes) wants to run a command its safety rules flagged: recursive delete") {
		t.Fatalf("sentence %q", r.Sentence)
	}
	for _, s := range []string{r.Sentence, f.pushes[0].subject, f.pushes[0].body} {
		if strings.Contains(s, "curl") || strings.Contains(s, "rm -rf") || strings.Contains(s, secretCanary) {
			t.Fatalf("command text reached the sentence or the push: %q", s)
		}
	}
	if !strings.HasPrefix(f.pushes[0].subject, "Vornik: Hermes is waiting for you (until ") ||
		!strings.Contains(f.pushes[0].body, "https://vornik.example/ui/approve/"+r.ID) {
		t.Fatalf("push = %+v", f.pushes[0])
	}
}

// §4.1: a request that is not Hermes's shape is refused before anything is
// written.
func TestHostAction_InvalidRequestsAreRefused(t *testing.T) {
	f, _ := hostFixture(t)
	for name, mut := range map[string]func(*HostActionRequest){
		"no request id":        func(r *HostActionRequest) { r.RequestID = "" },
		"request id not hex":   func(r *HostActionRequest) { r.RequestID = "../x" },
		"digest not sha256":    func(r *HostActionRequest) { r.Digest = "abc" },
		"surface":              func(r *HostActionRequest) { r.Surface = "phone" },
		"no once":              func(r *HostActionRequest) { r.AllowedChoices = []string{"deny"} },
		"unknown choice":       func(r *HostActionRequest) { r.AllowedChoices = []string{"once", "deny", "forever"} },
		"timeout not positive": func(r *HostActionRequest) { r.TimeoutSeconds = 0 },
		"empty command":        func(r *HostActionRequest) { r.Command = "" },
	} {
		req := hermesRequest("abc123")
		mut(&req)
		if _, err := f.svc.FileHostAction(context.Background(), "hermes", req); !errors.Is(err, ErrHostActionInvalid) {
			t.Errorf("%s: %v, want ErrHostActionInvalid", name, err)
		}
	}
	if p, _ := f.repo.ListPending(context.Background(), f.clock()); len(p) != 0 {
		t.Fatalf("an invalid request was written: %d", len(p))
	}
}

// §4.4: at most 3 pending per namespace and 30 an hour; over either the
// filing is refused as busy and counted as refused_cap.
func TestHostAction_Bounds(t *testing.T) {
	f, o := hostFixture(t)
	ctx := context.Background()
	for _, id := range []string{"a1", "a2", "a3"} {
		if _, err := f.svc.FileHostAction(ctx, "hermes", hermesRequest(id)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.svc.FileHostAction(ctx, "hermes", hermesRequest("a4")); !errors.Is(err, ErrHostActionBusy) {
		t.Fatalf("a fourth pending: %v, want ErrHostActionBusy", err)
	}
	if _, err := f.svc.FileHostAction(ctx, "codex", hermesRequest("a4")); err != nil {
		t.Fatalf("another namespace was refused: %v", err)
	}
	if got := o.list(); got[len(got)-2] != "hermes:refused_cap" {
		t.Fatalf("outcomes = %v", got)
	}
	// 30 an hour: expire the pending ones and keep filing.
	n := 3
	for i := 0; n < MaxHostActionsPerHour; i++ {
		f.advance(5 * time.Minute)
		if err := f.svc.Tick(ctx); err != nil {
			t.Fatal(err)
		}
		if f.clock().Sub(time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)) >= time.Hour {
			t.Fatal("the test ran past the hour")
		}
		for j := 0; j < 3 && n < MaxHostActionsPerHour; j++ {
			if _, err := f.svc.FileHostAction(ctx, "hermes", hermesRequest(strings.Repeat("b", 1+i)+string(rune('0'+j)))); err != nil {
				t.Fatalf("filing %d: %v", n+1, err)
			}
			n++
		}
	}
	f.advance(5 * time.Minute)
	if err := f.svc.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.FileHostAction(ctx, "hermes", hermesRequest("c1")); !errors.Is(err, ErrHostActionBusy) {
		t.Fatalf("the 31st in the hour: %v, want ErrHostActionBusy", err)
	}
}

// §4.3: the state is answered only for the key's own namespace.
func TestHostAction_StateIsNamespaceBound(t *testing.T) {
	f, _ := hostFixture(t)
	ctx := context.Background()
	if _, err := f.svc.FileHostAction(ctx, "hermes", hermesRequest("aa01")); err != nil {
		t.Fatal(err)
	}
	if st, err := f.svc.HostActionState(ctx, "hermes", "aa01"); err != nil || st.Status != persistence.ApprovalPending {
		t.Fatalf("own namespace: %+v %v", st, err)
	}
	if _, err := f.svc.HostActionState(ctx, "other", "aa01"); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("another namespace: %v, want ErrNotFound", err)
	}
	if _, err := f.svc.HostActionState(ctx, "hermes", "missing"); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("a missing request: %v, want ErrNotFound", err)
	}
	// A pending row past its deadline reads expired before the tick runs.
	f.advance(299 * time.Second)
	if st, _ := f.svc.HostActionState(ctx, "hermes", "aa01"); st.Status != persistence.ApprovalExpired {
		t.Fatalf("past the deadline: %s, want expired", st.Status)
	}
}

// §4.2 and §6: DecideChoice refuses always, a choice not in
// allowed_choices, a stale sha and a decision after expires_at; Decide
// refuses a host_action row (it would lose the scope).
func TestHostAction_DecideChoiceRefusals(t *testing.T) {
	f, o := hostFixture(t)
	ctx := context.Background()
	d, _ := f.firstDevice(t)
	noSession := hermesRequest("aa01")
	noSession.AllowedChoices = []string{"once", "deny"}
	if _, err := f.svc.FileHostAction(ctx, "hermes", noSession); err != nil {
		t.Fatal(err)
	}
	id := HostActionID("hermes", "aa01")
	r, _ := f.repo.GetRequest(ctx, id)
	for _, c := range []string{"always", "session", "approve", ""} {
		if err := f.svc.DecideChoice(ctx, d, id, r.RenderedSHA256, c); !errors.Is(err, ErrBadChoice) {
			t.Errorf("choice %q: %v, want ErrBadChoice", c, err)
		}
	}
	if err := f.svc.DecideChoice(ctx, d, id, "stale", "once"); !errors.Is(err, ErrNotDecidable) {
		t.Fatalf("stale sha: %v", err)
	}
	if err := f.svc.Decide(ctx, d, id, r.RenderedSHA256, true); !errors.Is(err, ErrChoiceRequired) {
		t.Fatalf("Decide on host_action: %v, want ErrChoiceRequired", err)
	}
	if err := f.svc.DecideChoice(ctx, nil, id, r.RenderedSHA256, "once"); !errors.Is(err, ErrNoDevice) {
		t.Fatalf("no device: %v", err)
	}
	f.advance(298 * time.Second)
	if err := f.svc.DecideChoice(ctx, d, id, r.RenderedSHA256, "once"); !errors.Is(err, ErrNotDecidable) {
		t.Fatalf("after expires_at: %v", err)
	}
	if got, _ := f.repo.GetRequest(ctx, id); got.Status != persistence.ApprovalPending || got.DecidedChoice != "" {
		t.Fatalf("a refused decision wrote: %+v", got)
	}
	for _, out := range o.list() {
		if out != "hermes:filed" {
			t.Fatalf("a refused decision was counted: %v", o.list())
		}
	}
}

// §4.2: once and session approve, deny rejects; the effect is a registered
// no-op and MarkApplied follows at once (§4.1). §8: a device deciding
// namespace A's request leaves namespace B's untouched.
func TestHostAction_DecideChoiceRecordsTheScope(t *testing.T) {
	f, o := hostFixture(t)
	ctx := context.Background()
	d, _ := f.firstDevice(t)
	for _, ns := range []string{"hermes", "other"} {
		for _, rid := range []string{"aa01", "aa02", "aa03"} {
			if _, err := f.svc.FileHostAction(ctx, ns, hermesRequest(rid)); err != nil {
				t.Fatal(err)
			}
		}
	}
	want := map[string]string{"aa01": "once", "aa02": "session", "aa03": "deny"}
	for rid, choice := range want {
		id := HostActionID("hermes", rid)
		r, _ := f.repo.GetRequest(ctx, id)
		if err := f.svc.DecideChoice(ctx, d, id, r.RenderedSHA256, choice); err != nil {
			t.Fatalf("%s: %v", choice, err)
		}
		got, _ := f.repo.GetRequest(ctx, id)
		wantStatus := persistence.ApprovalApproved
		if choice == "deny" {
			wantStatus = persistence.ApprovalRejected
		}
		if got.Status != wantStatus || got.DecidedChoice != choice || got.DecidedByDevice != d.ID {
			t.Fatalf("%s: %+v", choice, got)
		}
		if choice != "deny" && got.AppliedAt == nil {
			t.Fatalf("%s: the no-op effect did not mark the request applied", choice)
		}
		st, _ := f.svc.HostActionState(ctx, "hermes", rid)
		if st.Choice != choice {
			t.Fatalf("state choice %q, want %q", st.Choice, choice)
		}
	}
	for _, rid := range []string{"aa01", "aa02", "aa03"} {
		if st, _ := f.svc.HostActionState(ctx, "other", rid); st.Status != persistence.ApprovalPending {
			t.Fatalf("namespace other's %s was decided: %+v", rid, st)
		}
	}
	got := strings.Join(o.list(), ",")
	for _, c := range []string{"hermes:once", "hermes:session", "hermes:deny"} {
		if !strings.Contains(got, c) {
			t.Fatalf("outcomes %s lack %s", got, c)
		}
	}
}

// §8 observable: the expiry tick counts each host_action it expires, once.
func TestHostAction_ExpiryIsCounted(t *testing.T) {
	f, o := hostFixture(t)
	ctx := context.Background()
	if _, err := f.svc.FileHostAction(ctx, "hermes", hermesRequest("aa01")); err != nil {
		t.Fatal(err)
	}
	f.advance(5 * time.Minute)
	for i := 0; i < 2; i++ {
		if err := f.svc.Tick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if got := strings.Join(o.list(), ","); got != "hermes:filed,hermes:expired" {
		t.Fatalf("outcomes = %s", got)
	}
}
