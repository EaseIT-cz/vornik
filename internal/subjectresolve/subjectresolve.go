// Package subjectresolve holds the gates around resolving a data subject
// against the knowledge graph, shared by the console page
// (/ui/operator/regulatory/requests/<id>/resolve) and `vornikctl subject
// resolve-kg` so the two cannot drift: resolution needs a VERIFIED rights
// request, every search is on record in admin_audit before anything is read,
// candidates under an active Art 18 restriction are withheld, and no audit row
// or log line carries personal data.
//
// see LLD § https://docs.vornik.io §4.17.9
package subjectresolve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"vornik.io/vornik/internal/datasubject"
	"vornik.io/vornik/internal/persistence"
)

// Name bounds: a shorter term matches nearly every row, which is a dump, not a
// search (design §4.17.2).
const (
	MaxExtraNames = 5
	MinTermRunes  = 3
	MaxTermRunes  = 100
)

// Audit actions written to admin_audit.
const (
	ActionSearch     = "subject.resolve.search"
	ActionBindEntity = "subject.resolve.bind_entity"
)

// Requests reads the rights-request ledger (*postgres.DataSubjectRepository).
type Requests interface {
	GetRequest(ctx context.Context, id string) (datasubject.Request, error)
}

// KG is the knowledge-graph resolver (*datasubject.KGResolver).
type KG interface {
	Candidates(ctx context.Context, subjectID string, names, projects []string) ([]datasubject.KGCandidate, error)
	Bind(ctx context.Context, req datasubject.KGBindRequest) (datasubject.KGBindResult, error)
}

// AuditSink is the admin_audit writer (persistence.AdminAuditRepository).
type AuditSink interface {
	Insert(ctx context.Context, e *persistence.AdminAuditEntry) error
}

// Service wires the gates. Every field is required; a nil Audit refuses every
// search (ErrNoAudit) rather than running one unaudited.
type Service struct {
	Requests Requests
	KG       KG
	Screen   persistence.RestrictionScreen
	Audit    AuditSink
	// Now is the clock for audit timestamps; nil means time.Now.
	Now func() time.Time
}

// Actor is who is resolving, as the audit row records it.
type Actor struct {
	Principal string
	Source    string // "ui" or "cli"
	IP        string
	UserAgent string
}

var (
	// ErrRequestNotFound means there is no request with that id.
	ErrRequestNotFound = errors.New("no such request")
	// ErrNoAudit means no audit sink is wired, and an unaudited search is refused.
	ErrNoAudit = errors.New("no audit log is wired, and an unaudited search is refused")
	// ErrScreen means the restriction screen failed; an unscreened list is never shown.
	ErrScreen = errors.New("the processing-restriction check failed")
)

// NotVerifiedError means the request exists but may not produce data (Art 12(6)
// not cleared, or closed).
type NotVerifiedError struct {
	ID    string
	State datasubject.RequestState
}

func (e *NotVerifiedError) Error() string {
	return fmt.Sprintf("request %s is %s: resolution needs a verified request (Art 12(6))", e.ID, e.State)
}

// Authorize is the purpose limitation: a name search over everything the
// deployment holds is lawful only because a VERIFIED rights request requires
// it.
func (s *Service) Authorize(ctx context.Context, requestID string) (datasubject.Request, error) {
	req, err := s.Requests.GetRequest(ctx, requestID)
	if errors.Is(err, persistence.ErrNotFound) {
		return datasubject.Request{}, fmt.Errorf("%w: %s", ErrRequestNotFound, requestID)
	}
	if err != nil {
		return datasubject.Request{}, err
	}
	if !req.MayProduceData() {
		return datasubject.Request{}, &NotVerifiedError{ID: req.ID, State: req.State}
	}
	return req, nil
}

// ValidateNames trims the operator's extra names and enforces the bounds: at
// most MaxExtraNames, each MinTermRunes–MaxTermRunes after trimming.
func ValidateNames(raw []string) ([]string, error) {
	var out []string
	for _, n := range raw {
		n = strings.TrimSpace(n)
		if n == "" {
			continue
		}
		if c := utf8.RuneCountInString(n); c < MinTermRunes || c > MaxTermRunes {
			return nil, fmt.Errorf("each name must be %d–%d characters", MinTermRunes, MaxTermRunes)
		}
		out = append(out, n)
	}
	if len(out) > MaxExtraNames {
		return nil, fmt.Errorf("at most %d extra names per search", MaxExtraNames)
	}
	return out, nil
}

// SearchTerms is the display name plus the extra names, de-duplicated
// case-insensitively. A display name shorter than the floor is still searched
// for KG candidates by the resolver but is not a term.
func SearchTerms(display string, extra []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, n := range append([]string{display}, extra...) {
		n = strings.TrimSpace(n)
		key := strings.ToLower(n)
		if utf8.RuneCountInString(n) < MinTermRunes || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, n)
	}
	return out
}

