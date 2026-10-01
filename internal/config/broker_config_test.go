package config

import (
	"testing"
	"time"
)

// broker.writes — broker write-actions design (2026-09-29) D5: off by
// default, and an unknown value fails startup rather than falling through.
func TestBrokerWritesMode(t *testing.T) {
	for in, want := range map[string]string{"": "off", "off": "off", "on": "on"} {
		got, err := BrokerDaemonConfig{Writes: in}.WritesMode()
		if err != nil || got != want {
			t.Fatalf("%q -> %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := (BrokerDaemonConfig{Writes: "yes"}).WritesMode(); err == nil {
		t.Fatal("unknown broker.writes must be an error")
	}
}

func TestBrokerActionTimeout(t *testing.T) {
	if got, err := (BrokerDaemonConfig{}).EffectiveActionTimeout(); err != nil || got != 60*time.Second {
		t.Fatalf("default = %v, %v; want 60s", got, err)
	}
	if got, err := (BrokerDaemonConfig{ActionTimeout: "90s"}).EffectiveActionTimeout(); err != nil || got != 90*time.Second {
		t.Fatalf("90s -> %v, %v", got, err)
	}
	for _, bad := range []string{"soon", "0s", "-5s", "11m"} {
		if _, err := (BrokerDaemonConfig{ActionTimeout: bad}).EffectiveActionTimeout(); err == nil {
			t.Fatalf("%q must be refused", bad)
		}
	}
}
