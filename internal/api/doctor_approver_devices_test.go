package api

import (
	"context"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// countDevices answers only CountActiveDevices; the check reads nothing else.
type countDevices struct {
	persistence.ApproverDeviceRepository
	n int
}

func (c countDevices) CountActiveDevices(context.Context) (int, error) { return c.n, nil }

func TestCheckApproverDevices(t *testing.T) {
	withAgent := func() *memAgentSecrets {
		m := &memAgentSecrets{}
		_ = m.Upsert(context.Background(), persistence.AgentSecretRow{Namespace: "hermes", Name: "X", Kind: "secret",
			Ciphertext: []byte{1}, Nonce: []byte{1}, CreatedAt: time.Now(), UpdatedAt: time.Now()})
		return m
	}
	cases := []struct {
		name     string
		devices  persistence.ApproverDeviceRepository
		secrets  *memAgentSecrets
		push     bool
		status   string
		mentions string
	}{
		{"not wired", nil, nil, false, "SKIPPED", "not wired"},
		{"no agent namespaces", countDevices{n: 0}, &memAgentSecrets{}, false, "OK", "no agent namespaces"},
		{"agent, no device", countDevices{n: 0}, withAgent(), true, "WARNING", "pair one"},
		{"agent, device, no push", countDevices{n: 1}, withAgent(), false, "WARNING", "not pushed"},
		{"agent, device, push", countDevices{n: 2}, withAgent(), true, "OK", "2 active"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := &DoctorHandlers{}
			if c.secrets != nil {
				h.SetAgentSecretStore(c.secrets, "")
			}
			if c.devices != nil {
				h.SetApproverDevices(c.devices, c.push)
			}
			got := h.checkApproverDevices(context.Background(), false)
			if got.Status != c.status || !strings.Contains(got.Message, c.mentions) {
				t.Fatalf("got %s %q, want %s mentioning %q", got.Status, got.Message, c.status, c.mentions)
			}
		})
	}
}
