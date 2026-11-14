package ui

import (
	"sort"
	"strings"
	"testing"

	"golang.org/x/net/html"
)

// The phone bar was "very dense" on an iPhone (operator, 2026-11-14): up to
// eight cells, 48.75 pt each on a 390 pt screen. It is now five for every
// audience: Home, Ask, Needs you, Results and More, with the other areas in
// the More sheet (ui-design-refresh-design.md §18).

// phoneDoc renders the nav for one audience and applies what the browser's
// marker-cookie script does to it: a session unhides [data-signout]; "admin"
// unhides [data-admin-link]; "owner" unhides [data-owner-href] (pointing
// links at it) and hides [data-owner-hide]. It returns the phone nav.
func phoneDoc(t *testing.T, audience string) *html.Node {
	t.Helper()
	fx := navFixture{CurrentPage: "tasks", IsSession: true}
	switch audience {
	case "admin":
		fx.IsAdmin = true
	case "auth off":
		fx = navFixture{CurrentPage: "tasks", IsAdmin: true}
	}
	doc, err := html.Parse(strings.NewReader(renderTemplateString(t, "nav", fx)))
	if err != nil {
		t.Fatal(err)
	}
	phWalk(doc, func(n *html.Node) {
		switch {
		case audience != "auth off" && phHas(n, "data-signout"):
			phSetHidden(n, false)
		case audience == "admin" && phHas(n, "data-admin-link"):
			phSetHidden(n, false)
		case audience == "owner" && phHas(n, "data-owner-href"):
			phSetHidden(n, false)
			if n.Data == "a" {
				phSet(n, "href", phAttr(n, "data-owner-href"))
			}
		case audience == "owner" && phHas(n, "data-owner-hide"):
			phSetHidden(n, true)
		}
	})
	var phone *html.Node
	phWalk(doc, func(n *html.Node) {
		if phone == nil && n.Data == "nav" && strings.Contains(phAttr(n, "class"), "md:hidden") {
			phone = n
		}
	})
	if phone == nil {
		t.Fatal("no phone nav rendered")
	}
	return phone
}

func TestPhoneBar_FiveCellsForEveryAudience_18(t *testing.T) {
	want := "door:home door:ask door:needs tab:results tab:more"
	for _, audience := range []string{"admin", "owner", "project user", "auth off"} {
		bar := phFirstChild(phoneDoc(t, audience))
		var cells []string
		for c := bar.FirstChild; c != nil; c = c.NextSibling {
			if c.Type != html.ElementNode || phIsHidden(c) {
				continue
			}
			switch {
			case phHas(c, "data-nav-door"):
				cells = append(cells, "door:"+phAttr(c, "data-nav-door"))
			case phHas(c, "data-nav-tab"):
				cells = append(cells, "tab:"+phAttr(c, "data-nav-tab"))
			default:
				cells = append(cells, c.Data)
			}
		}
		if got := strings.Join(cells, " "); got != want {
			t.Errorf("%s: phone bar cells %q, want %q", audience, got, want)
		}
	}
}

// Every audience reaches, from the phone bar and its drawers, exactly the
// entries the rail gives it (visibleKeys, the #755 audience model).
func TestPhoneBar_EveryAudienceKeepsItsEntries_18(t *testing.T) {
	// The model the rendered page uses: edition-filtered (Trading off here).
	m := navModelFunc(NewServer().tradingNavEnabled)()
	for _, audience := range []string{"admin", "owner", "project user"} {
		want := map[string]bool{}
		keys := map[string]bool{}
		for _, k := range visibleKeys(m, audience) {
			keys[k] = true
		}
		for _, a := range m {
			if keys["door:"+a.Key] {
				want[a.Href] = true
			}
			for _, d := range a.Dests {
				if !keys[d.Key] {
					continue
				}
				href := d.Href
				if o := navOwner(a, d); audience == "owner" && o.Href != "" {
					href = o.Href
				}
				want[href] = true
			}
		}
		got := map[string]bool{}
		phWalk(phoneDoc(t, audience), func(n *html.Node) {
			if n.Data == "a" && !phHiddenInTree(n) {
				got[phAttr(n, "href")] = true
			}
		})
		if a, b := phSorted(got), phSorted(want); strings.Join(a, " ") != strings.Join(b, " ") {
			t.Errorf("%s reaches on the phone\n %v\nwant\n %v", audience, a, b)
		}
	}
}

