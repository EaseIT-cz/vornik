package ui

// navDest is one destination inside a nav area's contextual panel.
// Key matches the page's CurrentPage token so the panel can mark the
// active entry with the same value handlers already set today.
type navDest struct {
	Key   string
	Label string
	Href  string
	Icon  string // template name of the inline SVG, e.g. "navIconSwarms"
	// AdminOnly hides this destination from non-admins, mirroring the
	// area-level flag. Set for daemon-global / cross-project surfaces a
	// project-scoped RoleUser session is denied server-side
	// (sessionUserGlobalAuthoringPrefixes) — without it those users saw
	// nav links that 403 on click.
	AdminOnly bool
	// Cap names an optional capability this destination requires (e.g.
	// "trading"). Rendered as data-cap; the nav JS hides it when the
	// login caps cookie is present and omits the capability (fail-open:
	// no cookie → shown). Used for surfaces that are harmless-but-
	// irrelevant rather than access-denied (Trading degrades gracefully).
	Cap string
	// OwnerHref is where this entry takes a managed instance's owner
	// (tenant_admin, nav marker "owner"); empty means the owner may not open
	// it. Read only inside the AdminOnly Admin area, whose other entries the
	// marker script hides from the owner (GitHub #625 item 1; LLD 18,
	// amendment 2026-11-12).
	OwnerHref string
}

// navAreaDef is one top-level area shown in the icon rail. AdminOnly
// areas are rendered hidden-by-default and unhidden by the same
// IsAdmin / marker-cookie path used for the legacy Admin link.
type navAreaDef struct {
	Key   string
	Label string
	// Short is the compact label the < md bottom tab bar renders in place
	// of Label (2026-07-10 mobile-nav-overflow fix): the bar packs up to 8
	// flex-1 cells (~47px each on a 375px phone), where a 10px-font label
	// longer than ~7 characters ("Orchestration", "Integrations") overflows
	// its cell. Empty = Label is already short enough; see MobileLabel.
	Short string
	Icon  string
	// Href is the rail icon's click target — the area's primary
	// destination. Kept as an explicit field (not index .Dests 0) because
	// the template FuncMap overrides the builtin `index`.
	Href      string
	AdminOnly bool
	// OwnerHref is the rail icon's target for a managed instance's owner,
	// who is refused Href (see navDest.OwnerHref).
	OwnerHref string
	// Page is the CurrentPage token of a door's landing page, so the door
	// highlights there (#755: a door has no destinations to map).
	Page string
	// Badge is the live attention count on a door (Needs you, #755); set
	// per request by navModelForPage, zero renders nothing.
	Badge int
	// PhoneTab marks the four areas the < md bar shows as tabs (§18): the
	// others sit in its More sheet.
	PhoneTab bool
	Dests    []navDest
}

// MobileLabel is what the < md bottom tab bar renders under the area's
// icon: Short when set, else Label. Desktop surfaces (rail tooltip,
// contextual panel, drawer heading) always use the full Label.
func (a navAreaDef) MobileLabel() string {
	if a.Short != "" {
		return a.Short
	}
	return a.Label
}

