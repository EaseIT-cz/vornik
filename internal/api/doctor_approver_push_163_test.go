package api

// GitHub #163 (agent-administered design part 09 §9.3a B.8): the
// approver_devices check counts devices with notifications on. With no alert
// channel, one such device is enough to report the requests pushed; a device
// whose last push failed is a warning naming it.

import (
	"context"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

type pushSubs struct {
	persistence.ApproverPushRepository
	rows []persistence.ApproverPushSubscriptionRow
}

func (p pushSubs) ListActivePushSubscriptions(context.Context) ([]persistence.ApproverPushSubscriptionRow, error) {
	return p.rows, nil
}

type labelledDevices struct {
	countDevices
	labels map[string]string
}

func (l labelledDevices) ListDevices(context.Context) ([]persistence.ApproverDeviceRow, error) {
	var out []persistence.ApproverDeviceRow
	for id, label := range l.labels {
		out = append(out, persistence.ApproverDeviceRow{ID: id, Label: label})
	}
	return out, nil
}

func TestCheckApproverDevices_WebPush_163(t *testing.T) {
	withAgent := func() *memAgentSecrets {
		m := &memAgentSecrets{}
		_ = m.Upsert(context.Background(), persistence.AgentSecretRow{Namespace: "hermes", Name: "X", Kind: "secret",
			Ciphertext: []byte{1}, Nonce: []byte{1}, CreatedAt: time.Now(), UpdatedAt: time.Now()})
		return m
	}
	devs := labelledDevices{countDevices{n: 2}, map[string]string{"dev_a": "Pixel", "dev_b": "iPhone"}}
	at := time.Now()
	cases := []struct {
		name     string
		push     bool
		rows     []persistence.ApproverPushSubscriptionRow
		status   string
		mentions []string
	}{
		{"no channel, no device push", false, nil, "WARNING", []string{"not pushed"}},
		{"no channel, one device push", false, []persistence.ApproverPushSubscriptionRow{{DeviceID: "dev_a", LastStatus: "sent 201", LastSentAt: &at}},
			"OK", []string{"pushed to 1 device(s)"}},
		{"channel and a device push", true, []persistence.ApproverPushSubscriptionRow{{DeviceID: "dev_a"}},
			"OK", []string{"requests are pushed", "1 device(s) with notifications on"}},
		{"a failed device push", true, []persistence.ApproverPushSubscriptionRow{{DeviceID: "dev_b", LastStatus: "failed 403", LastSentAt: &at}},
			"WARNING", []string{"iPhone", "failed 403"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := &DoctorHandlers{}
			h.SetAgentSecretStore(withAgent(), "")
			h.SetApproverDevices(devs, c.push)
			h.SetApproverPush(pushSubs{rows: c.rows})
			got := h.checkApproverDevices(context.Background(), false)
			if got.Status != c.status {
				t.Fatalf("got %s %q, want %s", got.Status, got.Message, c.status)
			}
			for _, m := range c.mentions {
				if !strings.Contains(got.Message, m) {
					t.Fatalf("message %q lacks %q", got.Message, m)
				}
			}
		})
	}
}
