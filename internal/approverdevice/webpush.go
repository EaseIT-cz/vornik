package approverdevice

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/rs/zerolog/log"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/webpush"
)

// Web Push to approver devices (GitHub #163, agent-administered design part
// 09 §9.3a). Every push that goes to the operator channel also goes to each
// active device that turned notifications on: the subject only, encrypted
// to the device, with the approvals list as its link.

// PushSender sends one Web Push message; *webpush.Sender is the production
// one.
type PushSender interface {
	Send(ctx context.Context, endpoint, p256dh, auth string, payload []byte) webpush.Result
	PublicKey() string
}

// pushTimeout bounds one fan-out, all devices included (§9.3a B.5).
const pushTimeout = 30 * time.Second

// pushURL is the one link a notification opens.
const pushURL = "/ui/approve/"

// ErrBadSubscription is a subscription the daemon will not store.
var ErrBadSubscription = errors.New("approverdevice: not a supported push subscription")

// errPushUnavailable means this daemon has no VAPID key or no store.
var errPushUnavailable = errors.New("approverdevice: web push is not available")

// pushState is the Web Push part of the Service. The zero value is off.
type pushState struct {
	pushRepo    persistence.ApproverPushRepository
	pushSender  PushSender
	pushOutcome func(outcome string)
	pushWG      sync.WaitGroup
}

// WithWebPush turns Web Push on: repo stores the subscriptions and sender
// posts the messages. Without it, the push page says notifications are not
// available and only the operator channel is used.
func WithWebPush(repo persistence.ApproverPushRepository, sender PushSender) Option {
	return func(s *Service) { s.pushRepo, s.pushSender = repo, sender }
}

// WithPushOutcomeRecorder counts each push's outcome (sent, gone, failed):
// vornik_approver_push_total.
func WithPushOutcomeRecorder(fn func(outcome string)) Option {
	return func(s *Service) { s.pushOutcome = fn }
}

// WebPushEnabled reports whether devices can turn notifications on.
func (s *Service) WebPushEnabled() bool { return s.pushRepo != nil && s.pushSender != nil }

// SubscribePush stores the device's subscription after checking it.
func (s *Service) SubscribePush(ctx context.Context, deviceID, endpoint, p256dh, auth string) error {
	if !s.WebPushEnabled() {
		return errPushUnavailable
	}
	if webpush.ValidateEndpoint(endpoint) != nil || webpush.ValidateKeys(p256dh, auth) != nil {
		return ErrBadSubscription
	}
	return s.pushRepo.UpsertPushSubscription(ctx, persistence.ApproverPushSubscriptionRow{
		DeviceID: deviceID, Endpoint: endpoint, P256DH: p256dh, Auth: auth, CreatedAt: s.now(),
	})
}

// UnsubscribePush deletes the device's subscription.
func (s *Service) UnsubscribePush(ctx context.Context, deviceID string) error {
	if !s.WebPushEnabled() {
		return errPushUnavailable
	}
	return s.pushRepo.DeletePushSubscription(ctx, deviceID)
}

// PushStatus returns the device's subscription, or persistence.ErrNotFound.
func (s *Service) PushStatus(ctx context.Context, deviceID string) (*persistence.ApproverPushSubscriptionRow, error) {
	if !s.WebPushEnabled() {
		return nil, persistence.ErrNotFound
	}
	return s.pushRepo.GetPushSubscription(ctx, deviceID)
}

// TestPush sends a test notification to this device only, and waits for it.
func (s *Service) TestPush(ctx context.Context, deviceID string) error {
	row, err := s.PushStatus(ctx, deviceID)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), pushTimeout)
	defer cancel()
	s.sendOne(ctx, *row, "Vornik: test notification")
	return nil
}

// webPushAll sends subject to every active subscription, off the caller's
// path: one goroutine, a fresh 30-second context, devices in turn. Delivery
// is best effort and at most once (§9.3a B.5): the request itself is a row
// on the approvals list whatever happens here.
func (s *Service) webPushAll(ctx context.Context, subject string) {
	if !s.WebPushEnabled() {
		return
	}
	s.pushWG.Add(1)
	go func() {
		defer s.pushWG.Done()
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), pushTimeout)
		defer cancel()
		subs, err := s.pushRepo.ListActivePushSubscriptions(ctx)
		if err != nil {
			log.Warn().Err(err).Msg("approver web push: could not list subscriptions")
			return
		}
		for _, sub := range subs {
			s.sendOne(ctx, sub, subject)
		}
	}()
}

