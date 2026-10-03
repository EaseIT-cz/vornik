package config

import (
	"strings"
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

// Broker write-actions design, tier 2 revised item 9: the standing-grant
// bounds are daemon config, lower only. Above a ceiling is refused at load.
func TestBrokerStandingGrants_LowerOnly(t *testing.T) {
	d, u, l, err := (BrokerDaemonConfig{}).StandingGrants.Effective()
	if err != nil || d != 7 || u != 20 || l != 10 {
		t.Fatalf("defaults = %d %d %d %v, want 7 20 10", d, u, l, err)
	}
	d, u, l, err = (BrokerStandingGrantsConfig{MaxDays: 1, MaxUses: 5, MaxLivePerProject: 2}).Effective()
	if err != nil || d != 1 || u != 5 || l != 2 {
		t.Fatalf("lowered = %d %d %d %v", d, u, l, err)
	}
	for _, bad := range []BrokerStandingGrantsConfig{{MaxDays: 8}, {MaxUses: 21}, {MaxLivePerProject: 11}, {MaxDays: -1}} {
		if _, _, _, err := bad.Effective(); err == nil {
			t.Errorf("%+v accepted; the bounds may only be lowered", bad)
		}
	}
	c := DefaultConfig()
	c.API.AuthEnabled = false
	c.Broker.StandingGrants.MaxUses = 30
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "standing_grants") {
		t.Errorf("the loader accepted broker.standing_grants.max_uses above 20: %v", err)
	}
}
