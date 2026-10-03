package approverdevice

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"vornik.io/vornik/internal/approval"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/secrets"
)

// Host actions: the Hermes approval transport
// (https://docs.vornik.io).
// Hermes's own safety rules flag a command; the plugin's transport files it
// here through vornikctl; the paired phone answers with a scope. Vornik is
// the surface and the record, never the gate: Hermes enforces the answer.

// Bounds (design §4.1, §4.4).
const (
	MaxHostCommandBytes     = 4 << 10
	MaxHostDescriptionRunes = 512
	MaxPendingHostActions   = 3
	MaxHostActionsPerHour   = 30
	// hostActionCeiling bounds a row's life, not a policy.
	hostActionCeiling = 24 * time.Hour
	// hostActionMargin: filing happens after Hermes created the request,
	// and Hermes sends no creation time, so Vornik's deadline is 2 s short
	// of Hermes's (the margin the CLI uses).
	hostActionMargin = 2 * time.Second
	// HarnessHermes is the one harness with a host-action transport today.
	HarnessHermes = "hermes"
)

// Host-action choices. "always" is Hermes's, never offered from the phone
// (design §4.2): it writes a permanent entry into Hermes's allowlist.
const (
	ChoiceOnce    = "once"
	ChoiceSession = "session"
	ChoiceAlways  = "always"
	ChoiceDeny    = "deny"
)

// Host-action errors.
var (
	ErrHostActionInvalid  = errors.New("approverdevice: not a valid host approval request")
	ErrHostActionConflict = errors.New("approverdevice: a different request was already filed under this request id")
	ErrHostActionBusy     = errors.New("approverdevice: too many host approval requests")
	// ErrBadChoice: the choice is not one the request allows from the phone.
	ErrBadChoice = errors.New("approverdevice: that answer is not one this request allows")
	// ErrChoiceRequired: Decide on a kind whose decision carries a scope.
	ErrChoiceRequired = errors.New("approverdevice: this request is answered with a choice, not approve or reject")
)

// HostActionRequest is what the plugin sends: Hermes's ApprovalRequest
// fields, already redacted by Hermes (design §2).
type HostActionRequest struct {
	RequestID      string   `json:"request_id"`
	Digest         string   `json:"digest"`
	Command        string   `json:"command"`
	Description    string   `json:"description"`
	PatternKey     string   `json:"pattern_key"`
	PatternKeys    []string `json:"pattern_keys"`
	Surface        string   `json:"surface"`
	TimeoutSeconds float64  `json:"timeout_seconds"`
	AllowedChoices []string `json:"allowed_choices"`
}

// HostActionState is a filed request's state as the asker reads it.
type HostActionState struct {
	// Status is pending, approved, rejected or expired; a pending row past
	// its deadline reads expired before the tick closes it.
	Status   string    `json:"status"`
	Choice   string    `json:"choice,omitempty"`
	Deadline time.Time `json:"deadline"`
}

// hostActionDoc is the canonical rendering the device approves (§4.1).
type hostActionDoc struct {
	Kind           string   `json:"kind"`
	Harness        string   `json:"harness"`
	RequestID      string   `json:"request_id"`
	Digest         string   `json:"digest"`
	Command        string   `json:"command"`
	Description    string   `json:"description"`
	PatternKey     string   `json:"pattern_key"`
	Surface        string   `json:"surface"`
	AllowedChoices []string `json:"allowed_choices"`
	Deadline       string   `json:"deadline"`
}

// HostActionID derives the request's id from the namespace and Hermes's
// request id: "apr_" + the first 16 hex of their sha256.
func HostActionID(namespace, requestID string) string {
	sum := sha256.Sum256([]byte(namespace + "\x00" + requestID))
	return "apr_" + hex.EncodeToString(sum[:])[:16]
}

// WithHostActionRecorder counts host-action outcomes (design §8:
// vornik_host_approvals_total{harness, outcome}).
func WithHostActionRecorder(fn func(harness, outcome string)) Option {
	return func(s *Service) { s.hostRecord = fn }
}

// hostDetector is Vornik's default patterns, compiled once; entropy is off
// (see mask).
var hostDetector = sync.OnceValue(func() *secrets.MultiDetector {
	d, err := secrets.NewMultiDetector(secrets.Config{EntropyDisabled: true})
	if err != nil {
		return nil
	}
	return d
})

