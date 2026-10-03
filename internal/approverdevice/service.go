// Package approverdevice is the approver-device principal (agent-administered
// Vornik design §9.2): a phone browser holding a device cookie bound to a row
// in approver_devices, and the approval requests only such a device may
// decide. No API key, companion key, operator key or web session is ever
// accepted in its place; the routes this package serves consult no other
// credential at all.
package approverdevice

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode"

	"vornik.io/vornik/internal/approval"
	"vornik.io/vornik/internal/authz"
	"vornik.io/vornik/internal/chatauth"
	"vornik.io/vornik/internal/persistence"
)

// Timings (design §9.2, §12).
const (
	PairingTTL = 10 * time.Minute
	// ClaimTTL bounds a pending enrollment: the waiting phone's claim cookie
	// and the enrollment request both expire after it.
	ClaimTTL        = 15 * time.Minute
	RequestTTL      = 7 * 24 * time.Hour
	IdleExpiry      = 90 * 24 * time.Hour
	touchInterval   = time.Hour
	maxLabelRunes   = 40
	globalPairLimit = 100
)

// Errors. ErrNoDevice and ErrStaleDevice carry no detail on purpose: a guess
// learns nothing about which condition failed.
var (
	ErrNoDevice     = errors.New("approverdevice: not an approver device")
	ErrStaleDevice  = errors.New("approverdevice: this device signed in again in another tab")
	ErrRateLimited  = errors.New("approverdevice: too many attempts; wait a few minutes")
	ErrBadCode      = errors.New("approverdevice: that code is not valid (it may have expired or been used)")
	ErrUnknownKind  = errors.New("approverdevice: no effect is registered for this kind of request")
	ErrBadLabel     = errors.New("approverdevice: a device label is 1 to 40 printable characters")
	ErrNotDecidable = errors.New("approverdevice: this request is no longer pending, or it changed after you opened it")
	// ErrPermanent marks an effect failure that no retry can fix (a stale
	// read set, a change the loader refuses). The request ends with the
	// reason recorded instead of being retried every minute.
	ErrPermanent = errors.New("approverdevice: the change cannot be applied")
)

// Device is an authenticated approver device.
type Device struct {
	ID       string
	Label    string
	PairedAt time.Time
}

// ClaimState is where a pending enrollment stands.
type ClaimState string

// Claim states.
const (
	ClaimPending  ClaimState = "pending"
	ClaimApproved ClaimState = "approved"
	ClaimRejected ClaimState = "rejected"
	ClaimExpired  ClaimState = "expired"
)

// Redemption is the outcome of entering a pairing code: either a device token
// (the first device) or a claim token (a further device, awaiting approval).
type Redemption struct {
	DeviceToken string
	ClaimToken  string
	Device      *Device
}

// Effect applies an approved request. Every effect must be idempotent: the
// re-apply loop may run it again after a crash between apply and MarkApplied.
type Effect func(ctx context.Context, r persistence.AgentApprovalRequestRow) error

// Service is the device and approval-request logic.
type Service struct {
	repo   persistence.ApproverDeviceRepository
	now    func() time.Time
	perIP  *chatauth.RedemptionLimiter
	global *chatauth.RedemptionLimiter
	notify func(ctx context.Context, subject, body string)
	// notifyChannel/notifyActive: the operator channel pushes go to (§9.1a).
	notifyChannel string
	notifyActive  bool
	origin        string
	mu            sync.RWMutex
	effects       map[string]Effect
	onReject      map[string]func(ctx context.Context, r persistence.AgentApprovalRequestRow)
	// values are the value-entry handlers by kind (plan P4.2).
	values map[string]ValueEntry
	// describers produce each kind's plain summary and level (§18.7).
	describers map[string]Describer
	// connects are the sign-in starters by kind (plan P4.4).
	connects map[string]ConnectEntry
	// holder names this process in apply leases, so in a cluster one node
	// runs an approved request's effect at a time (review 4de8 F2).
	holder string
	// hostRecord counts host-action outcomes (Hermes approval transport
	// design §8).
	hostRecord func(harness, outcome string)
	// offers are the standing-grant offers by kind, and standing the
	// Standing approvals page (broker write-actions design, tier 2).
	offers   map[string]GrantOfferFunc
	standing StandingPages
}

// applyLease bounds how long one holder may run an effect before another
// node may take the request over.
const applyLease = 5 * time.Minute

// Option configures a Service.
type Option func(*Service)

// WithClock overrides the clock (tests).
func WithClock(now func() time.Time) Option { return func(s *Service) { s.now = now } }

