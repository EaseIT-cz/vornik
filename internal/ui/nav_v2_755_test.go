package ui

// Navigation v2 (ui-design-refresh-design.md §13.2, #755): three doors and
// four layers, and every audience keeps exactly today's nav. Before #755
// the rail had five engineering areas (Steer, Orchestration, Memory,
// Insight, Admin) and no Ask entry; the home page was only the logo.

import (
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
)

// todayDest is a destination as it stood before #755 (measured at
// 91492f271, design §13.1): its URL and gating.
type todayDest struct {
	href      string
	adminOnly bool
	cap       string
	ownerHref string
	adminArea bool // lived in the admin-only Admin area
}

var todayDests = map[string]todayDest{
	"live":                {href: "/ui/live"},
	"inbox":               {href: "/ui/inbox"},
	"tasks":               {href: "/ui/tasks"},
	"projects":            {href: "/ui/projects"},
	"swarms":              {href: "/ui/swarms", adminOnly: true},
	"workflows":           {href: "/ui/workflows", adminOnly: true},
	"executions":          {href: "/ui/executions"},
	"memory":              {href: "/ui/memory"},
	"reminders":           {href: "/ui/reminders"},
	"spend":               {href: "/ui/spend"},
	"trends":              {href: "/ui/insights/trends"},
	"quality":             {href: "/ui/insights/quality"},
	"insights":            {href: "/ui/insights/tool-budget"},
	"adoption":            {href: "/ui/insights/adoption"},
	"trading":             {href: "/ui/trading", cap: "trading"},
	"audit":               {href: "/ui/audit", adminOnly: true},
	"admin":               {href: "/ui/admin/", adminArea: true},
	"my-account":          {href: "/ui/account", ownerHref: "/ui/account", adminArea: true},
	"operator":            {href: "/ui/operator/accounts", adminOnly: true, adminArea: true},
	"regulatory":          {href: "/ui/operator/regulatory", adminOnly: true, adminArea: true},
	"admin-skills":        {href: "/ui/admin/skills", adminArea: true},
	"admin-control-plane": {href: "/ui/admin/control-plane", adminArea: true},
	"integrations":        {href: "/ui/integrations", adminArea: true},
	"admin-keys":          {href: "/ui/admin/keys", ownerHref: "/ui/credentials", adminArea: true},
	"admin-agents":        {href: "/ui/admin/agents", adminArea: true},
	// Added by #756 (design §14.2): the Diagnostics overview.
	"diagnostics-overview": {href: "/ui/diagnostics"},
	// Added by #757 (design §15.2): the Results page.
	"results": {href: "/ui/results"},
	// Added by #759 (design §17.2 M1): the Manage overview.
	"manage-overview": {href: "/ui/manage"},
}

func TestNavV2_DoorsLeadTheRail_755(t *testing.T) {
	m := navModel()
	want := []string{"home", "ask", "needs", "results", "manage", "diagnostics", "operator"}
	var got []string
	for _, a := range m {
		got = append(got, a.Key)
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("areas = %v, want %v", got, want)
	}
	hrefs := map[string]string{"home": "/ui/", "ask": "/ui/ask", "needs": "/ui/inbox", "results": "/ui/results"}
	for _, a := range m[:4] {
		if a.AdminOnly {
			t.Errorf("door %q is admin-only", a.Key)
		}
		if a.Href != hrefs[a.Key] {
			t.Errorf("door %q lands on %q, want %q", a.Key, a.Href, hrefs[a.Key])
		}
	}
	for _, a := range m[:3] {
		if len(a.Dests) != 0 {
			t.Errorf("door %q has a panel; Home, Ask and Needs you are plain links", a.Key)
		}
	}
	if !m[6].AdminOnly {
		t.Error("the operator console must be admin-only")
	}
	if m[4].AdminOnly || m[5].AdminOnly {
		t.Error("Manage and Diagnostics are not admin-only areas; their entries carry the gating")
	}
}

