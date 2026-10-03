package approverdevice

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"vornik.io/vornik/internal/approval"
	"vornik.io/vornik/internal/brokergrants"
	"vornik.io/vornik/internal/persistence"
)

// Standing grants on the approver device: broker write-actions design,
// "Tier 2: standing grants" as revised (items 2 and 8). This package only
// shows the offer, records the person's choice with the decision, and
// serves the Standing approvals page; the grant itself is created by the
// broker_action effect, in the seed approval's transaction.

// GrantOffer is a standing grant a person may create while approving.
type GrantOffer struct {
	Action     string
	Key        string
	Days, Uses []int
	Unreviewed []string
}

// GrantOfferFunc returns the offer for a request, or nil.
type GrantOfferFunc func(ctx context.Context, r persistence.AgentApprovalRequestRow) *GrantOffer

// RegisterGrantOffer makes pending requests of kind offer a standing grant
// under Approve when fn returns one.
func (s *Service) RegisterGrantOffer(kind string, fn GrantOfferFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.offers == nil {
		s.offers = map[string]GrantOfferFunc{}
	}
	s.offers[kind] = fn
}

func (s *Service) grantOffer(ctx context.Context, r persistence.AgentApprovalRequestRow) *GrantOffer {
	s.mu.RLock()
	fn := s.offers[r.Kind]
	s.mu.RUnlock()
	if fn == nil || r.Status != persistence.ApprovalPending {
		return nil
	}
	return fn(ctx, r)
}

// GrantChoice is the decided_choice an approval with a grant records.
func GrantChoice(days, uses int) string { return fmt.Sprintf("grant:%d:%d", days, uses) }

// ParseGrantChoice reads a decided_choice written by GrantChoice.
func ParseGrantChoice(c string) (days, uses int, ok bool) {
	rest, found := strings.CutPrefix(c, "grant:")
	if !found {
		return 0, 0, false
	}
	ds, us, found := strings.Cut(rest, ":")
	if !found {
		return 0, 0, false
	}
	d, err1 := strconv.Atoi(ds)
	u, err2 := strconv.Atoi(us)
	if err1 != nil || err2 != nil || d < 1 || u < 1 {
		return 0, 0, false
	}
	return d, u, true
}

func offered(xs []int, v int) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

// DecideGrant approves a request and records the standing grant chosen with
// it, in the same guarded decision statement; then the effect runs (it
// creates the grant in the seed's own approval transaction). A choice the
// request's offer does not hold is ErrBadChoice and decides nothing.
func (s *Service) DecideGrant(ctx context.Context, d *Device, requestID, shownSHA string, days, uses int) error {
	if d == nil {
		return ErrNoDevice
	}
	r, err := s.repo.GetRequest(ctx, requestID)
	if errors.Is(err, persistence.ErrNotFound) {
		return ErrNotDecidable
	}
	if err != nil {
		return err
	}
	o := s.grantOffer(ctx, *r)
	if o == nil || !offered(o.Days, days) || !offered(o.Uses, uses) {
		return ErrBadChoice
	}
	fn, ok := s.effect(r.Kind)
	if !ok {
		return ErrUnknownKind
	}
	if err := s.repo.DecideWithChoice(ctx, requestID, shownSHA, d.ID, true, GrantChoice(days, uses), s.now()); err != nil {
		if errors.Is(err, persistence.ErrApprovalNoTransition) {
			return ErrNotDecidable
		}
		return err
	}
	return s.claimAndApply(ctx, requestID, fn)
}

// StandingView is one grant on the Standing approvals page.
type StandingView struct {
	ID, Namespace, Workflow, Action, Key, State string
	UsesLeft, MaxUses                           int
	ExpiresAt                                   time.Time
	Covered                                     []StandingCovered
}

// StandingCovered is one write a grant approved, with its outcome.
type StandingCovered struct {
	ActionID, Status string
	At               time.Time
}

// StandingPages backs the Standing approvals page.
type StandingPages interface {
	List(ctx context.Context) ([]StandingView, error)
	// Change applies pause, unpause, revoke or confirm.
	Change(ctx context.Context, id, verb string) error
}

// SetStandingPages installs the Standing approvals page's backend.
func (s *Service) SetStandingPages(p StandingPages) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.standing = p
}

func (s *Service) standingPages() StandingPages {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.standing
}

type standingData struct {
	Grants []StandingView
	Flash  string
	Error  string
}

// standingPage serves GET /ui/approve/standing and
// POST /ui/approve/standing/<id>/<verb>. A change is not a decision about a
// request, so it does not rotate the device token (like a device revoke);
// it is same-origin checked.
func (s *Service) standingPage(w http.ResponseWriter, r *http.Request, rest string) {
	p := s.standingPages()
	if p == nil {
		http.NotFound(w, r)
		return
	}
	if rest == "" {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		views, err := p.List(r.Context())
		if err != nil {
			s.renderStatus(w, http.StatusInternalServerError, "Something went wrong", "Try again in a moment.")
			return
		}
		s.render(w, http.StatusOK, "standing.html", standingData{Grants: views, Flash: r.URL.Query().Get("done")})
		return
	}
	id, verb, ok := strings.Cut(rest, "/")
	if !ok || id == "" || !brokergrants.Verbs[verb] || strings.Contains(verb, "/") {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := approval.CheckRequest(r); err != nil {
		s.renderStatus(w, http.StatusForbidden, "Refused", "This request did not come from this page.")
		return
	}
	if err := p.Change(r.Context(), id, verb); err != nil {
		s.renderStatus(w, http.StatusConflict, "Not changed", "This standing approval is not in a state that allows that. Reload the page.")
		return
	}
	http.Redirect(w, r, "/ui/approve/standing?done="+verb, http.StatusSeeOther)
}
