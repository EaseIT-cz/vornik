package approverdevice

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Agent-administered design §9.1a (operator, 2026-10-03: the approval page
// "looks nothing like the rest of Vornik"). The pages take the console's
// tokens and mark, and still load nothing from another origin, because
// credentials are typed here.

func templateSources(t *testing.T) map[string]string {
	t.Helper()
	files, err := filepath.Glob("templates/*.html")
	if err != nil || len(files) == 0 {
		t.Fatalf("no templates: %v", err)
	}
	out := map[string]string{}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		out[f] = string(b)
	}
	return out
}

// Review 8a84 F6, e076 F5: script, stylesheet links, @import, and every URL
// in src/href/srcset/action/formaction/ping and CSS url() must be
// same-origin paths (or fragments).
func TestLook_NoThirdPartyResource(t *testing.T) {
	attr := regexp.MustCompile(`(?i)\b(src|href|srcset|action|formaction|ping)\s*=\s*"([^"]*)"`)
	cssURL := regexp.MustCompile(`(?i)url\(\s*['"]?([^'")]*)`)
	for f, src := range templateSources(t) {
		low := strings.ToLower(src)
		for _, banned := range []string{"<script", `rel="stylesheet"`, "@import"} {
			if strings.Contains(low, banned) {
				t.Errorf("%s contains %q", f, banned)
			}
		}
		check := func(u string) {
			u = strings.TrimSpace(u)
			if u == "" || strings.HasPrefix(u, "#") || strings.HasPrefix(u, "{{") {
				return
			}
			if !strings.HasPrefix(u, "/") || strings.HasPrefix(u, "//") {
				t.Errorf("%s references %q, not a same-origin path", f, u)
			}
		}
		for _, m := range attr.FindAllStringSubmatch(src, -1) {
			check(m[2])
		}
		for _, m := range cssURL.FindAllStringSubmatch(src, -1) {
			check(m[1])
		}
	}
}

var cssVarDef = regexp.MustCompile(`--([a-z0-9-]+)\s*:\s*([^;]+);`)

// Review 8a84 F1, e076 F1/F6: the copied tokens equal the console's, per
// theme, so the approval page cannot drift from it silently.
func TestLook_TokensMatchTheConsole(t *testing.T) {
	console, err := os.ReadFile("../ui/templates/_partials.html")
	if err != nil {
		t.Fatal(err)
	}
	layout, _ := os.ReadFile("templates/_layout.html")
	block := func(src, open string) map[string]string {
		i := strings.Index(src, open)
		if i < 0 {
			t.Fatalf("no %q block", open)
		}
		j := strings.Index(src[i:], "}")
		out := map[string]string{}
		for _, m := range cssVarDef.FindAllStringSubmatch(src[i:i+j], -1) {
			out[m[1]] = strings.TrimSpace(m[2])
		}
		return out
	}
	triple := func(v string) string { // "59 66 82" -> "#3b4252"
		var r, g, b int
		if _, err := fmt.Sscanf(v, "%d %d %d", &r, &g, &b); err != nil {
			t.Fatalf("not an RGB triple: %q", v)
		}
		return fmt.Sprintf("#%02x%02x%02x", r, g, b)
	}
	for _, theme := range []struct{ console, page string }{
		{`:root, [data-theme="light"] {`, "/* light */"},
		{`[data-theme="dark"] {`, "/* dark */"},
	} {
		c := block(string(console), theme.console)
		p := block(string(layout), theme.page)
		for _, tok := range []string{"surface-900", "surface-800", "surface-600", "ink-100", "ink-300", "ink-500", "brand-500", "accent-500"} {
			want := triple(c[tok])
			if got := strings.ToLower(p[tok]); got != want {
				t.Errorf("%s --%s = %q, console has %s", theme.page, tok, got, want)
			}
		}
		if p["reject"] == "" {
			t.Errorf("%s has no --reject", theme.page)
		}
	}
}