// WithNotifier sets the push channel (design §9.3). Without one, PushConfigured
// is false and approvals still work from the page.
func WithNotifier(fn func(ctx context.Context, subject, body string)) Option {
	return func(s *Service) { s.notify = fn }
}

// WithNotifyChannel names the operator channel pushes go to and whether it
// is active, so the list page can say how this device hears of a request
// (design §9.1a): pushes never go to the browser itself.
func WithNotifyChannel(channel string, active bool) Option {
	return func(s *Service) { s.notifyChannel, s.notifyActive = channel, active }
}

// WithOrigin sets the public origin used in pushed links.
func WithOrigin(origin string) Option {
	return func(s *Service) { s.origin = strings.TrimRight(origin, "/") }
}

// WithLimiters overrides the code-entry limiters (tests).
func WithLimiters(perIP, global *chatauth.RedemptionLimiter) Option {
	return func(s *Service) { s.perIP, s.global = perIP, global }
}

// New builds a Service. It registers the device_enrollment effect, which is a
// no-op by design (plan amendment 3): the enrollment's approval signal is the
// request row itself, read by PollClaim.
func New(repo persistence.ApproverDeviceRepository, opts ...Option) *Service {
	s := &Service{
		repo:     repo,
		now:      func() time.Time { return time.Now().UTC() },
		perIP:    chatauth.NewRedemptionLimiter(),
		global:   chatauth.NewRedemptionLimiterWith(globalPairLimit, 10*time.Minute),
		effects:  map[string]Effect{},
		onReject: map[string]func(context.Context, persistence.AgentApprovalRequestRow){},
	}
	for _, o := range opts {
		o(s)
	}
	if h, err := newID("node_"); err == nil {
		s.holder = h
	} else {
		s.holder = "node_" + fmt.Sprint(time.Now().UnixNano())
	}
	s.effects[persistence.ApprovalKindDeviceEnrollment] = func(context.Context, persistence.AgentApprovalRequestRow) error { return nil }
	// A host action runs nothing in Vornik: the asker reads the answer
	// (Hermes approval transport design §4.1), so its effect is a no-op and
	// MarkApplied follows at once.
	s.effects[persistence.ApprovalKindHostAction] = func(context.Context, persistence.AgentApprovalRequestRow) error { return nil }
	return s
}

// PushConfigured reports whether enrollments and requests are pushed.
func (s *Service) PushConfigured() bool { return s.notify != nil }

// RegisterEffect installs the effect for a request kind. Until a kind has one,
// requests of that kind cannot be approved (Decide returns ErrUnknownKind),
// which keeps later phases' kinds inert until they ship their effects.
func (s *Service) RegisterEffect(kind string, fn Effect) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.effects[kind] = fn
}

// RegisterOnReject installs what runs when a device rejects a request of
// kind (best effort: the rejection itself is already recorded).
func (s *Service) RegisterOnReject(kind string, fn func(ctx context.Context, r persistence.AgentApprovalRequestRow)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onReject[kind] = fn
}

// ValueEntry takes the value a request asks for (a credential slot, plan
// P4.2). It decides the request itself (Decide, then store), so entering
// the value is the approval. It returns ErrNotDecidable when the decision
// was refused (nothing stored), or ErrValueNotStored when the request was
// approved but the value could not be kept.
type ValueEntry func(ctx context.Context, d *Device, r persistence.AgentApprovalRequestRow, shownSHA string, value []byte) error

// ErrValueNotStored means a value-entry request was approved, but its value
// was not stored; the request records why and the agent asks again.
var ErrValueNotStored = errors.New("approverdevice: the request was approved, but the value could not be stored")

// MaxValueBytes bounds an entered value.
const MaxValueBytes = 16 << 10

// RegisterValueEntry makes requests of kind take a value on their page.
// Description is what the approval page shows first for a request (design
// §18.7 of the agent-administered design): a plain summary and a risk level
// with its reasons, produced by the requester from the approved document,
// never by an LLM or the requesting agent.
type Description struct {
	Summary string
	Level   string
	Reasons []string
	// Group, when set, puts the request with the others of the same group
	// (for writes: the task that drafted them) so they are reviewed together
	// and decided in one pass (approval fatigue, tier 1). GroupTitle names
	// the group on the list. Only [a-z0-9_] in Group.
	Group, GroupTitle string
}

// Describer reads a request of one kind and returns its Description, or nil
// to show the page without one.
type Describer func(r persistence.AgentApprovalRequestRow) *Description

