package webpush

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"time"

	"vornik.io/vornik/internal/netguard"
)

// Outcomes of one push (design §9.3a B.5), the values of the metric's
// outcome label.
const (
	OutcomeSent   = "sent"   // 2xx: the push service accepted it
	OutcomeGone   = "gone"   // 404 or 410: the browser dropped the subscription
	OutcomeFailed = "failed" // anything else, a redirect included; not retried
)

// maxResponse caps what is read of a push service's answer: 4 KiB.
const maxResponse = 4 << 10

// Result is one push's outcome; Status is 0 when no answer came.
type Result struct {
	Outcome string
	Status  int
	Err     error
}

// Sender posts encrypted messages to push services.
type Sender struct {
	vapid    *VAPID
	client   *http.Client
	now      func() time.Time
	validate func(string) error
	onRead   func(n int64) // test seam: bytes read of the answer
}

func noRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// NewSender returns a sender with a 10-second client that dials through
// netguard and follows no redirect.
func NewSender(v *VAPID) *Sender {
	c := netguard.NewGuardedClient(10 * time.Second)
	c.CheckRedirect = noRedirect
	return &Sender{vapid: v, client: c, now: time.Now, validate: ValidateEndpoint}
}

// PublicKey is the VAPID key browsers subscribe with.
func (s *Sender) PublicKey() string { return s.vapid.PublicKey() }

// Send encrypts payload to the subscription and posts it once.
func (s *Sender) Send(ctx context.Context, endpoint, p256dh, auth string, payload []byte) Result {
	if err := s.validate(endpoint); err != nil {
		return Result{Outcome: OutcomeFailed, Err: err}
	}
	ua, err := DecodeKey(p256dh)
	if err != nil {
		return Result{Outcome: OutcomeFailed, Err: err}
	}
	secret, err := DecodeKey(auth)
	if err != nil {
		return Result{Outcome: OutcomeFailed, Err: err}
	}
	body, err := Encrypt(payload, ua, secret)
	if err != nil {
		return Result{Outcome: OutcomeFailed, Err: err}
	}
	authz, err := s.vapid.Header(endpoint, s.now())
	if err != nil {
		return Result{Outcome: OutcomeFailed, Err: err}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return Result{Outcome: OutcomeFailed, Err: err}
	}
	req.Header.Set("Content-Encoding", "aes128gcm")
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("TTL", "86400")
	req.Header.Set("Urgency", "high")
	req.Header.Set("Authorization", authz)
	resp, err := s.client.Do(req)
	if err != nil {
		return Result{Outcome: OutcomeFailed, Err: err}
	}
	defer func() { _ = resp.Body.Close() }()
	n, _ := io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponse))
	if s.onRead != nil {
		s.onRead(n)
	}
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return Result{Outcome: OutcomeSent, Status: resp.StatusCode}
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
		return Result{Outcome: OutcomeGone, Status: resp.StatusCode}
	}
	return Result{Outcome: OutcomeFailed, Status: resp.StatusCode, Err: errors.New("webpush: push service answered " + resp.Status)}
}