// navModel is the single source of truth for the navigation IA. The
// icon rail, the contextual panel, and the mobile drawer all render
// from this slice so they cannot drift apart.
//
// The full model includes every destination; edition/capability gating is
// applied by navModelFunc (e.g. Trading is dropped on Community). Keeping
// navModel itself pure means pageToArea and tests see the canonical IA.
func navModel() []navAreaDef {
	// Navigation v2 (ui-design-refresh-design.md §13.2, #755): three doors
	// and four layers. The first four areas are the doors every session
	// sees. Home, Ask and Needs you are plain links with no panel; Results &
	// cost opens a panel (Spend, Trends, Adoption) until #757. Every
	// destination keeps its key, URL and flags from the five-area model
	// before it (§13.1); entries that left the admin-only Admin area for
	// Manage carry AdminOnly themselves, so no audience gains or loses a
	// nav entry (nav_v2_755_test.go pins all three audiences).
	return []navAreaDef{
		{Key: "home", Label: "Home", Icon: "navIconOverview", Href: "/ui/", Page: "dashboard", PhoneTab: true},
		// Ask lands on the dispatcher project's chat (askHandler).
		{Key: "ask", Label: "Ask", Icon: "navIconAsk", Href: "/ui/ask", PhoneTab: true},
		// Needs you is the outcome inbox; it carries the attention badge
		// (navModelForPage), which sat on the "My requests" entry before.
		{Key: "needs", Label: "Needs you", Short: "Needs", Icon: "navIconInbox", Href: "/ui/inbox", Page: "inbox", PhoneTab: true},
		// Results & cost lands on the Results page (#757, design §15.2).
		{Key: "results", Label: "Results & cost", Short: "Results", Icon: "navIconSpend", Href: "/ui/results", PhoneTab: true, Dests: []navDest{
			{Key: "results", Label: "Results", Href: "/ui/results", Icon: "navIconSpend"},
			{Key: "spend", Label: "Spend", Href: "/ui/spend", Icon: "navIconSpend"},
			{Key: "trends", Label: "Trends", Href: "/ui/insights/trends", Icon: "navIconInsight"},
			{Key: "adoption", Label: "Adoption", Href: "/ui/insights/adoption", Icon: "navIconAdoption"},
		}},
		// Manage holds everything an owner configures (no CLI on SaaS).
		// Four of the five entries from the old Admin area are AdminOnly:
		// project users saw none of them, and the owner sees Keys & access
		// by OwnerHref. My account is the exception (below).
		{Key: "manage", Label: "Manage", Icon: "navIconManage", Href: "/ui/manage", Dests: []navDest{
			// The overview of Manage (#759, design §17.2 M1).
			{Key: "manage-overview", Label: "Overview", Href: "/ui/manage", Icon: "navIconOverview"},
			{Key: "projects", Label: "Assistants", Href: "/ui/projects", Icon: "navIconProjects"},
			{Key: "integrations", Label: "Integrations", Href: "/ui/integrations", Icon: "navIconIntegrations", AdminOnly: true},
			{Key: "admin-keys", Label: "Keys & access", Href: "/ui/admin/keys", Icon: "navIconKey", AdminOnly: true, OwnerHref: "/ui/credentials"},
			{Key: "admin-agents", Label: "Coding agents", Href: "/ui/admin/agents", Icon: "navIconRobot", AdminOnly: true},
			{Key: "admin-skills", Label: "Skills", Href: "/ui/admin/skills", Icon: "navIconSkill", AdminOnly: true},
			{Key: "reminders", Label: "Reminders", Href: "/ui/reminders", Icon: "navIconReminders"},
			{Key: "insights", Label: "Tool budget", Href: "/ui/insights/tool-budget", Icon: "navIconGauge"},
			{Key: "trading", Label: "Trading", Href: "/ui/trading", Icon: "navIconTrading", Cap: "trading"},
			// Open to every session (my_account_test: the one screen a
			// non-admin needs); the old Admin area hid it from them.
			{Key: "my-account", Label: "My account", Href: "/ui/account", Icon: "navIconUser", OwnerHref: "/ui/account"},
		}},
		// Diagnostics: the engineering views, unchanged; it lands on the
		// overview of the tiles that left the home page (#756).
		{Key: "diagnostics", Label: "Diagnostics", Short: "Inspect", Icon: "navIconDiagnostics", Href: "/ui/diagnostics", Dests: []navDest{
			// The tiles that left the home page (#756, design §14.2).
			{Key: "diagnostics-overview", Label: "Overview", Href: "/ui/diagnostics", Icon: "navIconOverview"},
			{Key: "live", Label: "Live", Href: "/ui/live", Icon: "navIconLive"},
			{Key: "tasks", Label: "Tasks", Href: "/ui/tasks", Icon: "navIconTasks"},
			{Key: "executions", Label: "Executions", Href: "/ui/executions", Icon: "navIconExecutions"},
			{Key: "swarms", Label: "Swarms", Href: "/ui/swarms", Icon: "navIconSwarms", AdminOnly: true},
			{Key: "workflows", Label: "Workflows", Href: "/ui/workflows", Icon: "navIconWorkflows", AdminOnly: true},
			{Key: "memory", Label: "Memory", Href: "/ui/memory", Icon: "navIconMemory"},
			{Key: "quality", Label: "Quality", Href: "/ui/insights/quality", Icon: "navIconScorecard"},
			{Key: "audit", Label: "Audit", Href: "/ui/audit", Icon: "navIconAudit", AdminOnly: true},
		}},
		// The operator console: whoever runs the instance. Admin-only as
		// the Admin area was; the owner's entries moved to Manage.
		{Key: "operator", Label: "Operator console", Short: "Console", Icon: "navIconAdmin", Href: "/ui/admin/", AdminOnly: true, Dests: []navDest{
			{Key: "admin", Label: "Admin console", Href: "/ui/admin/", Icon: "navIconAdmin"},
			{Key: "operator", Label: "Accounts", Href: "/ui/operator/accounts", Icon: "navIconUsers", AdminOnly: true},
			{Key: "regulatory", Label: "Regulatory record", Href: "/ui/operator/regulatory", Icon: "navIconAudit", AdminOnly: true},
			{Key: "admin-control-plane", Label: "Control plane", Href: "/ui/admin/control-plane", Icon: "navIconControlPlane"},
		}},
	}
}

