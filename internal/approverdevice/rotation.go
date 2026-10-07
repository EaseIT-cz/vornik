package approverdevice

// Device-cookie rotation that survives a lost response (agent-administered
// design §9.2, amendment 2026-10-05 "Rotation must survive a lost response";
// P1: paired phones fell back to the pairing page while listed as active).
//
// Each decision still re-issues the cookie. The rotation is SHARED with
// requests already in flight on the previous value — they receive the same,
// deterministic successor — until shortly after the rotating response was
// written; then the previous value dies as before. The most recently killed
// value is recognised: "expired" (its successor was never presented: the
// response was lost) is resumable with a pairing code; "confirmed" (another
// browser presented the successor) is not.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"

	"vornik.io/vornik/internal/authz"
	"vornik.io/vornik/internal/persistence"
)

const (
	// ShareGrace is how long after the rotating response a rotation is still
	// shared with in-flight requests on the previous value.
	ShareGrace = 10 * time.Second
	// ShareCap bounds a share whose handler never closes it.
	ShareCap = 5 * time.Minute
	// StreakAlert is the number of consecutive shares that admitted another
	// request at which the clone alert fires.
	StreakAlert = 2
)

// ErrDeadDevice is a recognised dead value. It is an ErrNoDevice: every
// route refuses it; only the pairing page reads its reason.
type ErrDeadDevice struct{ Reason string }

func (e *ErrDeadDevice) Error() string { return "approverdevice: signed out (" + e.Reason + ")" }

// Is makes a dead value an ErrNoDevice.
func (e *ErrDeadDevice) Is(target error) bool { return target == ErrNoDevice }

