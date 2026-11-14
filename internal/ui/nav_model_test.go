package ui

import (
	"html/template"
	"strings"
	"testing"
)

func TestNavHelpersRegistered(t *testing.T) {
	tmpl := template.New("t").Funcs(uiFuncMap())
	src := `{{$a := navAreaForPage "swarms"}}{{$a}}|{{range navModel}}{{.Key}} {{end}}`
	tmpl, err := tmpl.Parse(src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var sb strings.Builder
	if err := tmpl.Execute(&sb, nil); err != nil {
		t.Fatalf("execute: %v", err)
	}
	out := sb.String()
	if !strings.HasPrefix(out, "diagnostics|") { // swarms live under Diagnostics since #755
		t.Errorf("navAreaForPage not wired: %q", out)
	}
	if !strings.Contains(out, "manage") || !strings.Contains(out, "operator") {
		t.Errorf("navModel not wired: %q", out)
	}
}

func TestNavAreaForPage(t *testing.T) {
	// Navigation v2 (design §13.2, #755): every page maps to its new area.
	cases := map[string]string{
		"manage-overview":      "manage",
		"projects":             "manage",
		"integrations":         "manage",
		"admin-keys":           "manage",
		"admin-agents":         "manage",
		"admin-skills":         "manage",
		"reminders":            "manage",
		"insights":             "manage",
		"trading":              "manage",
		"my-account":           "manage",
		"spend":                "results",
		"results":              "results",
		"trends":               "results",
		"adoption":             "results",
		"live":                 "diagnostics",
		"diagnostics-overview": "diagnostics",
		"tasks":                "diagnostics",
		"executions":           "diagnostics",
		"swarms":               "diagnostics",
		"workflows":            "diagnostics",
		"memory":               "diagnostics",
		"quality":              "diagnostics",
		"audit":                "diagnostics",
		"admin":                "operator",
		"operator":             "operator",
		"regulatory":           "operator",
		"admin-control-plane":  "operator",
		"dashboard":            "home",  // the Home door highlights on its page (#755)
		"inbox":                "needs", // the Needs you door
		"mcp":                  "",      // removed from nav (2026-07-08 dedupe → hub MCP tab)
		"":                     "",
		"nonsense":             "",
	}
	for page, want := range cases {
		if got := navAreaForPage(page); got != want {
			t.Errorf("navAreaForPage(%q) = %q, want %q", page, got, want)
		}
	}
}

func TestNavModelContract(t *testing.T) {
	m := navModel()
	byKey := map[string]navAreaDef{}
	for _, a := range m {
		byKey[a.Key] = a
	}
	order := func(area string, want ...string) {
		t.Helper()
		var got []string
		for _, d := range byKey[area].Dests {
			got = append(got, d.Key)
			if d.Href == "" || d.Label == "" || d.Icon == "" {
				t.Errorf("%s dest %q missing Href/Label/Icon", area, d.Key)
			}
		}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("%s dests = %v, want %v", area, got, want)
		}
	}
	order("results", "results", "spend", "trends", "adoption")
	order("manage", "manage-overview", "projects", "integrations", "admin-keys", "admin-agents", "admin-skills", "reminders", "insights", "trading", "my-account")
	order("diagnostics", "diagnostics-overview", "live", "tasks", "executions", "swarms", "workflows", "memory", "quality", "audit")
	order("operator", "admin", "operator", "regulatory", "admin-control-plane")
	// Diagnostics lands on its overview (#756).
	if byKey["diagnostics"].Href != "/ui/diagnostics" {
		t.Errorf("diagnostics Href = %q, want /ui/diagnostics", byKey["diagnostics"].Href)
	}
	if !byKey["operator"].AdminOnly {
		t.Error("the operator console must be AdminOnly")
	}
	// Trading is wired under Manage (it is an assistant's settings).
	for _, d := range byKey["manage"].Dests {
		if d.Key == "trading" && (d.Href != "/ui/trading" || d.Icon != "navIconTrading" || d.Cap != "trading") {
			t.Errorf("trading dest mis-wired: %+v", d)
		}
	}
}

