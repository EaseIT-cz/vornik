package approverdevice

// GitHub #763 (design part 09, amendment #763, review 2d36 M3): pairing
// codes and revokes are audited in the service, for every caller (console,
// CLI, the phone's own revoke). Without an audit writer they refuse. A mint
// row never holds the code.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

func TestAudited763_StartPairingByRecordsNoCode(t *testing.T) {
	f := newFixture(t)
	if _, _, err := f.svc.StartPairingBy(context.Background(), "Phone", "ui:owner", "ui"); !errors.Is(err, ErrNoAudit) {
		t.Fatalf("no audit writer: err = %v, want ErrNoAudit", err)
	}
	a := &audit489{}
	f.svc.SetAudit(a)
	code, _, err := f.svc.StartPairingBy(context.Background(), "Phone", "ui:owner", "ui")
	if err != nil || len(code) != 8 {
		t.Fatalf("code %q err %v", code, err)
	}
	if len(a.rows) != 1 || a.rows[0].Action != "device.pair_code" || a.rows[0].Principal != "ui:owner" || a.rows[0].Source != "ui" {
		t.Fatalf("rows %+v", a.rows)
	}
	if strings.Contains(a.rows[0].After+a.rows[0].Target+a.rows[0].Before, code) || !strings.Contains(a.rows[0].After, "Phone") {
		t.Fatalf("the row must name the label and never the code: %+v", a.rows[0])
	}
}

func TestAudited763_RevokeBy(t *testing.T) {
	f := newFixture(t)
	f.firstDevice(t)
	plantDevice(t, f, "dev_tab", "Tablet", false)
	ctx := context.Background()
	// No audit writer: the revoke still happens (it removes privilege) and
	// the caller is told it went unrecorded.
	plantDevice(t, f, "dev_tab0", "Old tablet", false)
	if err := f.svc.RevokeBy(ctx, "dev_tab0", "cli:me", "cli"); !errors.Is(err, ErrNoAudit) {
		t.Fatalf("no audit writer: %v", err)
	}
	if devs, _ := f.svc.ListDevices(ctx); !revoked763(devs, "dev_tab0") {
		t.Fatal("a revoke was refused for want of an audit writer")
	}
	a := &audit489{}
	f.svc.SetAudit(a)
	if err := f.svc.RevokeBy(ctx, "dev_tab", "cli:me", "cli"); err != nil {
		t.Fatal(err)
	}
	if len(a.rows) != 1 || a.rows[0].Action != "device.revoke" || a.rows[0].Target != "dev_tab" || !strings.Contains(a.rows[0].After, "Tablet") {
		t.Fatalf("rows %+v", a.rows)
	}
	if err := f.svc.RevokeBy(ctx, "dev_tab", "cli:me", "cli"); !errors.Is(err, ErrAlreadyRevoked) {
		t.Fatalf("second revoke: %v", err)
	}
	if err := f.svc.RevokeBy(ctx, "dev_nope", "cli:me", "cli"); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("unknown: %v", err)
	}
	if len(a.rows) != 1 {
		t.Fatalf("a refused revoke wrote a row: %+v", a.rows)
	}
}

func revoked763(devs []persistence.ApproverDeviceRow, id string) bool {
	for _, d := range devs {
		if d.ID == id {
			return d.RevokedAt != nil
		}
	}
	return false
}

// #763 review 016f M1: the last live device is kept, atomically. The seam
// runs a second revoke between the first one's count and its revoke: with the
// lock it waits and is refused; without it both would revoke.
func TestAudited763_RevokeKeepingOneIsAtomic(t *testing.T) {
	f := newFixture(t)
	f.firstDevice(t)
	plantDevice(t, f, "dev_b", "B", false)
	f.svc.SetAudit(&safeAudit763{})
	ctx := context.Background()
	devs, _ := f.svc.ListDevices(ctx)
	var first string
	for _, d := range devs {
		if d.RevokedAt == nil && d.ID != "dev_b" {
			first = d.ID
		}
	}
	second := make(chan error, 1)
	var once sync.Once
	f.svc.afterLiveCount = func() {
		once.Do(func() {
			go func() { second <- f.svc.RevokeKeepingOne(ctx, "dev_b", "ui:owner", "ui") }()
			select { // the second revoke either finishes (no lock) or waits (lock)
			case err := <-second:
				second <- err
			case <-time.After(300 * time.Millisecond):
			}
		})
	}
	if err := f.svc.RevokeKeepingOne(ctx, first, "ui:owner", "ui"); err != nil {
		t.Fatalf("first revoke: %v", err)
	}
	if err := <-second; !errors.Is(err, ErrLastDevice) {
		t.Fatalf("second revoke: %v, want ErrLastDevice", err)
	}
	if n, _ := f.repo.CountActiveDevices(ctx); n != 1 {
		t.Fatalf("live after two interleaved revokes = %d, want 1", n)
	}
}

type safeAudit763 struct {
	mu   sync.Mutex
	rows int
}

func (a *safeAudit763) Insert(context.Context, *persistence.AdminAuditEntry) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.rows++
	return nil
}
func (a *safeAudit763) List(context.Context, persistence.AdminAuditFilter) ([]*persistence.AdminAuditEntry, error) {
	return nil, nil
}