func TestPhoneBar_MoreHoldsTheOtherAreas_18(t *testing.T) {
	more := func(audience string) map[string]*html.Node {
		groups := map[string]*html.Node{}
		phWalk(phoneDoc(t, audience), func(n *html.Node) {
			if phAttr(n, "data-nav-drawer") == "more" {
				phWalk(n, func(g *html.Node) {
					if phHas(g, "data-nav-more") {
						groups[phAttr(g, "data-nav-more")] = g
					}
				})
			}
		})
		return groups
	}
	admin := more("admin")
	for _, k := range []string{"manage", "diagnostics", "operator"} {
		g := admin[k]
		if g == nil || g.Data != "details" {
			t.Fatalf("More lacks a <details> group for %q", k)
		}
		if phHiddenInTree(g) {
			t.Errorf("admin: the %q group is hidden", k)
		}
	}
	if g := more("owner")["operator"]; g == nil || !phHiddenInTree(g) {
		t.Error("owner: the operator console group should be hidden")
	}
	if g := more("project user")["operator"]; g == nil || !phHiddenInTree(g) {
		t.Error("project user: the operator console group should be hidden")
	}
	// No other area keeps a phone tab of its own.
	phWalk(phoneDoc(t, "admin"), func(n *html.Node) {
		if k := phAttr(n, "data-nav-tab"); k != "" && k != "results" && k != "more" {
			t.Errorf("%q still has a phone tab", k)
		}
	})
}

func TestPhoneBar_MoreHasThemeAccountAndSignOut_18(t *testing.T) {
	find := func(audience string) (theme, account, signout *html.Node) {
		phWalk(phoneDoc(t, audience), func(n *html.Node) {
			if phAttr(n, "data-nav-drawer") != "more" {
				return
			}
			phWalk(n, func(c *html.Node) {
				switch {
				case c.Data == "button" && phHas(c, "data-theme-toggle"):
					theme = c
				case c.Data == "a" && phAttr(c, "href") == "/ui/account" && phHas(c, "data-nav-account"):
					account = c
				case c.Data == "form" && phAttr(c, "action") == "/ui/logout":
					signout = c
				}
			})
		})
		return
	}
	theme, account, signout := find("project user")
	if theme == nil || phHiddenInTree(theme) {
		t.Error("More lacks a visible theme toggle")
	}
	if account == nil || phHiddenInTree(account) || signout == nil || phHiddenInTree(signout) {
		t.Fatal("More lacks a visible My account link and Sign out form for a session")
	}
	if phAttr(signout, "data-confirm") == "" {
		t.Error("Sign out lost its confirmation")
	}
	theme, account, signout = find("auth off")
	if theme == nil || phHiddenInTree(theme) {
		t.Error("auth off: the theme toggle should stay visible")
	}
	if !phHiddenInTree(account) || !phHiddenInTree(signout) {
		t.Error("auth off: My account and Sign out should be hidden")
	}
}

func TestPhoneBar_MoreIsActiveOnItsAreas_18(t *testing.T) {
	for page, want := range map[string]bool{
		"": false, "no-such-page": false,
		"dashboard": false, "inbox": false, "results": false, "spend": false,
		"projects": true, "manage-overview": true, "tasks": true, "audit": true,
	} {
		if got := navInMore(page); got != want {
			t.Errorf("navInMore(%q) = %v, want %v", page, got, want)
		}
		html := renderTemplateString(t, "nav", navFixture{CurrentPage: page, IsAdmin: true, IsSession: true})
		i := strings.Index(html, `data-nav-tab="more"`)
		if i < 0 {
			t.Fatal("no More tab")
		}
		end := strings.Index(html[i:], ">")
		active := strings.Contains(html[i:i+end], "text-brand-300")
		if active != want {
			t.Errorf("page %q: More highlighted = %v, want %v", page, active, want)
		}
	}
}