// RegisterDescriber installs the describer for requests of kind.
func (s *Service) RegisterDescriber(kind string, fn Describer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.describers == nil {
		s.describers = map[string]Describer{}
	}
	s.describers[kind] = fn
}

// Describe returns the request's Description, or nil when its kind has no
// describer.
func (s *Service) Describe(r persistence.AgentApprovalRequestRow) *Description { return s.describe(r) }

func (s *Service) describe(r persistence.AgentApprovalRequestRow) *Description {
	s.mu.RLock()
	fn := s.describers[r.Kind]
	s.mu.RUnlock()
	if fn == nil {
		return nil
	}
	return fn(r)
}

func (s *Service) RegisterValueEntry(kind string, fn ValueEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.values == nil {
		s.values = map[string]ValueEntry{}
	}
	s.values[kind] = fn
}

func (s *Service) valueEntry(kind string) (ValueEntry, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	fn, ok := s.values[kind]
	return fn, ok
}

// ConnectEntry starts a sign-in for a request (an OAuth credential slot,
// plan P4.4). Starting decides nothing: the request is decided when the
// sign-in comes back. Start returns where to send the phone and the cookie
// that binds the flow to it.
type ConnectEntry interface {
	Applies(r persistence.AgentApprovalRequestRow) bool
	Start(ctx context.Context, d *Device, r persistence.AgentApprovalRequestRow, shownSHA string) (authURL string, flow *http.Cookie, err error)
}

// RegisterConnect makes requests of kind that Applies offer Connect.
func (s *Service) RegisterConnect(kind string, c ConnectEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.connects == nil {
		s.connects = map[string]ConnectEntry{}
	}
	s.connects[kind] = c
}

func (s *Service) connectEntry(r persistence.AgentApprovalRequestRow) (ConnectEntry, bool) {
	s.mu.RLock()
	c, ok := s.connects[r.Kind]
	s.mu.RUnlock()
	if !ok || !c.Applies(r) {
		return nil, false
	}
	return c, true
}

// ActiveDevice returns a paired device that is neither revoked nor idle
// past IdleExpiry, or ErrNoDevice. It is how a flow that left the device's
// cookie scope (an OAuth sign-in) finds its device again.
func (s *Service) ActiveDevice(ctx context.Context, id string) (*Device, error) {
	rows, err := s.repo.ListDevices(ctx)
	if err != nil {
		return nil, err
	}
	for _, d := range rows {
		if d.ID == id && d.RevokedAt == nil && s.now().Sub(d.LastUsedAt) <= IdleExpiry {
			return &Device{ID: d.ID, Label: d.Label, PairedAt: d.PairedAt}, nil
		}
	}
	return nil, ErrNoDevice
}

// browserLine follows every pushed link: the device is a cookie in one
// browser, and a chat app opens links in its own (§9.2a).
const browserLine = "Open it in the browser you paired (in Telegram: ••• → Open in Chrome or Safari)."

// FileRequest stores an approval request and pushes its sentence and link
// (§9.3): no values, no decision buttons.
func (s *Service) FileRequest(ctx context.Context, r persistence.AgentApprovalRequestRow) error {
	if err := s.repo.CreateRequest(ctx, r); err != nil {
		return err
	}
	s.push(ctx, "Vornik: your assistant is asking for approval",
		fmt.Sprintf("%s\nReview it on your approver device: %s/ui/approve/%s\n%s", r.Sentence, s.origin, r.ID, browserLine))
	return nil
}

// FileRequestBatch files several requests that belong together (the writes
// one task drafted) and pushes once, with summary as the message and the
// list's link: content-free, as every push is (approval fatigue, tier 1). A
// request already filed is skipped; when none is new, nothing is pushed.
func (s *Service) FileRequestBatch(ctx context.Context, rows []persistence.AgentApprovalRequestRow, summary string) error {
	filed := 0
	for _, r := range rows {
		if _, err := s.repo.GetRequest(ctx, r.ID); err == nil {
			continue
		}
		if err := s.repo.CreateRequest(ctx, r); err != nil {
			if _, gerr := s.repo.GetRequest(ctx, r.ID); gerr == nil {
				continue // a concurrent filing won
			}
			return err
		}
		filed++
	}
	if filed > 0 {
		s.push(ctx, "Vornik: your assistant is asking for approval",
			fmt.Sprintf("%s.\nReview them on your approver device: %s/ui/approve/\n%s", summary, s.origin, browserLine))
	}
	return nil
}

