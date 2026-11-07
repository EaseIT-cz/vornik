package repotest

import (
	"context"
	"errors"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// approverPushRepo is a driver's repository: devices and push subscriptions
// on one struct.
type approverPushRepo interface {
	persistence.ApproverDeviceRepository
	persistence.ApproverPushRepository
}

// RunApproverPushSuite pins approver devices' Web Push subscriptions on both
// drivers (GitHub #163, agent-administered design part 09 §9.3a B.3, B.9).
// repo is the repository on the driver's real, migrated schema, where the
// miss contract runs; every other subtest runs on a store from fresh(t),
// which holds no device, as in RunApproverDeviceSuite.
func RunApproverPushSuite(t *testing.T, repo persistence.ApproverPushRepository, fresh func(t *testing.T) persistence.ApproverDeviceRepository) {
	t.Helper()
	open := func(t *testing.T) (*testing.T, approverPushRepo, *approverHarness) {
		h := newApproverHarness(t, fresh)
		r, ok := h.repo.(approverPushRepo)
		if !ok {
			t.Fatalf("%T does not implement persistence.ApproverPushRepository", h.repo)
		}
		return t, r, h
	}
	t.Run("Miss_is_ErrNotFound", func(t *testing.T) {
		ctx := context.Background()
		AssertMiss(t, "ApproverPushRepository.GetPushSubscription", func() (*persistence.ApproverPushSubscriptionRow, error) {
			return repo.GetPushSubscription(ctx, "dev_absent_push")
		})
		if err := repo.DeletePushSubscription(ctx, "dev_absent_push"); err != nil {
			t.Fatalf("DeletePushSubscription is idempotent: %v", err)
		}
		if err := repo.RecordPushResult(ctx, "dev_absent_push", "sent 201", time.Now()); err != nil {
			t.Fatalf("RecordPushResult without a row: %v", err)
		}
	})
	t.Run("Upsert_get_replace_and_record", func(t *testing.T) { approverPushUpsert(open(t)) })
	t.Run("Unknown_or_revoked_device_is_refused", func(t *testing.T) { approverPushRefused(open(t)) })
	t.Run("Revoke_deletes_and_list_holds_only_active", func(t *testing.T) { approverPushRevoke(open(t)) })
}

// pushDevice enrolls device dev_<code>: the first through its pairing, a
// further one by completing its claim.
func pushDevice(t *testing.T, h *approverHarness, code string) persistence.ApproverDeviceRow {
	t.Helper()
	h.pairing(t, code)
	d := h.device("dev_" + code)
	if !h.redeem(t, code) {
		if err := h.repo.CompletePairing(h.ctx, "claim-"+code, d); err != nil {
			t.Fatalf("complete %s: %v", code, err)
		}
	}
	return d
}

func pushRow(device, endpoint string, at time.Time) persistence.ApproverPushSubscriptionRow {
	return persistence.ApproverPushSubscriptionRow{DeviceID: device, Endpoint: endpoint, P256DH: "p-" + endpoint, Auth: "a", CreatedAt: at}
}

func approverPushUpsert(t *testing.T, r approverPushRepo, h *approverHarness) {
	d := pushDevice(t, h, "push1")
	if err := r.UpsertPushSubscription(h.ctx, pushRow(d.ID, "https://fcm.googleapis.com/a", h.now)); err != nil {
		t.Fatal(err)
	}
	got, err := r.GetPushSubscription(h.ctx, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Endpoint != "https://fcm.googleapis.com/a" || got.P256DH != "p-https://fcm.googleapis.com/a" || got.Auth != "a" ||
		!got.CreatedAt.Equal(h.now) || got.LastSentAt != nil || got.LastStatus != "" {
		t.Fatalf("round trip: %+v", got)
	}
	sent := h.now.Add(time.Minute)
	if err := r.RecordPushResult(h.ctx, d.ID, "failed 403", sent); err != nil {
		t.Fatal(err)
	}
	got, _ = r.GetPushSubscription(h.ctx, d.ID)
	if got.LastSentAt == nil || !got.LastSentAt.Equal(sent) || got.LastStatus != "failed 403" {
		t.Fatalf("recorded: %+v", got)
	}
	// Subscribing again from one device leaves one row, reset (B.9).
	later := h.now.Add(time.Hour)
	if err := r.UpsertPushSubscription(h.ctx, pushRow(d.ID, "https://fcm.googleapis.com/b", later)); err != nil {
		t.Fatal(err)
	}
	got, _ = r.GetPushSubscription(h.ctx, d.ID)
	if got.Endpoint != "https://fcm.googleapis.com/b" || !got.CreatedAt.Equal(later) || got.LastSentAt != nil || got.LastStatus != "" {
		t.Fatalf("replaced: %+v", got)
	}
	// Two devices leave two rows.
	d2 := pushDevice(t, h, "push2")
	if err := r.UpsertPushSubscription(h.ctx, pushRow(d2.ID, "https://web.push.apple.com/c", h.now)); err != nil {
		t.Fatal(err)
	}
	list, err := r.ListActivePushSubscriptions(h.ctx)
	if err != nil || len(list) != 2 || list[0].DeviceID != "dev_push1" || list[1].DeviceID != "dev_push2" {
		t.Fatalf("list: %v %+v", err, list)
	}
	if err := r.DeletePushSubscription(h.ctx, d.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := r.GetPushSubscription(h.ctx, d.ID); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("deleted: %v", err)
	}
}

func approverPushRefused(t *testing.T, r approverPushRepo, h *approverHarness) {
	if err := r.UpsertPushSubscription(h.ctx, pushRow("dev_nobody", "https://fcm.googleapis.com/x", h.now)); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("unknown device: %v", err)
	}
	d := pushDevice(t, h, "push3")
	if err := r.RevokeDevice(h.ctx, d.ID, h.now); err != nil {
		t.Fatal(err)
	}
	if err := r.UpsertPushSubscription(h.ctx, pushRow(d.ID, "https://fcm.googleapis.com/x", h.now)); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("revoked device: %v", err)
	}
}

func approverPushRevoke(t *testing.T, r approverPushRepo, h *approverHarness) {
	a := pushDevice(t, h, "push4")
	b := pushDevice(t, h, "push5")
	for _, d := range []string{a.ID, b.ID} {
		if err := r.UpsertPushSubscription(h.ctx, pushRow(d, "https://fcm.googleapis.com/"+d, h.now)); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.RevokeDevice(h.ctx, a.ID, h.now); err != nil {
		t.Fatal(err)
	}
	if _, err := r.GetPushSubscription(h.ctx, a.ID); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("revoke must delete the subscription: %v", err)
	}
	list, err := r.ListActivePushSubscriptions(context.Background())
	if err != nil || len(list) != 1 || list[0].DeviceID != b.ID {
		t.Fatalf("active list: %v %+v", err, list)
	}
}
