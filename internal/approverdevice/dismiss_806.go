package approverdevice

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"

	"vornik.io/vornik/internal/persistence"
)

// GitHub #806 (agent-administered design part 22 §22.2.16d, part 06 §6.5
// amendment 2026-11-17): a failed apply and a withheld-tools notice had no
// action and no dismiss, on the phone or in the console inbox.

// GaveUpPrefix starts the reason claimAndApply records when a transient
// failure reaches the ceiling; only such a failure may be retried.
const GaveUpPrefix = "gave up after "

// ErrUnknownNotice refuses an acknowledgement of a notice that is not live.
var ErrUnknownNotice = errors.New("approverdevice: no such notice")

// Notice is one informational card: a key that changes when what it says
// changes, and its text.
type Notice struct {
	Key  string
	Text string
}

// NoticeKey is the sha256 of the parts, each preceded by its length as eight
// big-endian bytes, so two different part lists never share a key.
func NoticeKey(parts ...string) string {
	h := sha256.New()
	var n [8]byte
	for _, p := range parts {
		binary.BigEndian.PutUint64(n[:], uint64(len(p)))
		h.Write(n[:])
		h.Write([]byte(p))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Notices returns the live notices nobody acknowledged. Acks of notices that
// are no longer live are pruned, so a suspension that ends and recurs is
// shown again. A failed ack read shows every notice (never hides one it
// could not check).
func (s *Service) Notices(ctx context.Context) []Notice {
	live := s.withheldLines(ctx)
	if len(live) == 0 {
		if err := s.repo.PruneNoticeAcks(ctx, nil); err != nil {
			log.Printf("approverdevice: pruning notice acks failed (acks of ended notices stay): %v", err)
		}
		return nil
	}
	keys := make([]string, len(live))
	for i, n := range live {
		keys[i] = n.Key
	}
	if err := s.repo.PruneNoticeAcks(ctx, keys); err != nil {
		log.Printf("approverdevice: pruning notice acks failed (acks of ended notices stay): %v", err)
	}
	acked, err := s.repo.ListNoticeAcks(ctx)
	if err != nil {
		return live
	}
	out := make([]Notice, 0, len(live))
	for _, n := range live {
		if !acked[n.Key] {
			out = append(out, n)
		}
	}
	return out
}

// AcknowledgeNotice records key as acknowledged by actor; the key must be a
// live notice's.
func (s *Service) AcknowledgeNotice(ctx context.Context, key, actor string) error {
	for _, n := range s.withheldLines(ctx) {
		if n.Key == key {
			return s.repo.AckNotice(ctx, key, actor, s.now())
		}
	}
	return ErrUnknownNotice
}

// DismissApplyFailed hides a failed apply from the phone and the console.
func (s *Service) DismissApplyFailed(ctx context.Context, id, actor string) error {
	return s.repo.DismissApplyFailed(ctx, id, actor, s.now())
}

// errRetryNotReset wraps a failure before the effect ran: the request was
// not reset, so a retry did not happen.
var errRetryNotReset = errors.New("approverdevice: the retry did not start")

// RetryApplyFailed makes a given-up failure approved-unapplied again and runs
// its effect at once, as the device that asked. A failure that is not given
// up, a dismissed or expired row, and a row under a live lease are refused
// with persistence.ErrApprovalNoTransition. Any other error before the effect
// runs wraps errRetryNotReset; an error after it is the effect's.
func (s *Service) RetryApplyFailed(ctx context.Context, id string, d *Device) error {
	r, err := s.repo.GetRequest(ctx, id)
	if err != nil {
		return fmt.Errorf("%w: %w", errRetryNotReset, err)
	}
	fn, ok := s.effect(r.Kind)
	if !ok {
		return ErrUnknownKind
	}
	if err := s.repo.ResetForRetry(ctx, id, GaveUpPrefix, "device:"+d.ID, s.now()); err != nil {
		return fmt.Errorf("%w: %w", errRetryNotReset, err)
	}
	return s.claimAndApply(ctx, id, fn)
}

// retryable reports whether a failure reason is the given-up kind. It reads
// the raw reason, before masking and cutting.
func retryable(reason string) bool { return strings.HasPrefix(reason, GaveUpPrefix) }

// dismissApplyFailed is POST /ui/approve/<id>/dismiss.
func (s *Service) dismissApplyFailed(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	d, _ := FromRequest(r)
	err := s.DismissApplyFailed(r.Context(), id, "device:"+d.ID)
	switch {
	case errors.Is(err, persistence.ErrApprovalNoTransition), errors.Is(err, persistence.ErrNotFound):
		s.renderStatus(w, http.StatusConflict, "Nothing to dismiss", "This request is not a failed change any more.")
		return
	case err != nil:
		log.Printf("approverdevice: dismiss of %s failed: %v", id, err)
		s.renderStatus(w, http.StatusInternalServerError, "Something went wrong", "Try again in a moment.")
		return
	}
	http.Redirect(w, r, "/ui/approve/?done="+"1+dismissed", http.StatusSeeOther)
}

// retryApplyFailed is POST /ui/approve/<id>/retry.
func (s *Service) retryApplyFailed(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	d, _ := FromRequest(r)
	err := s.RetryApplyFailed(r.Context(), id, d)
	switch {
	case errors.Is(err, persistence.ErrApprovalNoTransition), errors.Is(err, persistence.ErrNotFound), errors.Is(err, ErrUnknownKind):
		s.renderStatus(w, http.StatusConflict, "Cannot retry", "Only a change that kept failing for a while can be retried, within a week of the request. Ask your assistant to request it again.")
		return
	case errors.Is(err, errRetryNotReset):
		log.Printf("approverdevice: retry of %s did not start: %v", id, err)
		s.renderStatus(w, http.StatusInternalServerError, "Something went wrong", "Try again in a moment.")
		return
	}
	// Any other error is the effect failing again: the list shows the new
	// outcome (a further failure, or the request back for the tick).
	http.Redirect(w, r, "/ui/approve/?done="+"1+retried", http.StatusSeeOther)
}

// acknowledgeNotice is POST /ui/approve/notices/<key>/ack.
func (s *Service) acknowledgeNotice(w http.ResponseWriter, r *http.Request, key string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	d, _ := FromRequest(r)
	err := s.AcknowledgeNotice(r.Context(), key, "device:"+d.ID)
	switch {
	case errors.Is(err, ErrUnknownNotice):
		s.renderStatus(w, http.StatusConflict, "Already gone", "This notice no longer applies.")
		return
	case err != nil:
		log.Printf("approverdevice: acknowledging a notice failed: %v", err)
		s.renderStatus(w, http.StatusInternalServerError, "Something went wrong", "Try again in a moment.")
		return
	}
	http.Redirect(w, r, "/ui/approve/?done="+"1+acknowledged", http.StatusSeeOther)
}