func (s *Service) record(harness, outcome string) {
	if s.hostRecord != nil {
		s.hostRecord(harness, outcome)
	}
}

var (
	hexID    = regexp.MustCompile(`^[0-9a-f]{1,64}$`)
	sha256Re = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

func validHostRequest(r HostActionRequest) error {
	bad := func(why string) error { return fmt.Errorf("%w: %s", ErrHostActionInvalid, why) }
	switch {
	case !hexID.MatchString(r.RequestID):
		return bad("request_id must be 1 to 64 lowercase hex characters")
	case !sha256Re.MatchString(r.Digest):
		return bad("digest must be a sha256 hex digest")
	case r.Surface != "cli" && r.Surface != "gateway":
		return bad("surface must be cli or gateway")
	case !(r.TimeoutSeconds > 0):
		return bad("timeout_seconds must be positive")
	case strings.TrimSpace(r.Command) == "":
		return bad("command is empty")
	case len(r.PatternKey) > 200:
		return bad("pattern_key is too long")
	}
	seen := map[string]bool{}
	for _, c := range r.AllowedChoices {
		switch c {
		case ChoiceOnce, ChoiceSession, ChoiceAlways, ChoiceDeny:
			seen[c] = true
		default:
			return bad("allowed_choices holds an unknown choice")
		}
	}
	if !seen[ChoiceOnce] || !seen[ChoiceDeny] {
		return bad("allowed_choices must include once and deny")
	}
	return nil
}

// cutMarker is appended where a text was cut.
func cutMarker(n int) string { return fmt.Sprintf(" … [cut by Vornik: %d more]", n) }

// cutBytes keeps at most limit bytes on a rune boundary, with a marker.
func cutBytes(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	end := limit
	for end > 0 && !utf8.RuneStart(s[end]) {
		end--
	}
	return s[:end] + cutMarker(len(s)-end)
}

// cutRunes keeps at most limit characters, with a marker.
func cutRunes(s string, limit int) string {
	r := []rune(s)
	if len(r) <= limit {
		return s
	}
	return string(r[:limit]) + cutMarker(len(r)-limit)
}

// mask applies Vornik's strong secret patterns on top of Hermes's own
// redaction (defence in depth, §3). Heuristic (entropy) findings are not
// masked: a command's paths and hashes are what the person judges.
func (s *Service) mask(text string) string {
	d := hostDetector()
	if d == nil {
		// The shipped patterns failed to compile: show nothing rather than
		// an unmasked text.
		return "[not shown: Vornik's secret masking is unavailable]"
	}
	fs := secrets.DropHeuristic(d.Scan([]byte(text)))
	if len(fs) == 0 {
		return text
	}
	return string(secrets.Redact([]byte(text), fs))
}

// hostDoc reads a host_action row's rendering.
func hostDoc(r persistence.AgentApprovalRequestRow) (hostActionDoc, bool) {
	var d hostActionDoc
	if json.Unmarshal(r.Rendered, &d) != nil || d.Kind != persistence.ApprovalKindHostAction {
		return d, false
	}
	return d, true
}

func (s *Service) hostState(r persistence.AgentApprovalRequestRow) HostActionState {
	st := HostActionState{Status: r.Status, Choice: r.DecidedChoice, Deadline: r.ExpiresAt}
	if r.Status == persistence.ApprovalPending && !s.now().Before(r.ExpiresAt) {
		st.Status = persistence.ApprovalExpired
	}
	return st
}

// FileHostAction files a Hermes approval request for namespace ns, or
// answers the state of the one already filed under the same request id
// (design §4.1). The row is created once: a refile with the same Hermes
// digest writes nothing and reads the row's state; another digest is
// ErrHostActionConflict. Over the bounds (§4.4) it is ErrHostActionBusy.
func (s *Service) FileHostAction(ctx context.Context, ns string, req HostActionRequest) (HostActionState, error) {
	if err := validHostRequest(req); err != nil {
		return HostActionState{}, err
	}
	id := HostActionID(ns, req.RequestID)
	if existing, err := s.repo.GetRequest(ctx, id); err == nil {
		return s.refile(*existing, ns, req)
	} else if !errors.Is(err, persistence.ErrNotFound) {
		return HostActionState{}, err
	}
	now := s.now()
	// Compared in seconds first: a huge float would overflow a Duration.
	life := hostActionCeiling
	if req.TimeoutSeconds <= (hostActionCeiling + hostActionMargin).Seconds() {
		life = time.Duration(req.TimeoutSeconds*float64(time.Second)) - hostActionMargin
	}
	deadline := now.Add(life)
	// Masked first, then cut: a whole secret is masked before a cut could
	// split it past recognition.
	command := cutBytes(s.mask(req.Command), MaxHostCommandBytes)
	description := cutRunes(s.mask(req.Description), MaxHostDescriptionRunes)
	allowed := append([]string(nil), req.AllowedChoices...)
	raw, err := json.Marshal(hostActionDoc{Kind: persistence.ApprovalKindHostAction, Harness: HarnessHermes,
		RequestID: req.RequestID, Digest: req.Digest, Command: command, Description: description,
		PatternKey: req.PatternKey, Surface: req.Surface, AllowedChoices: allowed, Deadline: deadline.UTC().Format(time.RFC3339)})
	if err != nil {
		return HostActionState{}, err
	}
	canon, err := approval.Canonical(raw)
	if err != nil {
		return HostActionState{}, err
	}
	sum, err := approval.CanonicalSHA256(canon)
	if err != nil {
		return HostActionState{}, err
	}
	row := persistence.AgentApprovalRequestRow{ID: id, Namespace: ns, Kind: persistence.ApprovalKindHostAction,
		Sentence: fmt.Sprintf("Hermes (%s) wants to run a command its safety rules flagged: %s.", ns, strings.TrimRight(description, ".")),
		Rendered: canon, RenderedSHA256: sum, Status: persistence.ApprovalPending, CreatedAt: now, ExpiresAt: deadline}
	limit := persistence.ApprovalCap{MaxPending: MaxPendingHostActions, MaxRecent: MaxHostActionsPerHour, Since: now.Add(-time.Hour)}
	if err := s.repo.CreateRequestCapped(ctx, row, limit, now); err != nil {
		switch {
		case errors.Is(err, persistence.ErrApprovalCapPending):
			s.record(HarnessHermes, "refused_cap")
			return HostActionState{}, fmt.Errorf("%w: %d requests are already waiting for an answer", ErrHostActionBusy, MaxPendingHostActions)
		case errors.Is(err, persistence.ErrApprovalCapRecent):
			s.record(HarnessHermes, "refused_cap")
			return HostActionState{}, fmt.Errorf("%w: %d requests in the last hour", ErrHostActionBusy, MaxHostActionsPerHour)
		}
		// Expected, not exceptional: two filings of the same Hermes request
		// (a retry racing the first call) both miss the GetRequest above,
		// and the loser's insert fails on the duplicate id. It lands here
		// and is answered as a refile of the winner's row: the same state
		// on an equal digest, a conflict otherwise, nothing written.
		if existing, gerr := s.repo.GetRequest(ctx, id); gerr == nil {
			return s.refile(*existing, ns, req)
		}
		return HostActionState{}, err
	}
	s.record(HarnessHermes, "filed")
	s.push(ctx, fmt.Sprintf("Vornik: Hermes is waiting for you (until %s)", hostClock(deadline, now)),
		fmt.Sprintf("%s\nReview it on your approver device: %s/ui/approve/%s", row.Sentence, s.origin, id))
	return s.hostState(row), nil
}

// refile answers a filing whose id exists: the row's state when it is the
// same Hermes request, a conflict otherwise. Nothing is written.
func (s *Service) refile(existing persistence.AgentApprovalRequestRow, ns string, req HostActionRequest) (HostActionState, error) {
	doc, ok := hostDoc(existing)
	if !ok || existing.Namespace != ns || doc.RequestID != req.RequestID || doc.Digest != req.Digest {
		s.record(HarnessHermes, "conflict")
		return HostActionState{}, ErrHostActionConflict
	}
	return s.hostState(existing), nil
}

// HostActionState returns the state of namespace ns's request filed under
// Hermes's requestID. Another namespace's request is persistence.ErrNotFound,
// the same as a missing one (design §4.3).
func (s *Service) HostActionState(ctx context.Context, ns, requestID string) (HostActionState, error) {
	r, err := s.repo.GetRequest(ctx, HostActionID(ns, requestID))
	if err != nil {
		return HostActionState{}, err
	}
	if doc, ok := hostDoc(*r); !ok || r.Namespace != ns || doc.RequestID != requestID {
		return HostActionState{}, persistence.ErrNotFound
	}
	return s.hostState(*r), nil
}

// HostChoices are the answers the phone offers for a host_action row: its
// allowed_choices without always, in the page's order.
func HostChoices(r persistence.AgentApprovalRequestRow) []string {
	doc, ok := hostDoc(r)
	if !ok {
		return nil
	}
	allowed := map[string]bool{}
	for _, c := range doc.AllowedChoices {
		allowed[c] = true
	}
	var out []string
	for _, c := range []string{ChoiceOnce, ChoiceSession, ChoiceDeny} {
		if allowed[c] {
			out = append(out, c)
		}
	}
	return out
}

// DecideChoice records a device's answer to a host_action request: once or
// session approve, deny rejects, and decided_choice records which, in the
// same guarded statement (design §4.2). always, or a choice the request does
// not allow, is ErrBadChoice. It takes only a *Device.
func (s *Service) DecideChoice(ctx context.Context, d *Device, requestID, shownSHA, choice string) error {
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
	if r.Kind != persistence.ApprovalKindHostAction {
		return ErrBadChoice
	}
	offered := false
	for _, c := range HostChoices(*r) {
		offered = offered || c == choice
	}
	if !offered {
		return ErrBadChoice
	}
	fn, ok := s.effect(r.Kind)
	if !ok {
		return ErrUnknownKind
	}
	approve := choice != ChoiceDeny
	if err := s.repo.DecideWithChoice(ctx, requestID, shownSHA, d.ID, approve, choice, s.now()); err != nil {
		if errors.Is(err, persistence.ErrApprovalNoTransition) {
			return ErrNotDecidable
		}
		return err
	}
	doc, _ := hostDoc(*r)
	s.record(harnessOf(doc), choice)
	if !approve {
		return nil
	}
	return s.claimAndApply(ctx, requestID, fn)
}

func harnessOf(d hostActionDoc) string {
	if d.Harness == "" {
		return HarnessHermes
	}
	return d.Harness
}

// recordExpired counts the host actions an expiry pass closed (design §8).
func (s *Service) recordExpired(rows []persistence.AgentApprovalRequestRow) {
	for _, r := range rows {
		if r.Kind != persistence.ApprovalKindHostAction {
			continue
		}
		doc, _ := hostDoc(r)
		s.record(harnessOf(doc), "expired")
	}
}

// hostClock says a deadline for the page and the push: the time, with the
// date when it is not ref's day.
func hostClock(t, ref time.Time) string {
	t, ref = t.UTC(), ref.UTC()
	if t.Format("2006-01-02") == ref.Format("2006-01-02") {
		return t.Format("15:04 MST")
	}
	return t.Format("15:04 MST on 2 Jan")
}

// hostPage is what request.html shows for a host_action row.
type hostPage struct {
	Command, PatternKey, Surface string
	Session                      bool
	Choices                      []string
	// Deadline is the sentence stating Vornik's own deadline.
	Deadline string
}

func (s *Service) hostPageData(r persistence.AgentApprovalRequestRow) *hostPage {
	doc, ok := hostDoc(r)
	if !ok {
		return nil
	}
	p := &hostPage{Command: doc.Command, PatternKey: doc.PatternKey, Choices: HostChoices(r)}
	for _, c := range p.Choices {
		p.Session = p.Session || c == ChoiceSession
	}
	p.Surface = "in a terminal"
	if doc.Surface == "gateway" {
		p.Surface = "from a chat"
	}
	when := hostClock(r.ExpiresAt, r.CreatedAt)
	switch {
	case r.Status == persistence.ApprovalExpired || (r.Status == persistence.ApprovalPending && !s.now().Before(r.ExpiresAt)):
		p.Deadline = "This request expired at " + when + "; an answer now would not be used."
	case r.ExpiresAt.Sub(r.CreatedAt) >= hostActionCeiling:
		p.Deadline = "Vornik accepts an answer until " + when + "; Hermes may wait longer and will then deny."
	default:
		p.Deadline = "Hermes asked for an answer by " + when + "."
	}
	return p
}