// navModelFunc is the capability-aware nav template func. It drops the
// Trading destination unless tradingEnabled() reports the destination is
// worth showing — without this the data-cap hint would still render a dead
// or empty link. Built at the template-setup site so uiFuncMap stays
// edition-agnostic.
//
// tradingEnabled is a predicate, not a bool, because the answer is not fixed
// at server-construction time: it combines the static edition gate with a
// live "does any project actually have trading configured" check that must
// follow a config reload (see Server.tradingNavEnabled). It is called on
// every nav render; nil means "hidden".
func navModelFunc(tradingEnabled func() bool) func() []navAreaDef {
	return func() []navAreaDef {
		m := navModel()
		if tradingEnabled != nil && tradingEnabled() {
			return m
		}
		for i := range m {
			m[i].Dests = filterDests(m[i].Dests, "trading")
		}
		return m
	}
}

// filterDests returns dests with any entry whose Key == drop removed,
// preserving order. Used to elide edition-gated destinations (e.g. Trading
// on Community) so the nav never renders a link to a 404 route.
func filterDests(dests []navDest, drop string) []navDest {
	out := make([]navDest, 0, len(dests))
	for _, d := range dests {
		if d.Key != drop {
			out = append(out, d)
		}
	}
	return out
}

// pageToArea is derived from navModel at init so the mapping cannot
// drift from the IA defined above.
var pageToArea = func() map[string]string {
	m := map[string]string{}
	// Map every page→area including Trading: this is only consulted when the
	// caller is actually on that page (which can't happen in CE, where the
	// route 404s), and keeping it edition-independent avoids a second flag here.
	for _, a := range navModel() {
		if a.Page != "" {
			m[a.Page] = a.Key
		}
		for _, d := range a.Dests {
			m[d.Key] = a.Key
		}
	}
	return m
}()

// navAttentionCounter lets a page's Data struct expose the viewer's live
// outcome-inbox attention count for the nav's "My requests" badge
// (design outcome-inbox-design.md §5.7 Q4), without threading per-request
// nav state through every page handler's render() call. Optional/opt-in:
// a Data struct that doesn't implement it (every page except InboxData,
// as of task 4.4) simply renders no badge.
type navAttentionCounter interface {
	NavAttentionCount() int
}

// navModelForPage is the template FuncMap's "navModel" entry (wired by
// NewServer in place of the bare navModelFunc). It returns the same
// edition-filtered destinations navModelFunc does, plus the Needs you
// door's Badge populated from data's attention count when data
// implements navAttentionCounter. _partials.html's nav partial calls
// this as `{{range navModel .}}` (four call sites — rail, panel, mobile
// tabs, mobile drawer) so `.` (the current page's Data) reaches here.
//
// base() is called fresh on every invocation (mirroring navModelFunc's
// existing per-render allocation), so mutating the returned slice's
// Dests in place below is safe — it never aliases another request's
// copy.
func navModelForPage(tradingEnabled func() bool) func(data any) []navAreaDef {
	base := navModelFunc(tradingEnabled)
	return func(data any) []navAreaDef {
		m := base()
		counter, ok := data.(navAttentionCounter)
		if !ok {
			return m
		}
		count := counter.NavAttentionCount()
		if count <= 0 {
			return m
		}
		for i := range m {
			if m[i].Key == "needs" {
				m[i].Badge = count
			}
		}
		return m
	}
}

// navAreaForPage returns the top-level area key for a CurrentPage token, or
// "" when the page maps to no area (e.g. the dashboard, reached via the logo).
// "" means: no rail icon highlighted and no contextual panel open — so pages
// outside the IA don't leave a stale submenu hanging open.
func navAreaForPage(page string) string {
	if a, ok := pageToArea[page]; ok {
		return a
	}
	return ""
}

// navInMore reports whether a page's area sits in the phone bar's More sheet
// (§18): it maps to an area, and that area is not a phone tab. The bar
// highlights More on such pages.
func navInMore(page string) bool {
	key := navAreaForPage(page)
	if key == "" {
		return false
	}
	for _, a := range navModel() {
		if a.Key == key {
			return !a.PhoneTab
		}
	}
	return false
}

// navOwnerAttr is what the nav partial's navOwnerAttr template renders for one
// destination: Href for data-owner-href, Hide for data-owner-hide.
type navOwnerAttr struct {
	Href string
	Hide bool
}

// navOwner decides a destination's owner attribute (GitHub #625 item 1; LLD
// 18, amendment 2026-11-12). Only the AdminOnly area is the owner's business:
// elsewhere the owner sees what a project user sees. Inside it, an entry with
// OwnerHref is shown to the owner at that address; a non-AdminOnly entry
// without one is hidden from them (AdminOnly ones are already hidden from every
// non-admin).
func navOwner(area navAreaDef, d navDest) navOwnerAttr {
	// OwnerHref counts in any area since #755: the owner's entries moved
	// from the admin-only Admin area to Manage (design §13.2). An OwnerHref
	// therefore means "the owner may open this", even on an AdminOnly entry
	// in an admin-only area: set it only where the owner's gate admits them.
	if d.OwnerHref != "" {
		return navOwnerAttr{Href: d.OwnerHref}
	}
	if !area.AdminOnly {
		return navOwnerAttr{}
	}
	return navOwnerAttr{Hide: !d.AdminOnly}
}
