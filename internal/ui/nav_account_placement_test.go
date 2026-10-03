package ui

import (
	"os"
	"strings"
	"testing"
)

// Operator, 2026-09-16: "my account should probably not be under steer but the
// admin menu — where the control plane, and access keys menu items are — also
// the key is not the best icon for it, should probably be a user icon […] the
// accounts icon is not good, should be something similar but at the same time
// distinct from my account, maybe one vs multiple user outlines."
//
// Steer is live control of running work (Live, My requests). A page listing
// the identities that resolve to you is not that; it belongs with the other
// identity surfaces, which is where an operator goes looking for it. The Admin
// AREA is the right home on both editions: Keys & access and Control plane are
// Community features reached from it, and allUICallersAdmin admits every
// caller on Community and on any auth-off deployment.

func navDestArea(t *testing.T, key string) navAreaDef {
	t.Helper()
	for _, a := range navModel() {
		for _, d := range a.Dests {
			if d.Key == key {
				return a
			}
		}
	}
	t.Fatalf("nav dest %q is in no area at all", key)
	return navAreaDef{}
}

func navDestByKey(t *testing.T, key string) navDest {
	t.Helper()
	for _, a := range navModel() {
		for _, d := range a.Dests {
			if d.Key == key {
				return d
			}
		}
	}
	t.Fatalf("nav dest %q not found", key)
	return navDest{}
}

func TestNav_MyAccountLivesWithTheOtherIdentitySurfaces(t *testing.T) {
	area := navDestArea(t, "my-account")
	if area.Key != "admin" {
		t.Errorf("My account is in the %q area, want %q — Steer is live control of running work", area.Key, "admin")
	}
	// Same area as the surfaces the operator named, so the grouping is the
	// property under test rather than one entry's coordinates.
	for _, sibling := range []string{"admin-keys", "admin-control-plane"} {
		if got := navDestArea(t, sibling).Key; got != area.Key {
			t.Errorf("%s is in area %q but My account is in %q", sibling, got, area.Key)
		}
	}
}

// The dest itself stays un-flagged: it is the page for the people who are NOT
// admins. Flagging it would hide it even where the area is visible — on
// Community, and on every auth-off deployment.
func TestNav_MyAccountIsNotItselfAdminOnly(t *testing.T) {
	if navDestByKey(t, "my-account").AdminOnly {
		t.Error("My account is marked AdminOnly; it is the one screen a non-admin needs")
	}
}

// Three entries shared navIconKey — My account, Accounts and Keys & access —
// so the rail gave the operator no way to tell them apart. A key means a key.
func TestNav_IdentitySurfacesDoNotShareAnIcon(t *testing.T) {
	icons := map[string]string{}
	for _, key := range []string{"my-account", "operator", "admin-keys"} {
		icon := navDestByKey(t, key).Icon
		if icon == "" {
			t.Fatalf("%s has no icon", key)
		}
		if other, dup := icons[icon]; dup {
			t.Errorf("%s and %s both render %s; they are different things", key, other, icon)
		}
		icons[icon] = key
	}
	if got := navDestByKey(t, "my-account").Icon; got != "navIconUser" {
		t.Errorf("My account icon = %q, want navIconUser (one person — yourself)", got)
	}
	if got := navDestByKey(t, "operator").Icon; got != "navIconUsers" {
		t.Errorf("Accounts icon = %q, want navIconUsers (several people — everyone else)", got)
	}
}

// Assistants reused Accounts' two-people icon, so the rail read it as a list
// of human users (operator, 2026-10-02). It has its own robot icon, which is
// defined, and no other entry renders it.
func TestNav_AssistantsHaveTheirOwnRobotIcon(t *testing.T) {
	if got := navDestByKey(t, "admin-agents").Icon; got != "navIconRobot" {
		t.Fatalf("Assistants icon = %q, want navIconRobot (software acting for a person, not people)", got)
	}
	for _, key := range []string{"my-account", "operator", "admin-keys"} {
		if navDestByKey(t, key).Icon == "navIconRobot" {
			t.Errorf("%s renders navIconRobot; it belongs to Assistants", key)
		}
	}
	b, err := os.ReadFile("templates/_partials.html")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `{{define "navIconRobot"}}`) {
		t.Fatal("navIconRobot is not defined in templates/_partials.html")
	}
}
