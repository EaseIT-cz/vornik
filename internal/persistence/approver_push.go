package persistence

import (
	"context"
	"time"
)

// ApproverPushSubscriptionRow is one approver device's Web Push subscription
// (GitHub #163, agent-administered design part 09 §9.3a B.3): one per
// device, since a device is one browser. Endpoint, P256DH and Auth are the
// browser's PushSubscription, already checked (internal/webpush).
type ApproverPushSubscriptionRow struct {
	DeviceID  string
	Endpoint  string
	P256DH    string
	Auth      string
	CreatedAt time.Time
	// LastSentAt and LastStatus record the last push: nil and "" before
	// the first; LastStatus is an outcome and the HTTP status, such as
	// "sent 201" or "failed 403".
	LastSentAt *time.Time
	LastStatus string
}

// ApproverPushRepository stores the subscriptions. The same driver struct
// implements ApproverDeviceRepository; RevokeDevice deletes the device's
// subscription in its transaction.
type ApproverPushRepository interface {
	// UpsertPushSubscription stores or replaces the device's subscription,
	// only while the device is unrevoked; otherwise ErrNotFound. A replaced
	// row restarts its LastSentAt and LastStatus.
	UpsertPushSubscription(ctx context.Context, row ApproverPushSubscriptionRow) error
	// GetPushSubscription returns ErrNotFound when the device has none.
	GetPushSubscription(ctx context.Context, deviceID string) (*ApproverPushSubscriptionRow, error)
	// DeletePushSubscription is idempotent.
	DeletePushSubscription(ctx context.Context, deviceID string) error
	// ListActivePushSubscriptions returns the subscriptions of unrevoked
	// devices, by device id.
	ListActivePushSubscriptions(ctx context.Context) ([]ApproverPushSubscriptionRow, error)
	// RecordPushResult sets the last push's time and status; a device
	// without a subscription is not an error.
	RecordPushResult(ctx context.Context, deviceID, status string, at time.Time) error
}