// The drawer script has no DOM test lane; this pins the selector contract
// it relies on (review-20261114-3224 F7): each tab names a drawer that
// exists, the outside-tap and auto-hide selectors are the ones in the
// script, and no drawer claims role="menu" (F5: details and buttons are
// not menu items).
func TestPhoneBar_DrawerScriptContract_18(t *testing.T) {
	src := renderTemplateString(t, "nav", navFixture{CurrentPage: "tasks", IsAdmin: true, IsSession: true})
	for _, want := range []string{
		"e.target.closest('[data-nav-tab]') || e.target.closest('[data-nav-drawer]')",
		"document.querySelectorAll('[data-nav-drawer] a')",
		"b.setAttribute('aria-expanded'",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("drawer script lacks %s", want)
		}
	}
	phone := phoneDoc(t, "admin")
	drawers := map[string]bool{}
	phWalk(phone, func(n *html.Node) {
		if k := phAttr(n, "data-nav-drawer"); k != "" {
			drawers[k] = true
			if phAttr(n, "role") == "menu" {
				t.Errorf("drawer %q has role=menu", k)
			}
			if phAttr(n, "id") != "nav-drawer-"+k || phAttr(n, "aria-label") == "" {
				t.Errorf("drawer %q lacks its id or accessible name", k)
			}
		}
	})
	phWalk(phone, func(n *html.Node) {
		if k := phAttr(n, "data-nav-tab"); k != "" {
			if !drawers[k] || phAttr(n, "aria-controls") != "nav-drawer-"+k || phAttr(n, "aria-expanded") != "false" || phAttr(n, "aria-label") == "" {
				t.Errorf("tab %q: drawer, aria-controls, aria-expanded or name missing", k)
			}
		}
	})
	if !drawers["more"] || !drawers["results"] || len(drawers) != 2 {
		t.Errorf("drawers %v, want exactly results and more", drawers)
	}
}

// The phone theme row did nothing (operator, on an iPhone, 2026-11-14,
// after 055fd7b9f): the theme script runs inside the rail, before the
// phone bar exists in the document, so a querySelectorAll binding at run
// time never reached the More sheet's button. A toggle rendered after the
// script is reachable only through a document-level (delegated) listener.
func TestThemeToggle_ReachesTogglesRenderedAfterIt_18(t *testing.T) {
	src := renderTemplateString(t, "nav", navFixture{CurrentPage: "tasks", IsSession: true})
	script := strings.Index(src, "vornik:theme")
	if script < 0 {
		t.Fatal("no theme script rendered")
	}
	start := strings.LastIndex(src[:script], "<script>")
	end := script + strings.Index(src[script:], "</script>")
	body := src[start:end]
	delegated := strings.Contains(body, "document.addEventListener('click'") && strings.Contains(body, "closest('[data-theme-toggle]')")
	after := strings.Count(src[end:], "data-theme-toggle")
	if after == 0 {
		t.Fatal("expected the phone More sheet's toggle after the theme script")
	}
	if !delegated {
		t.Errorf("%d theme toggle(s) render after the theme script, which binds at run time: they do nothing; delegate from document", after)
	}
}

func TestThemeToggle_BindsEveryToggle_18(t *testing.T) {
	src := renderTemplateString(t, "nav", navFixture{CurrentPage: "tasks", IsSession: true})
	if !strings.Contains(src, `id="theme-toggle"`) || strings.Count(src, "data-theme-toggle") < 3 {
		t.Error("the rail toggle keeps its id and both toggles carry data-theme-toggle")
	}
}

// --- small html helpers ---

func phWalk(n *html.Node, f func(*html.Node)) {
	f(n)
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		phWalk(c, f)
	}
}

func phAttr(n *html.Node, k string) string {
	if n == nil {
		return ""
	}
	for _, a := range n.Attr {
		if a.Key == k {
			return a.Val
		}
	}
	return ""
}

func phHas(n *html.Node, k string) bool {
	for _, a := range n.Attr {
		if a.Key == k {
			return true
		}
	}
	return false
}

func phSet(n *html.Node, k, v string) {
	for i := range n.Attr {
		if n.Attr[i].Key == k {
			n.Attr[i].Val = v
			return
		}
	}
	n.Attr = append(n.Attr, html.Attribute{Key: k, Val: v})
}

func phIsHidden(n *html.Node) bool {
	for _, c := range strings.Fields(phAttr(n, "class")) {
		if c == "hidden" {
			return true
		}
	}
	return false
}

// phHiddenInTree reports whether n or an ancestor is hidden. A closed
// drawer does not count: its tab opens it. A nil node counts as hidden,
// so a missing element fails a "visible" check.
func phHiddenInTree(n *html.Node) bool {
	if n == nil {
		return true
	}
	for p := n; p != nil; p = p.Parent {
		if p.Type == html.ElementNode && phIsHidden(p) && !phHas(p, "data-nav-drawer") {
			return true
		}
	}
	return false
}

func phSetHidden(n *html.Node, hidden bool) {
	var out []string
	for _, c := range strings.Fields(phAttr(n, "class")) {
		if c != "hidden" {
			out = append(out, c)
		}
	}
	if hidden {
		out = append(out, "hidden")
	}
	phSet(n, "class", strings.Join(out, " "))
}

func phFirstChild(n *html.Node) *html.Node {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.ElementNode {
			return c
		}
	}
	return nil
}

func phSorted(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