func (s *Service) effect(kind string) (Effect, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	fn, ok := s.effects[kind]
	return fn, ok
}

// HashToken is the stored digest of a device or claim token.
func HashToken(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}

func newToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func newID(prefix string) (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(b), nil
}

// CleanLabel bounds a device label: 1 to 40 runes, control characters dropped.
func CleanLabel(label string) (string, error) {
	out := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, strings.TrimSpace(label))
	out = strings.TrimSpace(out)
	if n := len([]rune(out)); n == 0 || n > maxLabelRunes {
		return "", ErrBadLabel
	}
	return out, nil
}

// StartPairing issues a one-time code (vornikctl pair-device).
func (s *Service) StartPairing(ctx context.Context, label string) (code string, expires time.Time, err error) {
	label, err = CleanLabel(label)
	if err != nil {
		return "", time.Time{}, err
	}
	code, err = authz.NewOneTimeCode()
	if err != nil {
		return "", time.Time{}, err
	}
	now := s.now()
	expires = now.Add(PairingTTL)
	if err := s.repo.CreatePairing(ctx, persistence.ApproverPairingRow{
		CodeHash: authz.HashOneTimeCode(code), Label: label, CreatedAt: now, ExpiresAt: expires,
	}); err != nil {
		return "", time.Time{}, err
	}
	return code, expires, nil
}

// DeviceExists reports whether at least one unrevoked device exists.
func (s *Service) DeviceExists(ctx context.Context) (bool, error) {
	n, err := s.repo.CountActiveDevices(ctx)
	return n > 0, err
}

// Redeem enters a pairing code. The limiters are consulted first and count
// attempts, not failures; unknown codes therefore spend the caller's own
// per-IP budget, which is intended (plan amendment 10). A refused attempt
// consumes no code.
func (s *Service) Redeem(ctx context.Context, code, clientIP string) (Redemption, error) {
	if !s.perIP.Allow("approver-pair", clientIP) || !s.global.Allow("approver-pair", "*") {
		return Redemption{}, ErrRateLimited
	}
	codeHash := authz.HashOneTimeCode(code)
	// The label is read first so the device row and the enrollment sentence
	// carry it. This is no oracle beyond redemption itself, and the limiter
	// has already counted the attempt. RedeemPairing re-checks the code
	// atomically.
	p, err := s.repo.GetPairing(ctx, codeHash, s.now())
	if errors.Is(err, persistence.ErrNotFound) {
		return Redemption{}, ErrBadCode
	}
	if err != nil {
		return Redemption{}, err
	}
	now := s.now()
	devTok, err := newToken()
	if err != nil {
		return Redemption{}, err
	}
	claimTok, err := newToken()
	if err != nil {
		return Redemption{}, err
	}
	devID, err := newID("dev_")
	if err != nil {
		return Redemption{}, err
	}
	reqID, err := newID("apr_")
	if err != nil {
		return Redemption{}, err
	}
	d := persistence.ApproverDeviceRow{ID: devID, Label: p.Label, TokenHash: HashToken(devTok),
		PairedAt: now, PairedBy: "first-device", LastUsedAt: now}
	req, err := enrollmentRequest(reqID, p.Label, now)
	if err != nil {
		return Redemption{}, err
	}
	first, err := s.repo.RedeemPairing(ctx, codeHash, HashToken(claimTok), d, req, now)
	if errors.Is(err, persistence.ErrNotFound) {
		return Redemption{}, ErrBadCode
	}
	if err != nil {
		return Redemption{}, err
	}
	if first {
		s.pushEnrolled(ctx, p.Label, now)
		return Redemption{DeviceToken: devTok, Device: &Device{ID: devID, Label: p.Label, PairedAt: now}}, nil
	}
	s.push(ctx, "Vornik: a new device wants to approve for you",
		fmt.Sprintf("A new device %q asked to become an approver at %s. Approve or reject it on a device you already use: %s/ui/approve/%s",
			p.Label, now.Format(time.RFC1123), s.origin, reqID))
	return Redemption{ClaimToken: claimTok}, nil
}

