package approverdevice

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

// GitHub #302 (2026-10-13): an interrupted config apply is resolved on the
// host only. The device gains a host-level card answered with one of the
// choices its kind registers (keep or restore), through the same guarded
// path host_action uses (config-apply journal design §13.3).

func journalChoices(r persistence.AgentApprovalRequestRow) []Choice {
	out := []Choice{{Value: "keep", Label: "Keep the files as they are"}}
	if !strings.Contains(string(r.Rendered), `"applied":true`) {
		out = append(out, Choice{Value: "restore", Label: "Restore the previous version"})
	}
	return out
}

type choiceFixture struct {
	*fixture
	d       *Device
	decided []string
}

func newChoiceFixture(t *testing.T) *choiceFixture {
	f := &choiceFixture{fixture: newFixture(t)}
	f.d, _ = f.firstDevice(t)
	f.svc.RegisterChoices(persistence.ApprovalKindConfigJournalDrift, journalChoices)
	f.svc.RegisterEffect(persistence.ApprovalKindConfigJournalDrift, func(_ context.Context, r persistence.AgentApprovalRequestRow) error {
		f.decided = append(f.decided, r.ID+":"+r.DecidedChoice)
		return nil
	})
	return f
}

func (f *choiceFixture) file(t *testing.T, id, rendered string) {
	t.Helper()
	if err := f.repo.CreateRequest(context.Background(), persistence.AgentApprovalRequestRow{
		ID: id, Namespace: "", Kind: persistence.ApprovalKindConfigJournalDrift,
		Sentence: "A config change for hermes--global-news was interrupted.", Rendered: []byte(rendered), RenderedSHA256: "h-" + id,
		Status: persistence.ApprovalPending, CreatedAt: f.clock(), ExpiresAt: f.clock().Add(RequestTTL),
	}); err != nil {
		t.Fatal(err)
	}
}

func TestChoiceKind_DecideRunsTheEffectWithTheChoice(t *testing.T) {
	f := newChoiceFixture(t)
	ctx := context.Background()
	f.file(t, "apr_c1", `{"journal_id":"caj_1"}`)
	if err := f.svc.DecideChoice(ctx, f.d, "apr_c1", "h-apr_c1", "restore"); err != nil {
		t.Fatal(err)
	}
	if len(f.decided) != 1 || f.decided[0] != "apr_c1:restore" {
		t.Fatalf("effect saw %v, want apr_c1:restore", f.decided)
	}
	r, _ := f.repo.GetRequest(ctx, "apr_c1")
	if r.Status != persistence.ApprovalApproved || r.DecidedChoice != "restore" || r.AppliedAt == nil {
		t.Errorf("row = %+v", r)
	}
}

// #334 audit 2026-10-14: Decide refused only host_action, so a plain
// approve on a config_journal_drift card applied its effect with an empty
// choice (neither keep nor restore). Every choice kind is answered through
// DecideChoice; Decide refuses it either way and leaves the row pending.
func TestChoiceKind_PlainDecideIsRefused(t *testing.T) {
	f := newChoiceFixture(t)
	ctx := context.Background()
	f.file(t, "apr_c0", `{"journal_id":"caj_0"}`)
	for _, approve := range []bool{true, false} {
		if err := f.svc.Decide(ctx, f.d, "apr_c0", "h-apr_c0", approve); !errors.Is(err, ErrChoiceRequired) {
			t.Errorf("Decide(approve=%v) on a choice kind = %v, want ErrChoiceRequired", approve, err)
		}
	}
	if len(f.decided) != 0 {
		t.Errorf("an effect ran without a choice: %v", f.decided)
	}
	if r, _ := f.repo.GetRequest(ctx, "apr_c0"); r == nil || r.Status != persistence.ApprovalPending {
		t.Errorf("row must stay pending: %+v", r)
	}
}