// TermLengths is all the audit row records about the searched names: how many
// and how long. Security review 2026-10-12: an UNKEYED hash of a low-entropy
// name reverses with a dictionary, so the audit log would be a second list of
// the subject's names (design §4.17.8).
func TermLengths(terms []string) []int {
	out := make([]int, 0, len(terms))
	for _, t := range terms {
		out = append(out, utf8.RuneCountInString(t))
	}
	return out
}

// RootCause returns the innermost wrapped error. Resolver errors wrap the
// database error with the searched name or another subject's display name
// ("search %q in %s: %w"), so a log line or a CLI error carries only the root
// cause (security review 2026-10-12).
func RootCause(err error) error {
	for {
		next := errors.Unwrap(err)
		if next == nil {
			return err
		}
		err = next
	}
}

// AuditSearch puts a search on record BEFORE anything is read. The caller
// refuses the search on any error. The row holds ids, the project filter and
// term lengths — never a name.
func (s *Service) AuditSearch(ctx context.Context, a Actor, req datasubject.Request, projects, terms []string) error {
	return s.AuditDecision(ctx, a, ActionSearch, req, map[string]any{
		"subject_id":   req.SubjectID,
		"project":      strings.Join(projects, ","),
		"terms":        len(terms),
		"term_lengths": TermLengths(terms),
	})
}

// AuditDecision writes one row for req. fields must hold ids, choices and
// constant outcome classes only.
func (s *Service) AuditDecision(ctx context.Context, a Actor, action string, req datasubject.Request, fields map[string]any) error {
	if s.Audit == nil {
		return ErrNoAudit
	}
	blob, err := json.Marshal(fields)
	if err != nil {
		return err
	}
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	return s.Audit.Insert(ctx, &persistence.AdminAuditEntry{
		ID:        persistence.GenerateID("admaud"),
		Timestamp: now().UTC(),
		Principal: a.Principal,
		Source:    a.Source,
		Action:    action,
		Target:    req.ID,
		After:     string(blob),
		IP:        a.IP,
		UserAgent: a.UserAgent,
	})
}

// Screened is a KG candidate after the restriction screen. A Withheld
// candidate may be shown by id and project only: no name, alias, match,
// count, binding state or action.
type Screened struct {
	datasubject.KGCandidate
	Withheld bool
}

// ScreenedCandidates runs the resolver's candidate search and screens the
// result. It fails closed: a screen error returns ErrScreen and no list. A
// candidate-read error is returned as is — callers log or print RootCause.
func (s *Service) ScreenedCandidates(ctx context.Context, subjectID string, names, projects []string) ([]Screened, error) {
	cands, err := s.KG.Candidates(ctx, subjectID, names, projects)
	if err != nil {
		return nil, err
	}
	if len(cands) == 0 {
		return nil, nil
	}
	ids := make([]string, 0, len(cands))
	for _, c := range cands {
		ids = append(ids, c.Entity.ID)
	}
	restricted, err := s.Screen.RestrictedEntities(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrScreen, err)
	}
	out := make([]Screened, 0, len(cands))
	for _, c := range cands {
		out = append(out, Screened{KGCandidate: c, Withheld: restricted[c.Entity.ID]})
	}
	return out, nil
}

// BindOutcome is one bind decision's result. Page is for the operator (it may
// quote another subject's display name, as Bind's conflict refusal does);
// Audited is the outcome class for admin_audit and never carries a name.
type BindOutcome struct {
	OK      bool
	Page    string
	Audited string
	Result  datasubject.KGBindResult
}

// BindEntity binds one KG entity, refusing one under an active restriction
// (fail closed on a screen error). An entity id the search did not propose is
// refused by Bind itself, which re-runs Candidates with the same names and
// projects (design §4.2.1, findEntity).
func (s *Service) BindEntity(ctx context.Context, req datasubject.Request, entityID string, names, projects []string, adopt bool) BindOutcome {
	restricted, err := s.Screen.RestrictedEntities(ctx, []string{entityID})
	if err != nil {
		out := "refused: the restriction check failed"
		return BindOutcome{Page: out, Audited: out}
	}
	if restricted[entityID] {
		out := "refused: the entity is under an active Art 18 restriction"
		return BindOutcome{Page: out, Audited: out}
	}
	res, err := s.KG.Bind(ctx, datasubject.KGBindRequest{
		SubjectID: req.SubjectID, EntityID: entityID, Names: names, Projects: projects, Adopt: adopt,
	})
	if err != nil {
		audited := "refused: the resolver refused the bind"
		if res.State != "" {
			audited += " (state " + string(res.State) + ")"
		}
		return BindOutcome{Page: "refused: " + err.Error(), Audited: audited, Result: res}
	}
	out := fmt.Sprintf("ok: %d record(s) linked", res.LinksAdded)
	if res.AdoptedFrom != "" {
		out += fmt.Sprintf(", %d moved from placeholder %s", res.LinksMoved, res.AdoptedFrom)
	}
	return BindOutcome{OK: true, Page: out, Audited: out, Result: res}
}
