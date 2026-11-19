package approverdevice

// GitHub #763 (design part 09, amendment #763): pairing codes and revokes
// are audited here, for every caller, as Remove already is.

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// ErrAlreadyRevoked answers a revoke of a device that is already revoked.
var ErrAlreadyRevoked = errors.New("approver device: already revoked")

// ErrLastDevice answers RevokeKeepingOne for the last live device.
var ErrLastDevice = errors.New("approver device: the last live device")

func (s *Service) auditWriter() persistence.AdminAuditRepository {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.audit
}

// StartPairingBy issues a one-time code and records one device.pair_code
// row naming who asked, the label and the expiry, never the code. Without
// an audit writer it refuses (ErrNoAudit). When the row cannot be written
// it returns the error and no code; the stored hash expires unused.
func (s *Service) StartPairingBy(ctx context.Context, label, principal, source string) (string, time.Time, error) {
	audit := s.auditWriter()
	if audit == nil {
		return "", time.Time{}, ErrNoAudit
	}
	code, expires, err := s.StartPairing(ctx, label)
	if err != nil {
		return "", time.Time{}, err
	}
	clean, _ := CleanLabel(label)
	after, _ := json.Marshal(map[string]string{"label": clean, "expires_at": expires.UTC().Format(time.RFC3339)})
	if err := audit.Insert(ctx, &persistence.AdminAuditEntry{ID: persistence.GenerateID("admaud"), Timestamp: s.now(),
		Principal: principal, Source: source, Action: "device.pair_code", Target: "pairing", After: string(after)}); err != nil {
		return "", time.Time{}, err
	}
	return code, expires, nil
}

// RevokeBy revokes a live device and records one device.revoke row. An
// unknown device is persistence.ErrNotFound and a revoked one
// ErrAlreadyRevoked; neither writes a row. A revoke removes privilege, so it
// is never refused for want of an audit writer: without one it revokes and
// returns ErrNoAudit beside the done revoke, for the caller to report.
func (s *Service) RevokeBy(ctx context.Context, id, principal, source string) error {
	s.revokeMu.Lock()
	defer s.revokeMu.Unlock()
	return s.revokeBy(ctx, id, principal, source, false)
}

// RevokeKeepingOne is RevokeBy that refuses the last live device
// (ErrLastDevice). The console uses it for a tenant_admin: with the rule that
// a tenant_admin mints only while a device exists, a stolen owner session
// cannot empty the table and turn a further-device code into a first-device
// one. The count and the revoke run under one lock.
func (s *Service) RevokeKeepingOne(ctx context.Context, id, principal, source string) error {
	s.revokeMu.Lock()
	defer s.revokeMu.Unlock()
	return s.revokeBy(ctx, id, principal, source, true)
}

func (s *Service) revokeBy(ctx context.Context, id, principal, source string, keepOne bool) error {
	audit := s.auditWriter()
	devs, err := s.repo.ListDevices(ctx)
	if err != nil {
		return err
	}
	var dev *persistence.ApproverDeviceRow
	for i := range devs {
		if devs[i].ID == id {
			dev = &devs[i]
		}
	}
	switch {
	case dev == nil:
		return persistence.ErrNotFound
	case dev.RevokedAt != nil:
		return ErrAlreadyRevoked
	}
	if keepOne {
		live := 0
		for _, d := range devs {
			if d.RevokedAt == nil {
				live++
			}
		}
		if live <= 1 {
			return ErrLastDevice
		}
		if s.afterLiveCount != nil {
			s.afterLiveCount()
		}
	}
	if err := s.Revoke(ctx, id); err != nil {
		return err
	}
	if audit == nil {
		return ErrNoAudit
	}
	after, _ := json.Marshal(map[string]string{"label": dev.Label})
	if err := audit.Insert(ctx, &persistence.AdminAuditEntry{ID: persistence.GenerateID("admaud"), Timestamp: s.now(),
		Principal: principal, Source: source, Action: "device.revoke", Target: id, After: string(after)}); err != nil {
		// The revoke happened; say it went unrecorded (review 016f L1).
		return errors.Join(ErrNoAudit, err)
	}
	return nil
}

// Origin is the public origin the service uses in links ("" when none is
// configured); the console's pair page shows it (#763).
func (s *Service) Origin() string { return s.origin }