// Every destination of today keeps its URL and gating; entries leaving the
// admin-only Admin area for Manage gain AdminOnly so nobody's view changes.
func TestNavV2_EveryDestinationKeepsItsGating_755(t *testing.T) {
	seen := map[string]int{}
	manageMoves := map[string]bool{"my-account": true, "admin-keys": true, "integrations": true, "admin-skills": true, "admin-agents": true}
	for _, a := range navModel() {
		for _, d := range a.Dests {
			seen[d.Key]++
			w, ok := todayDests[d.Key]
			if !ok {
				t.Errorf("new destination %q in %q: list it in todayDests or the design", d.Key, a.Key)
				continue
			}
			if d.Href != w.href || d.Cap != w.cap || d.OwnerHref != w.ownerHref {
				t.Errorf("%q changed: href %q cap %q ownerHref %q", d.Key, d.Href, d.Cap, d.OwnerHref)
			}
			wantAdmin := w.adminOnly || (w.adminArea && a.Key == "manage" && d.Key != "my-account")
			if w.adminArea && a.Key == "operator" {
				wantAdmin = w.adminOnly // inside an admin-only area, as today
			}
			if d.AdminOnly != wantAdmin {
				t.Errorf("%q in %q: AdminOnly %v, want %v", d.Key, a.Key, d.AdminOnly, wantAdmin)
			}
			if manageMoves[d.Key] && a.Key != "manage" {
				t.Errorf("%q should be in Manage, is in %q", d.Key, a.Key)
			}
		}
	}
	for k := range todayDests {
		if k == "inbox" {
			if seen[k] != 0 {
				t.Error("the inbox is the Needs you door now, not a panel entry")
			}
			continue
		}
		if seen[k] != 1 {
			t.Errorf("destination %q appears %d times, want once", k, seen[k])
		}
	}
}

// visibleKeys renders the nav for one audience and returns the destination
// and door keys it can see: what is hidden by `hidden` for a non-admin, and
// what the owner marker script hides or unhides, applied here.
func visibleKeys(m []navAreaDef, audience string) []string {
	var out []string
	add := func(k string) { out = append(out, k) }
	for _, a := range m {
		areaVisible := !a.AdminOnly || audience == "admin" || (audience == "owner" && a.OwnerHref != "")
		if len(a.Dests) == 0 && areaVisible {
			add("door:" + a.Key)
		}
		for _, d := range a.Dests {
			o := navOwner(a, d)
			switch audience {
			case "admin":
				if areaVisible {
					add(d.Key)
				}
			case "owner":
				if o.Href != "" {
					add(d.Key) // the marker unhides data-owner-href anywhere
				} else if areaVisible && !d.AdminOnly && !o.Hide {
					add(d.Key)
				}
			default:
				if areaVisible && !d.AdminOnly {
					add(d.Key)
				}
			}
		}
	}
	sort.Strings(out)
	return out
}

func TestNavV2_EveryAudienceKeepsItsNav_755(t *testing.T) {
	m := navModel()
	// My account is the one deliberate gain: open to project users, as
	// my_account_test requires; the old admin-only area hid it.
	projectUser := []string{"adoption", "diagnostics-overview", "executions", "insights", "live", "manage-overview", "memory", "my-account", "projects", "quality", "reminders", "results", "spend", "tasks", "trading", "trends"}
	owner := append(append([]string{}, projectUser...), "admin-keys")
	var admin []string
	for k := range todayDests {
		if k != "inbox" {
			admin = append(admin, k)
		}
	}
	doors := []string{"door:ask", "door:home", "door:needs"}
	for _, tc := range []struct {
		audience string
		want     []string
	}{
		{"project user", append(append([]string{}, projectUser...), doors...)},
		{"owner", append(append([]string{}, owner...), doors...)},
		{"admin", append(append([]string{}, admin...), doors...)},
	} {
		sort.Strings(tc.want)
		got := visibleKeys(m, tc.audience)
		if strings.Join(got, ",") != strings.Join(tc.want, ",") {
			t.Errorf("%s sees %v,\nwant %v", tc.audience, got, tc.want)
		}
	}
	// The owner opens Keys & access at the credentials page, as today.
	for _, a := range m {
		for _, d := range a.Dests {
			if d.Key == "admin-keys" && navOwner(a, d).Href != "/ui/credentials" {
				t.Errorf("owner's Keys & access goes to %q", navOwner(a, d).Href)
			}
		}
	}
}