// sendOne sends to one device and records the outcome. A panic in the
// sender is recovered and counted as failed.
func (s *Service) sendOne(ctx context.Context, sub persistence.ApproverPushSubscriptionRow, subject string) {
	res := webpush.Result{Outcome: webpush.OutcomeFailed}
	func() {
		defer func() {
			if p := recover(); p != nil {
				log.Error().Str("device", sub.DeviceID).Msgf("approver web push: sender panicked: %v", p)
				res = webpush.Result{Outcome: webpush.OutcomeFailed}
			}
		}()
		payload, _ := json.Marshal(map[string]string{"title": subject, "url": pushURL})
		res = s.pushSender.Send(ctx, sub.Endpoint, sub.P256DH, sub.Auth, payload)
	}()
	if s.pushOutcome != nil {
		s.pushOutcome(res.Outcome)
	}
	if res.Outcome == webpush.OutcomeGone {
		if err := s.pushRepo.DeletePushSubscription(ctx, sub.DeviceID); err != nil {
			log.Warn().Err(err).Str("device", sub.DeviceID).Msg("approver web push: could not delete a dropped subscription")
		}
		return
	}
	status := res.Outcome
	if res.Status != 0 {
		status = fmt.Sprintf("%s %d", res.Outcome, res.Status)
	}
	if res.Err != nil && res.Outcome != webpush.OutcomeSent {
		// Never the endpoint: its path is the browser's push token. A
		// *url.Error names the full URL, so only its cause is logged.
		cause := res.Err
		var ue *url.Error
		if errors.As(cause, &ue) {
			cause = ue.Err
		}
		log.Info().Err(cause).Str("device", sub.DeviceID).Str("push_host", endpointHost(sub.Endpoint)).Msg("approver web push: not delivered")
	}
	if err := s.pushRepo.RecordPushResult(ctx, sub.DeviceID, status, s.now()); err != nil {
		log.Warn().Err(err).Str("device", sub.DeviceID).Msg("approver web push: could not record the outcome")
	}
}

// endpointHost is the push service's host, the one part of an endpoint safe
// to log.
func endpointHost(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// waitPushes waits for every fan-out in flight (tests).
func (s *Service) waitPushes() { s.pushWG.Wait() }

// pushPageCSP is pageCSP with one difference: a same-origin script, for
// /ui/static/approve-push.js. Only the push page, and only when Web Push is
// on, gets it (§9.3a B.6).
const pushPageCSP = "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self'; " +
	"form-action 'self'; frame-ancestors 'none'; base-uri 'none'"

type pushPageData struct {
	Device    *Device
	Available bool
	Key       string
	On        bool
	Row       *persistence.ApproverPushSubscriptionRow
	Flash     string
}

// pushRouter serves /ui/approve/push and its three POSTs. RequireDevice has
// admitted the device and, for a POST, checked the origin.
func (s *Service) pushRouter(w http.ResponseWriter, r *http.Request, rest string) {
	d, _ := FromRequest(r)
	if !s.WebPushEnabled() {
		if rest == "" && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
			s.render(w, http.StatusOK, "push.html", pushPageData{Device: d})
			return
		}
		http.NotFound(w, r)
		return
	}
	switch {
	case rest == "" && (r.Method == http.MethodGet || r.Method == http.MethodHead):
		data := pushPageData{Device: d, Available: true, Key: s.pushSender.PublicKey(), Flash: r.URL.Query().Get("done")}
		if row, err := s.PushStatus(r.Context(), d.ID); err == nil {
			data.On, data.Row = true, row
		}
		w.Header().Set("Content-Security-Policy", pushPageCSP)
		s.render(w, http.StatusOK, "push.html", data)
	case rest == "" && r.Method == http.MethodPost:
		err := s.SubscribePush(r.Context(), d.ID, r.PostFormValue("endpoint"), r.PostFormValue("p256dh"), r.PostFormValue("auth"))
		switch {
		case errors.Is(err, ErrBadSubscription):
			s.renderStatus(w, http.StatusBadRequest, "Not supported", "This browser's push service is not one Vornik sends to. New requests still appear on the approvals list.")
		case err != nil:
			s.renderStatus(w, http.StatusInternalServerError, "Something went wrong", "Try again in a moment.")
		default:
			http.Redirect(w, r, "/ui/approve/push?done=on", http.StatusSeeOther)
		}
	case rest == "/test" && r.Method == http.MethodPost:
		if err := s.TestPush(r.Context(), d.ID); err != nil {
			s.renderStatus(w, http.StatusConflict, "Notifications are off", "Turn notifications on first.")
			return
		}
		http.Redirect(w, r, "/ui/approve/push?done=test", http.StatusSeeOther)
	case rest == "/off" && r.Method == http.MethodPost:
		if err := s.UnsubscribePush(r.Context(), d.ID); err != nil {
			s.renderStatus(w, http.StatusInternalServerError, "Something went wrong", "Try again in a moment.")
			return
		}
		http.Redirect(w, r, "/ui/approve/push?done=off", http.StatusSeeOther)
	case rest == "" || rest == "/test" || rest == "/off":
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	default:
		http.NotFound(w, r)
	}
}

// pushListLine is the list page's line for this device, empty when Web
// Push is off.
func (s *Service) pushListLine(ctx context.Context, deviceID string) string {
	if !s.WebPushEnabled() {
		return ""
	}
	if _, err := s.PushStatus(ctx, deviceID); err == nil {
		return "This device: notifications on."
	}
	return "This device: notifications off."
}