// enrollmentRequest renders the device_enrollment request: the sentence from
// a fixed template, and the canonical JSON the device approves.
func enrollmentRequest(id, label string, now time.Time) (persistence.AgentApprovalRequestRow, error) {
	raw, err := json.Marshal(map[string]string{"kind": persistence.ApprovalKindDeviceEnrollment, "label": label, "requested_at": now.Format(time.RFC3339)})
	if err != nil {
		return persistence.AgentApprovalRequestRow{}, err
	}
	canon, err := approval.Canonical(raw)
	if err != nil {
		return persistence.AgentApprovalRequestRow{}, err
	}
	sum, err := approval.CanonicalSHA256(canon)
	if err != nil {
		return persistence.AgentApprovalRequestRow{}, err
	}
	return persistence.AgentApprovalRequestRow{
		ID: id, Kind: persistence.ApprovalKindDeviceEnrollment,
		Sentence: fmt.Sprintf("A new device %q wants to approve for you (requested %s).", label, now.Format("2 Jan 15:04 MST")),
		Rendered: canon, RenderedSHA256: sum, Status: persistence.ApprovalPending,
		CreatedAt: now, ExpiresAt: now.Add(ClaimTTL),
	}, nil
}

// PollClaim reports a pending enrollment's state. On approval it creates the
// device and returns its token, once: the token exists only from this
// moment, never before approval (plan amendment 3).
func (s *Service) PollClaim(ctx context.Context, claimToken string) (string, ClaimState, error) {
	p, err := s.repo.GetPairingByClaim(ctx, HashToken(claimToken))
	if errors.Is(err, persistence.ErrNotFound) {
		return "", ClaimExpired, nil
	}
	if err != nil {
		return "", "", err
	}
	if p.DeviceID != "" || p.RequestID == "" { // completed already, or the impossible unattached claim
		return "", ClaimExpired, nil
	}
	r, err := s.repo.GetRequest(ctx, p.RequestID)
	if errors.Is(err, persistence.ErrNotFound) {
		return "", ClaimExpired, nil
	}
	if err != nil {
		return "", "", err
	}
	now := s.now()
	switch {
	case r.Status == persistence.ApprovalRejected:
		return "", ClaimRejected, nil
	case r.Status == persistence.ApprovalExpired, r.Status == persistence.ApprovalPending && !now.Before(r.ExpiresAt):
		return "", ClaimExpired, nil
	case r.Status == persistence.ApprovalPending:
		return "", ClaimPending, nil
	}
	tok, err := newToken()
	if err != nil {
		return "", "", err
	}
	devID, err := newID("dev_")
	if err != nil {
		return "", "", err
	}
	d := persistence.ApproverDeviceRow{ID: devID, Label: p.Label, TokenHash: HashToken(tok), PairedAt: now,
		PairedBy: "device:" + r.DecidedByDevice, LastUsedAt: now}
	if err := s.repo.CompletePairing(ctx, HashToken(claimToken), d); err != nil {
		if errors.Is(err, persistence.ErrNotFound) {
			return "", ClaimExpired, nil
		}
		return "", "", err
	}
	s.pushEnrolled(ctx, p.Label, now)
	return tok, ClaimApproved, nil
}

// Authenticate resolves a device token. Unknown, revoked and idle-expired
// devices are one error, so the cookie value learns nothing.
func (s *Service) Authenticate(ctx context.Context, token string) (*Device, error) {
	if token == "" {
		return nil, ErrNoDevice
	}
	d, err := s.repo.GetDeviceByTokenHash(ctx, HashToken(token))
	if errors.Is(err, persistence.ErrNotFound) {
		return nil, ErrNoDevice
	}
	if err != nil {
		return nil, err
	}
	now := s.now()
	if d.RevokedAt != nil || now.Sub(d.LastUsedAt) > IdleExpiry {
		return nil, ErrNoDevice
	}
	if now.Sub(d.LastUsedAt) > touchInterval {
		_ = s.repo.TouchDevice(ctx, d.ID, now)
	}
	return &Device{ID: d.ID, Label: d.Label, PairedAt: d.PairedAt}, nil
}

// Rotate re-issues a device's token after a decision (design §9.2). A stale
// presented token (another tab rotated first) is ErrStaleDevice.
func (s *Service) Rotate(ctx context.Context, d *Device, oldToken string) (string, error) {
	tok, err := newToken()
	if err != nil {
		return "", err
	}
	if err := s.repo.RotateToken(ctx, d.ID, HashToken(oldToken), HashToken(tok), s.now()); err != nil {
		if errors.Is(err, persistence.ErrNotFound) {
			return "", ErrStaleDevice
		}
		return "", err
	}
	return tok, nil
}

// Revoke revokes a device; revoking an unknown or revoked device is a no-op.
func (s *Service) Revoke(ctx context.Context, id string) error {
	return s.repo.RevokeDevice(ctx, id, s.now())
}