// navGate adapts a static bool to the func() bool predicate navModelFunc and
// navModelForPage take. Production passes Server.tradingNavEnabled, which is
// re-evaluated per render (see trading_nav_gate_test.go).
func navGate(v bool) func() bool { return func() bool { return v } }

// TestNavModelCommunityHidesTrading pins the 2026-06-29 fix: trading is an
// Enterprise-only capability (the /trading route 404s on CE via
// WithTradingEnabled), so a false gate must omit the Trading destination
// entirely — otherwise CE renders a nav link to a 404. A true gate keeps it,
// and the canonical navModel() is unchanged.
func TestNavModelCommunityHidesTrading(t *testing.T) {
	// Community: no "trading" dest anywhere.
	for _, a := range navModelFunc(navGate(false))() {
		for _, d := range a.Dests {
			if d.Key == "trading" {
				t.Fatalf("navModelFunc(navGate(false)) must omit the Trading dest; found it under %q", a.Key)
			}
		}
	}
	// Enterprise keeps it (guards against an over-eager filter).
	found := false
	for _, a := range navModelFunc(navGate(true))() {
		for _, d := range a.Dests {
			if d.Key == "trading" {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("navModelFunc(navGate(true)) must keep the Trading dest")
	}
	// Canonical navModel() is edition-agnostic — always full.
	if !navModelHasTrading(navModel()) {
		t.Fatal("navModel() must include the Trading dest (gating happens in navModelFunc)")
	}

	// Render-level: the CE-wired navModel func emits no trading entry; sibling
	// Insight dests (spend) still render.
	fm := uiFuncMap()
	fm["navModel"] = navModelFunc(navGate(false))
	tmpl, err := template.New("t").Funcs(fm).
		Parse(`{{range navModel}}{{range .Dests}}{{.Key}} {{end}}{{end}}`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var sb strings.Builder
	if err := tmpl.Execute(&sb, nil); err != nil {
		t.Fatalf("execute: %v", err)
	}
	out := sb.String()
	if strings.Contains(out, "trading") {
		t.Errorf("CE nav render must not contain trading: %q", out)
	}
	if !strings.Contains(out, "spend") {
		t.Errorf("CE nav render should still contain sibling Insight dests: %q", out)
	}
}

// TestNavModel_InboxRelabelledMyRequests pins the task 4.4 relabel
// (design §5.7): the "Needs you" nav destination becomes "My requests"
// now that the inbox is the non-admin default home and carries the
// broader "Your requests" list, not just what needs attention.
// The inbox is the Needs you door since #755 (it was the "My requests"
// destination under Steer), and the attention badge rides on the door.
func TestNavModel_InboxIsTheNeedsYouDoor(t *testing.T) {
	for _, a := range navModel() {
		if a.Key == "needs" {
			if a.Href != "/ui/inbox" || a.Label != "Needs you" {
				t.Errorf("needs door = %+v", a)
			}
			return
		}
	}
	t.Fatal("no Needs you door")
}

// fakeNavCounter is a minimal navAttentionCounter for testing
// navModelForPage without depending on InboxData.
type fakeNavCounter struct{ n int }

func (f fakeNavCounter) NavAttentionCount() int { return f.n }

func needsBadge(m []navAreaDef) int {
	for _, a := range m {
		if a.Key == "needs" {
			return a.Badge
		}
	}
	return -1
}

func TestNavModelForPage_BadgesNeedsYouWhenCounterPositive(t *testing.T) {
	if got := needsBadge(navModelForPage(navGate(true))(fakeNavCounter{n: 3})); got != 3 {
		t.Errorf("Needs you Badge = %d, want 3", got)
	}
}

// A page whose Data doesn't implement navAttentionCounter renders no badge.
func TestNavModelForPage_NoCounterNoBadge(t *testing.T) {
	for _, data := range []any{nil, "a plain string", struct{ Foo string }{Foo: "bar"}} {
		if got := needsBadge(navModelForPage(navGate(true))(data)); got != 0 {
			t.Errorf("data=%#v: Needs you Badge = %d, want 0", data, got)
		}
	}
}

// A zero (or negative) count must not render "(0)".
func TestNavModelForPage_ZeroCounterNoBadge(t *testing.T) {
	if got := needsBadge(navModelForPage(navGate(true))(fakeNavCounter{n: 0})); got != 0 {
		t.Errorf("Needs you Badge = %d, want 0 for a zero count", got)
	}
}

// The counter must never leak onto another area or destination.
func TestNavModelForPage_OnlyNeedsYouBadged(t *testing.T) {
	for _, a := range navModelForPage(navGate(true))(fakeNavCounter{n: 5}) {
		if a.Key != "needs" && a.Badge != 0 {
			t.Errorf("area %q got Badge %d", a.Key, a.Badge)
		}
	}
}

// TestInboxData_NavAttentionCount — InboxData implements
// navAttentionCounter via its own Count field.
func TestInboxData_NavAttentionCount(t *testing.T) {
	d := InboxData{Count: 7}
	var counter navAttentionCounter = d
	if got := counter.NavAttentionCount(); got != 7 {
		t.Errorf("NavAttentionCount() = %d, want 7", got)
	}
}

func navModelHasTrading(m []navAreaDef) bool {
	for _, a := range m {
		for _, d := range a.Dests {
			if d.Key == "trading" {
				return true
			}
		}
	}
	return false
}

// TestNavModel_MobileLabelsFitTabBar is the 2026-07-10 mobile-nav-overflow
// regression: the < md bottom bar renders up to 8 flex-1 cells (Home + 6
// areas + Sign out), which leaves ~47px per cell on a 375px phone. A
// 10px-font label wider than ~7 characters ("Orchestration",
// "Integrations") overflows its cell, so every area must expose a mobile
// label that fits; the template renders MobileLabel(), not Label, in the
// tab bar.
func TestNavModel_MobileLabelsFitTabBar(t *testing.T) {
	const maxMobileLabelLen = 7
	for _, a := range navModel() {
		got := a.MobileLabel()
		if got == "" {
			t.Errorf("area %q: MobileLabel() is empty", a.Key)
		}
		if len(got) > maxMobileLabelLen {
			t.Errorf("area %q: MobileLabel() = %q (%d chars), want <= %d — it overflows the mobile tab bar",
				a.Key, got, len(got), maxMobileLabelLen)
		}
	}
}

// MobileLabel falls back to Label when no Short label is set, so a future
// area with an already-short Label needs no extra field.
func TestNavAreaDef_MobileLabelFallsBackToLabel(t *testing.T) {
	a := navAreaDef{Label: "Steer"}
	if got := a.MobileLabel(); got != "Steer" {
		t.Errorf("MobileLabel() = %q, want the Label fallback %q", got, "Steer")
	}
	a = navAreaDef{Label: "Orchestration", Short: "Tasks"}
	if got := a.MobileLabel(); got != "Tasks" {
		t.Errorf("MobileLabel() = %q, want the Short override %q", got, "Tasks")
	}
}

// TestNav_IntegrationsLivesBesideControlPlane pins the 2026-10-03 move
// (operator: "move integrations from the connect menu to control plane, to
// declutter navigation"). The Integrations destination leaves its own
// one-entry top-level area (the rail's "Integrations", the mobile bar's
// "Connect") and sits in the Admin area directly after Control plane. The
// URL is unchanged; only the grouping moves.
func TestNav_IntegrationsLivesInManage(t *testing.T) {
	m := navModel()
	for _, a := range m {
		if a.Key == "integrations" || a.MobileLabel() == "Connect" {
			t.Errorf("the Connect/Integrations area still exists (%q) — it should be gone", a.Key)
		}
	}
	// Since #755 Integrations sits in Manage (connections an owner
	// configures), not beside Control plane in the operator console.
	if area := navDestArea(t, "integrations"); area.Key != "manage" {
		t.Fatalf("Integrations is in the %q area, want manage", area.Key)
	}
	d := navDestByKey(t, "integrations")
	if d.Href != "/ui/integrations" || d.Label != "Integrations" || d.Icon != "navIconIntegrations" {
		t.Errorf("integrations dest mis-wired: %+v", d)
	}
	// AdminOnly since #755: before it the admin-only Admin area hid it from
	// project users and the owner; Manage is not admin-only, so the entry
	// carries the flag itself (design §13.2) and no audience's nav changes.
	if !d.AdminOnly {
		t.Error("integrations dest must be AdminOnly in Manage")
	}
}