func TestChoiceKind_RefusesAChoiceNotOffered(t *testing.T) {
	f := newChoiceFixture(t)
	ctx := context.Background()
	f.file(t, "apr_c2", `{"journal_id":"caj_2","applied":true}`)
	for _, c := range []string{"restore", "deny", "approve", "once"} {
		if err := f.svc.DecideChoice(ctx, f.d, "apr_c2", "h-apr_c2", c); !errors.Is(err, ErrBadChoice) {
			t.Errorf("choice %q = %v, want ErrBadChoice", c, err)
		}
	}
	if len(f.decided) != 0 {
		t.Errorf("an effect ran: %v", f.decided)
	}
	// A kind with no registered choices is still refused.
	if err := f.repo.CreateRequest(ctx, persistence.AgentApprovalRequestRow{
		ID: "apr_w", Namespace: "hermes", Kind: persistence.ApprovalKindWideningChange, Sentence: "s", Rendered: []byte(`{}`),
		RenderedSHA256: "h-w", Status: persistence.ApprovalPending, CreatedAt: f.clock(), ExpiresAt: f.clock().Add(RequestTTL),
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.DecideChoice(ctx, f.d, "apr_w", "h-w", "keep"); !errors.Is(err, ErrBadChoice) {
		t.Errorf("widening_change with a choice = %v, want ErrBadChoice", err)
	}
}

func TestChoiceKind_PageOffersTheChoicesAndPostsOne(t *testing.T) {
	// The phone is the first device here (no fixture device), as in the
	// host-action page tests.
	f := &choiceFixture{fixture: newFixture(t)}
	f.svc.RegisterChoices(persistence.ApprovalKindConfigJournalDrift, journalChoices)
	f.svc.RegisterEffect(persistence.ApprovalKindConfigJournalDrift, func(_ context.Context, r persistence.AgentApprovalRequestRow) error {
		f.decided = append(f.decided, r.ID+":"+r.DecidedChoice)
		return nil
	})
	srv := serve(t, f.fixture)
	phone := newBrowser(t, srv)
	code, _, _ := f.svc.StartPairing(context.Background(), "Pixel")
	phone.pairWith(code)
	f.file(t, "apr_c3", `{"journal_id":"caj_3"}`)
	_, page := phone.do(http.MethodGet, "/ui/approve/apr_c3", nil, nil)
	for _, want := range []string{`value="keep"`, "Keep the files as they are", `value="restore"`, "Restore the previous version", "A config change for hermes--global-news was interrupted."} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	for _, never := range []string{`value="approve"`, `value="reject"`, `value="deny"`} {
		if strings.Contains(page, never) {
			t.Errorf("page has %q", never)
		}
	}
	res, _ := phone.do(http.MethodPost, "/ui/approve/apr_c3", url.Values{"decision": {"keep"}, "rendered_sha256": {"h-apr_c3"}}, nil)
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("post = %d", res.StatusCode)
	}
	if len(f.decided) != 1 || f.decided[0] != "apr_c3:keep" {
		t.Fatalf("effect saw %v", f.decided)
	}
	res, _ = phone.do(http.MethodPost, "/ui/approve/apr_c3", url.Values{"decision": {"restore"}, "rendered_sha256": {"h-apr_c3"}}, nil)
	if res.StatusCode == http.StatusSeeOther {
		t.Error("a second decision was accepted")
	}
}

// The daemon's own card is pushed with its own subject, the sentence and
// the link only.
func TestFileRequestPush_StoresAndPushesTheSubject(t *testing.T) {
	f := newFixture(t)
	r := persistence.AgentApprovalRequestRow{ID: "apr_p", Kind: persistence.ApprovalKindConfigJournalDrift,
		Sentence: "A config change for alpha was interrupted.", Rendered: []byte(`{}`), RenderedSHA256: "h",
		Status: persistence.ApprovalPending, CreatedAt: f.clock(), ExpiresAt: f.clock().Add(RequestTTL)}
	if err := f.svc.FileRequestPush(context.Background(), r, "Vornik: a config change was interrupted"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.repo.GetRequest(context.Background(), "apr_p"); err != nil {
		t.Fatal(err)
	}
	if len(f.pushes) != 1 || f.pushes[0].subject != "Vornik: a config change was interrupted" ||
		!strings.Contains(f.pushes[0].body, "/ui/approve/apr_p") || !strings.Contains(f.pushes[0].body, r.Sentence) {
		t.Fatalf("pushes = %+v", f.pushes)
	}
	if err := f.svc.FileRequestPush(context.Background(), r, "x"); err == nil {
		t.Error("a duplicate id must be refused")
	}
}