// ListDevices lists every device, revoked ones included.
func (s *Service) ListDevices(ctx context.Context) ([]persistence.ApproverDeviceRow, error) {
	return s.repo.ListDevices(ctx)
}

// ListPending lists requests awaiting a decision.
func (s *Service) ListPending(ctx context.Context) ([]persistence.AgentApprovalRequestRow, error) {
	return s.repo.ListPending(ctx, s.now())
}

// Request returns one request.
func (s *Service) Request(ctx context.Context, id string) (*persistence.AgentApprovalRequestRow, error) {
	return s.repo.GetRequest(ctx, id)
}

// Decide records a device's decision and, on approval, applies the effect.
// It takes only a *Device: there is no other principal it accepts. A kind
// with no registered effect is refused before the transition.
func (s *Service) Decide(ctx context.Context, d *Device, requestID, shownSHA string, approve bool) error {
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
	if r.Kind == persistence.ApprovalKindHostAction {
		// A plain approve would lose the scope (Hermes approval transport
		// design §4.2): DecideChoice answers these.
		return ErrChoiceRequired
	}
	fn, ok := s.effect(r.Kind)
	if !ok {
		return ErrUnknownKind
	}
	if err := s.repo.Decide(ctx, requestID, shownSHA, d.ID, approve, s.now()); err != nil {
		if errors.Is(err, persistence.ErrApprovalNoTransition) {
			return ErrNotDecidable
		}
		return err
	}
	if !approve {
		s.mu.RLock()
		hook := s.onReject[r.Kind]
		s.mu.RUnlock()
		if hook != nil {
			if decided, err := s.repo.GetRequest(ctx, requestID); err == nil {
				hook(ctx, *decided)
			}
		}
		return nil
	}
	return s.claimAndApply(ctx, requestID, fn)
}

// claimAndApply runs an approved request's effect under a lease, on the row
// AS DECIDED (re-read after the transition, so an effect sees the approver and
// the decision time, the same row the re-apply loop reads; review 4de8 F1).
// Another node holding the lease means it is being applied there: not an error.
func (s *Service) claimAndApply(ctx context.Context, id string, fn Effect) error {
	now := s.now()
	ok, err := s.repo.ClaimApply(ctx, id, s.holder, now.Add(applyLease), now)
	if err != nil || !ok {
		return err
	}
	r, err := s.repo.GetRequest(ctx, id)
	if err != nil {
		return err
	}
	if err := fn(ctx, *r); err != nil {
		if errors.Is(err, ErrPermanent) {
			if merr := s.repo.MarkApplyFailed(ctx, id, err.Error(), s.now()); merr != nil {
				return merr
			}
			return fmt.Errorf("approverdevice: request %s was approved, but it cannot be applied: %w", id, err)
		}
		return fmt.Errorf("approverdevice: request %s was approved, but applying it failed (attempt %d; it will be retried): %w", id, r.ApplyAttempts, err)
	}
	return s.repo.MarkApplied(ctx, id, s.now())
}

// Tick runs the once-a-minute housekeeping: expire pending requests past
// their TTL (§12), and re-apply approved requests whose effect did not run.
func (s *Service) Tick(ctx context.Context) error {
	expired, err := s.repo.ExpirePendingRows(ctx, s.now())
	if err != nil {
		return err
	}
	s.recordExpired(expired)
	return s.ReapplyApproved(ctx)
}

// ReapplyApproved retries effects of approved, unapplied requests.
func (s *Service) ReapplyApproved(ctx context.Context) error {
	rows, err := s.repo.ListApprovedUnapplied(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, r := range rows {
		fn, ok := s.effect(r.Kind)
		if !ok {
			continue
		}
		if err := s.claimAndApply(ctx, r.ID, fn); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Run ticks every interval until ctx ends.
func (s *Service) Run(ctx context.Context, interval time.Duration, onErr func(error)) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := s.Tick(ctx); err != nil && onErr != nil {
				onErr(err)
			}
		}
	}
}

// pushEnrolled raises the enrollment alert (design §9.2): label and time,
// no values, no buttons.
func (s *Service) pushEnrolled(ctx context.Context, label string, at time.Time) {
	s.push(ctx, "Vornik: a new approver device was paired",
		fmt.Sprintf("A new approver device %q was paired at %s. If this was not you, revoke it: %s/ui/approve/devices",
			label, at.Format(time.RFC1123), s.origin))
}

func (s *Service) push(ctx context.Context, subject, body string) {
	if s.notify != nil {
		s.notify(ctx, subject, body)
	}
}