func TestNavV2_NeedsYouCarriesTheBadge_755(t *testing.T) {
	f := navModelForPage(func() bool { return true })
	for _, tc := range []struct {
		count int
		want  int
	}{{3, 3}, {0, 0}} {
		var needs navAreaDef
		for _, a := range f(inboxCounterStub(tc.count)) {
			if a.Key == "needs" {
				needs = a
			}
		}
		if needs.Badge != tc.want {
			t.Errorf("count %d: Needs you badge %d, want %d", tc.count, needs.Badge, tc.want)
		}
	}
}

type inboxCounterStub int

func (c inboxCounterStub) NavAttentionCount() int { return int(c) }

func TestNavV2_AskRedirects_755(t *testing.T) {
	for _, tc := range []struct {
		name       string
		dispatcher string
		canOpen    bool
		want       string
	}{
		{"dispatcher set and open", "assistant", true, "/ui/projects/assistant/chat"},
		{"dispatcher set, not this session's", "assistant", false, "/ui/projects"},
		{"no dispatcher", "", true, "/ui/projects"},
		{"a dispatcher id that is not a plain id", "../../evil", true, "/ui/projects"},
		{"a query in the id", "assistant?next=https://evil.example", true, "/ui/projects"},
		{"a fragment in the id", "assistant#x", true, "/ui/projects"},
		{"a percent escape in the id", "assistant%2F..", true, "/ui/projects"},
	} {
		h := askHandler(func() string { return tc.dispatcher }, func(*http.Request, string) bool { return tc.canOpen })
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest(http.MethodGet, "/ui/ask", nil))
		loc := rec.Header().Get("Location")
		if rec.Code != http.StatusSeeOther || loc != tc.want {
			t.Errorf("%s: %d %q, want 303 %q", tc.name, rec.Code, loc, tc.want)
		}
		if !strings.HasPrefix(loc, "/ui/") {
			t.Errorf("%s: Location %q leaves the console", tc.name, loc)
		}
	}
}

// The phone bar: Home, Ask and Needs you are links; areas with
// destinations are drawer buttons (design §13.2).
func TestNavV2_PhoneBarDoorsAreLinks_755(t *testing.T) {
	html := renderTemplateString(t, "nav", navFixture{CurrentPage: "tasks", IsAdmin: true, IsSession: true})
	for _, k := range []string{"home", "ask", "needs"} {
		if !strings.Contains(html, `data-nav-door="`+k+`"`) {
			t.Errorf("phone bar: %q should be a link (data-nav-door)", k)
		}
		if strings.Contains(html, `data-nav-tab="`+k+`"`) || strings.Contains(html, `data-panel="`+k+`"`) {
			t.Errorf("%q should have no drawer or panel", k)
		}
	}
	// Results is a drawer tab; since the phone bar's More sheet (design §18)
	// the other areas are groups in More, not tabs of their own.
	if !strings.Contains(html, `data-nav-tab="results"`) || !strings.Contains(html, `data-nav-drawer="results"`) {
		t.Error("phone bar: results should be a drawer button with its drawer")
	}
	for _, k := range []string{"manage", "diagnostics", "operator"} {
		if !strings.Contains(html, `data-nav-more="`+k+`"`) {
			t.Errorf("phone bar: %q should be a group in More", k)
		}
	}
	if strings.Contains(html, `aria-label="Dashboard"`) {
		t.Error("the hard-coded Home cell is gone; Home is the first door")
	}
}

// The Needs you count renders on the desktop rail and the phone door, and
// nothing renders for zero (review-20261113-293f).
func TestNavV2_BadgeRenders_755(t *testing.T) {
	tmpl := NewServer().templates
	for _, tc := range []struct {
		count int
		want  int
	}{{4, 2}, {0, 0}} {
		var b strings.Builder
		if err := tmpl.ExecuteTemplate(&b, "nav", InboxData{Count: tc.count, CurrentPage: "inbox"}); err != nil {
			t.Fatal(err)
		}
		html := b.String()
		if n := strings.Count(html, "rail-badge"); n != tc.want {
			t.Errorf("count %d: %d badges rendered, want %d (rail and phone door)", tc.count, n, tc.want)
		}
		if tc.count > 0 && !strings.Contains(html, "Needs you (4)") {
			t.Error("the badge count should reach the accessible name")
		}
	}
}
