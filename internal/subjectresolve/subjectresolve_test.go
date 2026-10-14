package subjectresolve

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/datasubject"
	"vornik.io/vornik/internal/persistence"
)

// GDPR LLD §4.17.9 (GitHub #345, 2026-10-13): these gates were inline in the
// console handler; the CLI had none. One implementation now serves both, so
// each gate is pinned here once.

type reqs map[string]datasubject.Request

func (r reqs) GetRequest(_ context.Context, id string) (datasubject.Request, error) {
	if q, ok := r[id]; ok {
		return q, nil
	}
	return datasubject.Request{}, persistence.ErrNotFound
}

type kg struct {
	cands   []datasubject.KGCandidate
	candErr error
	bindErr error
	state   datasubject.KGBindingState
	binds   int
}

func (k *kg) Candidates(context.Context, string, []string, []string) ([]datasubject.KGCandidate, error) {
	return k.cands, k.candErr
}

func (k *kg) Bind(_ context.Context, r datasubject.KGBindRequest) (datasubject.KGBindResult, error) {
	k.binds++
	if k.bindErr != nil {
		return datasubject.KGBindResult{State: k.state}, k.bindErr
	}
	return datasubject.KGBindResult{EntityID: r.EntityID, LinksAdded: 2, AdoptedFrom: "ds_ph", LinksMoved: 1}, nil
}

type screen struct {
	restricted map[string]bool
	err        error
}

func (s screen) RestrictedEntities(_ context.Context, _ []string) (map[string]bool, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.restricted, nil
}

func (s screen) RestrictedChunks(context.Context, []string) (map[string]bool, error) { return nil, nil }

type sink struct {
	rows []*persistence.AdminAuditEntry
	err  error
}

func (s *sink) Insert(_ context.Context, e *persistence.AdminAuditEntry) error {
	if s.err != nil {
		return s.err
	}
	s.rows = append(s.rows, e)
	return nil
}

func TestAuthorize(t *testing.T) {
	svc := &Service{Requests: reqs{
		"ok":   {ID: "ok", SubjectID: "ds", State: datasubject.StateVerified},
		"open": {ID: "open", SubjectID: "ds", State: datasubject.StateOpen},
	}}
	if r, err := svc.Authorize(context.Background(), "ok"); err != nil || r.SubjectID != "ds" {
		t.Fatalf("verified: %v %v", r, err)
	}
	if _, err := svc.Authorize(context.Background(), "nope"); !errors.Is(err, ErrRequestNotFound) {
		t.Fatalf("unknown: %v", err)
	}
	var nv *NotVerifiedError
	if _, err := svc.Authorize(context.Background(), "open"); !errors.As(err, &nv) || nv.State != datasubject.StateOpen {
		t.Fatalf("open: %v", err)
	}
}

func TestValidateNamesAndTerms(t *testing.T) {
	if got, err := ValidateNames([]string{"  Jane  ", "", "Janey"}); err != nil || strings.Join(got, "|") != "Jane|Janey" {
		t.Fatalf("got %v %v", got, err)
	}
	if _, err := ValidateNames([]string{" ab "}); err == nil {
		t.Fatal("2-rune name accepted after trimming")
	}
	if _, err := ValidateNames([]string{strings.Repeat("x", 101)}); err == nil {
		t.Fatal("101-rune name accepted")
	}
	if _, err := ValidateNames([]string{"aaa", "bbb", "ccc", "ddd", "eee", "fff"}); err == nil {
		t.Fatal("six names accepted")
	}
	if got := SearchTerms("Jane Doe", []string{"jane doe", "JD", "Janey"}); strings.Join(got, "|") != "Jane Doe|Janey" {
		t.Fatalf("SearchTerms = %v", got)
	}
	if got := TermLengths([]string{"Jané", "abc"}); got[0] != 4 || got[1] != 3 {
		t.Fatalf("TermLengths = %v", got)
	}
}

func TestRootCause(t *testing.T) {
	root := errors.New("connection reset")
	if got := RootCause(fmt.Errorf("search %q: %w", "Jane", root)); got != root {
		t.Fatalf("RootCause = %v", got)
	}
}