// successor is the deterministic successor of the PRESENTED plaintext value
// during a share. The row stores only hashes and the nonce, so it cannot be
// recomputed from the database; the nonce is erased when the share ends.
func successor(deviceID, presented, nonce string) string {
	h := sha256.New()
	for _, part := range []string{"vornik/approver-rotation/v1", deviceID, presented, nonce} {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	return base64.RawURLEncoding.EncodeToString(h.Sum(nil))
}

func newNonce() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// authResult is what a presented value resolves to.
type authResult struct {
	device *Device
	// reissue is the share's successor when the request was admitted on the
	// previous value; the response must carry it.
	reissue string
}

func (s *Service) usable(d *persistence.ApproverDeviceRow, now time.Time) bool {
	return d.RevokedAt == nil && now.Sub(d.LastUsedAt) <= IdleExpiry
}

// authenticate resolves a presented value (design §9.2, amendment
// 2026-10-05): the current value (confirming any rotation), the previous one
// inside its share (admitted, with the successor to re-send), or a dead one
// (*ErrDeadDevice). Anything else is ErrNoDevice.
func (s *Service) authenticate(ctx context.Context, token string) (authResult, error) {
	if token == "" {
		return authResult{}, ErrNoDevice
	}
	h := HashToken(token)
	d, err := s.repo.GetDeviceByTokenHash(ctx, h)
	if errors.Is(err, persistence.ErrNotFound) {
		return authResult{}, ErrNoDevice
	}
	if err != nil {
		return authResult{}, err
	}
	now := s.now()
	if !s.usable(d, now) {
		return authResult{}, ErrNoDevice
	}
	dev := &Device{ID: d.ID, Label: d.Label, PairedAt: d.PairedAt}
	switch h {
	case d.TokenHash:
		if d.PrevTokenHash != "" || d.DeadReason == persistence.DeadExpired {
			if err := s.repo.ConfirmToken(ctx, d.ID, h); err != nil {
				return authResult{}, err
			}
		}
		// The first presentation touches at once, so "never used" (the
		// enrollment re-mint guard, design §9.2 T11) is exact and not blurred
		// by touchInterval. That touch fails closed: if it cannot be written
		// the device would still read "never used" and a claim-cookie holder
		// could re-mint a token in use. The hourly touch stays best-effort.
		if d.LastUsedAt.Equal(d.PairedAt) {
			if err := s.repo.TouchDevice(ctx, d.ID, now); err != nil {
				return authResult{}, err
			}
		} else if now.Sub(d.LastUsedAt) > touchInterval {
			_ = s.repo.TouchDevice(ctx, d.ID, now)
		}
		return authResult{device: dev}, nil
	case d.PrevTokenHash:
		if d.ShareUntil != nil && now.Before(*d.ShareUntil) {
			s.admit(ctx, d)
			return authResult{device: dev, reissue: successor(d.ID, token, d.RotationNonce)}, nil
		}
		// The share is over: the same guarded transition the tick runs.
		if err := s.repo.ExpireShares(ctx, now); err != nil {
			return authResult{}, err
		}
		return authResult{}, &ErrDeadDevice{Reason: persistence.DeadExpired}
	case d.DeadTokenHash:
		return authResult{}, &ErrDeadDevice{Reason: d.DeadReason}
	}
	return authResult{}, ErrNoDevice
}

// admit records an admission on the previous value during a share and raises
// the clone alert once the streak reaches StreakAlert.
func (s *Service) admit(ctx context.Context, d *persistence.ApproverDeviceRow) {
	streak, first, err := s.repo.AdmitShare(ctx, d.ID, d.RotationNonce)
	if err != nil {
		log.Printf("approverdevice: record share admission for %s: %v", d.ID, err)
		return
	}
	if first && streak == StreakAlert {
		s.push(ctx, "Vornik: another browser may be using an approver device's sign-in",
			fmt.Sprintf("Approver device %q: another browser has been using its sign-in during your approvals. "+
				"If you did not double-tap, or lose your connection, twice in a row, revoke it: %s/ui/approve/devices",
				d.Label, s.origin))
	}
}

// Authenticate resolves a device token. Unknown, revoked, idle-expired and
// dead values are all ErrNoDevice (errors.Is), so the cookie value learns
// nothing on the approval routes.
func (s *Service) Authenticate(ctx context.Context, token string) (*Device, error) {
	res, err := s.authenticate(ctx, token)
	if err != nil {
		return nil, err
	}
	return res.device, nil
}

// rotation is one issued successor and the share it belongs to.
type rotation struct {
	token string
	nonce string
}

// rotate re-issues a device's value after a decision. From the current value
// it opens a share; from the previous value inside a share it returns that
// share's successor, writing nothing but the admission. Anything else is
// ErrStaleDevice.
func (s *Service) rotate(ctx context.Context, d *Device, presented string) (rotation, error) {
	if d == nil {
		return rotation{}, ErrNoDevice
	}
	nonce, err := newNonce()
	if err != nil {
		return rotation{}, err
	}
	now := s.now()
	tok := successor(d.ID, presented, nonce)
	err = s.repo.RotateToken(ctx, d.ID, HashToken(presented), HashToken(tok), nonce, now.Add(ShareCap), now)
	if err == nil {
		return rotation{token: tok, nonce: nonce}, nil
	}
	if !errors.Is(err, persistence.ErrNotFound) {
		return rotation{}, err
	}
	// The presented value is not current: a share from it, if one is open.
	row, err := s.repo.GetDeviceByTokenHash(ctx, HashToken(presented))
	if errors.Is(err, persistence.ErrNotFound) {
		return rotation{}, ErrStaleDevice
	}
	if err != nil {
		return rotation{}, err
	}
	if row.ID != d.ID || !s.usable(row, now) || row.PrevTokenHash != HashToken(presented) ||
		row.ShareUntil == nil || !now.Before(*row.ShareUntil) {
		return rotation{}, ErrStaleDevice
	}
	s.admit(ctx, row)
	return rotation{token: successor(d.ID, presented, row.RotationNonce), nonce: row.RotationNonce}, nil
}

// Rotate re-issues a device's token after a decision (design §9.2); see
// rotate. A value that is neither current nor shared is ErrStaleDevice.
func (s *Service) Rotate(ctx context.Context, d *Device, presented string) (string, error) {
	r, err := s.rotate(ctx, d, presented)
	return r.token, err
}

// closeShare ends a rotation's share ShareGrace after its response was
// written. Called in a defer, so an error render closes it too.
func (s *Service) closeShare(ctx context.Context, deviceID string, r rotation) {
	if r.nonce == "" {
		return
	}
	if err := s.repo.CloseShare(context.WithoutCancel(ctx), deviceID, r.nonce, s.now().Add(ShareGrace)); err != nil {
		log.Printf("approverdevice: close share for %s: %v", deviceID, err)
	}
}

// Resume restores the device whose value died as expired — its last
// rotation's response never arrived — given a fresh pairing code from the
// terminal (design §9.2, amendment 2026-10-05). The same device row gets a
// new value. Rate limited as Redeem; a confirmed dead value, a revoked device
// or a bad code is ErrBadCode, and nothing changes.
func (s *Service) Resume(ctx context.Context, code, deadToken, clientIP string) (string, *Device, error) {
	if !s.perIP.Allow("approver-pair", clientIP) || !s.global.Allow("approver-pair", "*") {
		return "", nil, ErrRateLimited
	}
	tok, err := newToken()
	if err != nil {
		return "", nil, err
	}
	now := s.now()
	row, err := s.repo.ResumeDevice(ctx, authz.HashOneTimeCode(code), HashToken(deadToken), HashToken(tok), now)
	if errors.Is(err, persistence.ErrNotFound) {
		return "", nil, ErrBadCode
	}
	if err != nil {
		return "", nil, err
	}
	s.push(ctx, "Vornik: an approver device was restored",
		fmt.Sprintf("Approver device %q was restored with a pairing code at %s, after the answer to its last approval did not reach it. If this was not you, revoke it: %s/ui/approve/devices",
			row.Label, now.Format(time.RFC1123), s.origin))
	return tok, &Device{ID: row.ID, Label: row.Label, PairedAt: row.PairedAt}, nil
}

// cookieWriter writes the device cookie lazily: at the first WriteHeader or
// Write, only if the value is still the device's current one, so a slow
// response cannot overwrite a newer value in the jar. It fails open: if the
// check errors, the cookie is sent (as built).
type cookieWriter struct {
	http.ResponseWriter
	s       *Service
	r       *http.Request
	token   string
	written bool
}

// withCookie wraps w so the response carries token, lazily.
func (s *Service) withCookie(w http.ResponseWriter, r *http.Request, token string) http.ResponseWriter {
	if cw, ok := w.(*cookieWriter); ok {
		cw.token = token
		return cw
	}
	return &cookieWriter{ResponseWriter: w, s: s, r: r, token: token}
}

func (c *cookieWriter) emit() {
	if c.written {
		return
	}
	c.written = true
	if c.token == "" {
		return
	}
	d, err := c.s.repo.GetDeviceByTokenHash(c.r.Context(), HashToken(c.token))
	if err == nil && (d.TokenHash != HashToken(c.token) || d.RevokedAt != nil) {
		return // superseded (the jar keeps its newer value) or revoked
	}
	SetCookie(c.ResponseWriter, c.r, c.token)
}

func (c *cookieWriter) WriteHeader(code int) {
	c.emit()
	c.ResponseWriter.WriteHeader(code)
}

func (c *cookieWriter) Write(b []byte) (int, error) {
	c.emit()
	return c.ResponseWriter.Write(b)
}

func (c *cookieWriter) Flush() {
	c.emit()
	if f, ok := c.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (c *cookieWriter) Unwrap() http.ResponseWriter { return c.ResponseWriter }