// Review 8a84 F2: every variable a rule references is defined in both
// schemes, and every defined variable is referenced (no inert token).
func TestLook_EveryTokenDefinedAndUsed(t *testing.T) {
	layout, _ := os.ReadFile("templates/_layout.html")
	src := string(layout)
	used := map[string]bool{}
	for _, m := range regexp.MustCompile(`var\(--([a-z0-9-]+)\)`).FindAllStringSubmatch(src, -1) {
		used[m[1]] = true
	}
	for _, scheme := range []string{"/* light */", "/* dark */"} {
		i := strings.Index(src, scheme)
		if i < 0 {
			t.Fatalf("no %s block", scheme)
		}
		j := strings.Index(src[i:], "}")
		defined := map[string]bool{}
		for _, m := range cssVarDef.FindAllStringSubmatch(src[i:i+j], -1) {
			defined[m[1]] = true
		}
		for v := range used {
			if !defined[v] {
				t.Errorf("%s does not define --%s, which a rule uses", scheme, v)
			}
		}
		for v := range defined {
			if !used[v] {
				t.Errorf("%s defines --%s, which no rule uses", scheme, v)
			}
		}
	}
}

// Review 8a84 F7, e076 F4: the CSP is on every approval-route response,
// redirects and errors included.
func TestLook_CSPOnEveryResponse(t *testing.T) {
	f := newFixture(t)
	h := f.svc.Handler(nil)
	for _, path := range []string{"/ui/approve/", "/ui/approve/nope/extra/x", "/ui/pair", "/ui/pair/wait"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		csp := rec.Header().Get("Content-Security-Policy")
		if !strings.Contains(csp, "script-src 'none'") || !strings.Contains(csp, "default-src 'self'") {
			t.Errorf("%s (%d): CSP %q", path, rec.Code, csp)
		}
	}
}

// Review 8a84 F5, e076 F3: the list says how this device is told about new
// requests, from the configuration: live, configured but off, or nothing.
func TestLook_ListNamesTheNotifyChannel(t *testing.T) {
	for _, tc := range []struct {
		opt  Option
		want string
	}{
		{WithNotifyChannel("telegram", true), "announced on Telegram"},
		{WithNotifyChannel("slack", false), "Slack is configured but switched off"},
		{WithNotifyChannel("", false), "No notification channel is configured"},
	} {
		f := newFixture(t, tc.opt)
		srv := serve(t, f)
		phone := newBrowser(t, srv)
		code, _, _ := f.svc.StartPairing(context.Background(), "Pixel")
		phone.pairWith(code)
		_, list := phone.do(http.MethodGet, "/ui/approve/", nil, nil)
		if !strings.Contains(list, tc.want) {
			t.Errorf("want %q on the list:\n%s", tc.want, list)
		}
	}
}

// Review 8a84 F3, e076 F7: the approvals manifest installs the approval list
// and its scope covers every device route.
func TestLook_ApprovalManifestScope(t *testing.T) {
	raw, err := os.ReadFile("../ui/static/approve.webmanifest")
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		StartURL string `json:"start_url"`
		Scope    string `json:"scope"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m.StartURL != "/ui/approve/" {
		t.Fatalf("start_url %q", m.StartURL)
	}
	for _, r := range Routes() {
		if r.DeviceOnly && !strings.HasPrefix(r.Path, m.Scope) {
			t.Errorf("device route %s outside the manifest scope %s", r.Path, m.Scope)
		}
	}
}

// Design §9.1a, links (operator, 2026-10-03: "the links in the approval page
// look different from vornik interface"): every link is a nav pill, an action
// button (act) or an inline reference (ref), as the console draws them, and
// the layout defines those styles.
func TestLook_LinksAreStyledAsTheConsoleDraws(t *testing.T) {
	entries, err := templateFS.ReadDir("templates")
	if err != nil {
		t.Fatal(err)
	}
	anchor := regexp.MustCompile(`<a [^>]*>`)
	for _, e := range entries {
		raw, err := templateFS.ReadFile("templates/" + e.Name())
		if err != nil {
			t.Fatal(err)
		}
		body := string(raw)
		navless := regexp.MustCompile(`(?s)<nav>.*?</nav>`).ReplaceAllString(body, "")
		for _, a := range anchor.FindAllString(navless, -1) {
			if !strings.Contains(a, `class="act"`) && !strings.Contains(a, `class="ref"`) {
				t.Errorf("%s: link %s is neither an action (act) nor a reference (ref)", e.Name(), a)
			}
		}
	}
	layout, err := templateFS.ReadFile("templates/_layout.html")
	if err != nil {
		t.Fatal(err)
	}
	for _, rule := range []string{"text-decoration:none", "nav a{", `nav a[aria-current=page]{`, ".act{", ".ref{"} {
		if !strings.Contains(string(layout), rule) {
			t.Errorf("_layout.html does not define %q", rule)
		}
	}
}