func TestAuditSearch_NoNamesAndNoSinkRefuses(t *testing.T) {
	req := datasubject.Request{ID: "dsr", SubjectID: "ds"}
	if err := (&Service{}).AuditSearch(context.Background(), Actor{}, req, nil, []string{"x"}); !errors.Is(err, ErrNoAudit) {
		t.Fatalf("nil sink: %v", err)
	}
	s := &sink{}
	svc := &Service{Audit: s, Now: func() time.Time { return time.Unix(0, 0) }}
	if err := svc.AuditSearch(context.Background(), Actor{Principal: "p", Source: "cli"}, req, []string{"p1"},
		[]string{"Sentinel Name"}); err != nil {
		t.Fatal(err)
	}
	row := s.rows[0]
	if row.Action != ActionSearch || row.Target != "dsr" || row.Source != "cli" || row.Principal != "p" {
		t.Fatalf("row = %+v", row)
	}
	if strings.Contains(row.After, "Sentinel") || !strings.Contains(row.After, `"term_lengths":[13]`) ||
		!strings.Contains(row.After, `"project":"p1"`) {
		t.Fatalf("After = %s", row.After)
	}
	s.err = errors.New("boom")
	if err := svc.AuditSearch(context.Background(), Actor{}, req, nil, nil); err == nil {
		t.Fatal("a failed write was swallowed")
	}
}

func TestScreenedCandidates(t *testing.T) {
	k := &kg{cands: []datasubject.KGCandidate{{Entity: datasubject.KGEntity{ID: "a"}}, {Entity: datasubject.KGEntity{ID: "b"}}}}
	svc := &Service{KG: k, Screen: screen{restricted: map[string]bool{"b": true}}}
	got, err := svc.ScreenedCandidates(context.Background(), "ds", nil, nil)
	if err != nil || len(got) != 2 || got[0].Withheld || !got[1].Withheld {
		t.Fatalf("got %+v %v", got, err)
	}
	svc.Screen = screen{err: errors.New("db")}
	if got, err := svc.ScreenedCandidates(context.Background(), "ds", nil, nil); !errors.Is(err, ErrScreen) || got != nil {
		t.Fatalf("screen error must fail closed: %v %v", got, err)
	}
	k.candErr = errors.New("read")
	if _, err := svc.ScreenedCandidates(context.Background(), "ds", nil, nil); err == nil || errors.Is(err, ErrScreen) {
		t.Fatalf("candidate error: %v", err)
	}
}

func TestBindEntity(t *testing.T) {
	req := datasubject.Request{ID: "dsr", SubjectID: "ds"}
	k := &kg{}
	svc := &Service{KG: k, Screen: screen{restricted: map[string]bool{"r": true}}}
	if out := svc.BindEntity(context.Background(), req, "r", nil, nil, false); out.OK || k.binds != 0 ||
		!strings.Contains(out.Audited, "restriction") {
		t.Fatalf("restricted: %+v binds=%d", out, k.binds)
	}
	svc.Screen = screen{err: errors.New("db")}
	if out := svc.BindEntity(context.Background(), req, "a", nil, nil, false); out.OK || k.binds != 0 {
		t.Fatalf("screen error must fail closed: %+v", out)
	}
	svc.Screen = screen{}
	if out := svc.BindEntity(context.Background(), req, "a", nil, nil, true); !out.OK ||
		out.Audited != "ok: 2 record(s) linked, 1 moved from placeholder ds_ph" {
		t.Fatalf("ok: %+v", out)
	}
	k.bindErr, k.state = errors.New(`held by ds_9 ("Other Sentinel")`), datasubject.KGConflict
	out := svc.BindEntity(context.Background(), req, "a", nil, nil, false)
	if out.OK || !strings.Contains(out.Page, "Other Sentinel") || strings.Contains(out.Audited, "Sentinel") ||
		out.Audited != "refused: the resolver refused the bind (state conflict)" {
		t.Fatalf("refusal: %+v", out)
	}
}
