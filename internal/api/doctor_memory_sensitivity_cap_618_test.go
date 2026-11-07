package api

// GitHub #618 C4 (firewall design, "Amendment 2026-11-06 (#618)"): doctor
// states what each capped role can retrieve, by tier, and warns when a cap
// admits nothing.

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func sensitivityCapCheck(roles []SensitivityCapRole, err error) DoctorCheck {
	h := &DoctorHandlers{}
	h.SetSensitivityCapRoles(func(context.Context) ([]SensitivityCapRole, error) { return roles, err })
	return h.checkMemorySensitivityCap(context.Background())
}

func TestMemorySensitivityCap_UnwiredOrUncappedIsSkipped_618(t *testing.T) {
	if c := (&DoctorHandlers{}).checkMemorySensitivityCap(context.Background()); c.Name != "memory_sensitivity_cap" || c.Status != "SKIPPED" {
		t.Fatalf("unwired: got %s %s", c.Name, c.Status)
	}
	if c := sensitivityCapCheck(nil, nil); c.Status != "SKIPPED" {
		t.Fatalf("no capped role: got %s %q", c.Status, c.Message)
	}
}

func TestMemorySensitivityCap_OKStatesTheCounts_618(t *testing.T) {
	c := sensitivityCapCheck([]SensitivityCapRole{{ProjectID: "vornik-architect", Role: "consultant", MaxSensitivity: "public",
		DerivedPublic: 1219, Unlabelled: 3258, Admitted: 1219}}, nil)
	if c.Status != "OK" || len(c.Items) != 1 {
		t.Fatalf("got %s %v", c.Status, c.Items)
	}
	for _, want := range []string{"vornik-architect/consultant", "max public", "admits 1219", "derived public 1219", "unlabelled 3258"} {
		if !strings.Contains(c.Items[0], want) {
			t.Errorf("item %q lacks %q", c.Items[0], want)
		}
	}
}

func TestMemorySensitivityCap_WarnsWhenACapAdmitsNothing_618(t *testing.T) {
	c := sensitivityCapCheck([]SensitivityCapRole{
		{ProjectID: "p", Role: "ok", MaxSensitivity: "public", Admitted: 3},
		{ProjectID: "p", Role: "empty", MaxSensitivity: "public", Unlabelled: 40},
	}, nil)
	if c.Status != "WARNING" || !strings.Contains(c.Message, "1 role(s) admit no chunk") {
		t.Fatalf("got %s %q", c.Status, c.Message)
	}
}

func TestMemorySensitivityCap_ReadErrorsWarn_618(t *testing.T) {
	if c := sensitivityCapCheck(nil, errors.New("registry down")); c.Status != "WARNING" || !strings.Contains(c.Message, "registry down") {
		t.Fatalf("list error: got %s %q", c.Status, c.Message)
	}
	c := sensitivityCapCheck([]SensitivityCapRole{{ProjectID: "p", Role: "r", MaxSensitivity: "public", Err: errors.New("db down")}}, nil)
	if c.Status != "WARNING" || !strings.Contains(c.Items[0], "db down") {
		t.Fatalf("count error: got %s %v", c.Status, c.Items)
	}
}
