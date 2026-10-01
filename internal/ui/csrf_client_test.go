package ui

// The console's double-submit sender — 2026-09-19-ce-human-login-design.md
// §6.3.
//
// INCIDENT 2026-09-28 (P1): the double-submit validator shipped 2026-09-19/20
// and NOTHING in the console sent the token, so every mutating UI action from
// a session holding the vornik_csrf cookie was refused 403 (seen: pausing a
// task, rechecking the Telegram integration). These tests pin the one central
// sender and prove every mutating call site in the templates reaches it.

import (
	"bytes"
	"io/fs"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"testing"
)

func readTemplates(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := fs.WalkDir(templatesFS, "templates", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".html") {
			return err
		}
		b, rerr := fs.ReadFile(templatesFS, path)
		if rerr != nil {
			return rerr
		}
		out[strings.TrimPrefix(path, "templates/")] = string(b)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// isFullPage reports whether a template renders a whole document (and so is
// where the sender must be installed) rather than an htmx fragment swapped
// into one.
func isFullPage(src string) bool { return strings.Contains(src, "<html") }

// Every full page renders pageHead — the one place the sender is installed.
func TestCSRFSender_EveryFullPageIncludesPageHead(t *testing.T) {
	pages := 0
	for name, src := range readTemplates(t) {
		if name == "_partials.html" || !isFullPage(src) {
			continue
		}
		pages++
		if !strings.Contains(src, `{{template "pageHead"`) {
			t.Errorf("%s renders a full page without pageHead, so the CSRF sender is not installed on it", name)
		}
	}
	if pages == 0 {
		t.Fatal("found no full-page templates; the walk is broken")
	}
	t.Logf("examined %d full-page templates", pages)
}

// pageHead renders the sender, inline, byte-for-byte from csrf_client.js.
func TestCSRFSender_PageHeadRendersTheSenderInline(t *testing.T) {
	src, err := os.ReadFile("csrf_client.js")
	if err != nil {
		t.Fatalf("csrf_client.js: %v", err)
	}
	// The PRODUCTION registry, not a test-built one: the sender must be
	// present in what the daemon actually renders.
	s := NewServer()
	var buf bytes.Buffer
	if err := s.templates.ExecuteTemplate(&buf, "pageHead", map[string]any{"Title": "t"}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, strings.TrimSpace(string(src))) {
		t.Fatal("pageHead does not render csrf_client.js verbatim; the console sends no CSRF token")
	}
	// It must run before any page script calls fetch() or submits a form:
	// before htmx, and before </head>.
	i, j := strings.Index(out, "vornik_csrf"), strings.Index(out, "/ui/static/htmx.min.js")
	if i < 0 || j < 0 || i > j {
		t.Fatalf("the sender must be installed before htmx loads (sender at %d, htmx at %d)", i, j)
	}
}

var (
	reHxMutating = regexp.MustCompile(`hx-(post|put|patch|delete)=`)
	reFormPost   = regexp.MustCompile(`(?i)<form[^>]*\bmethod="?post`)
	reFetchCall  = regexp.MustCompile(`\bfetch\(\s*([^,)]*)`)
	reFormAttr   = regexp.MustCompile(`\sform="([^"{}]+)"`)
)

// The inventory: every mutating call site in the templates, by transport, and
// the proof that each transport is one the sender covers. Publishes the
// denominator (CLAUDE.md §4) so "no finding" means "examined", not "skipped".
func TestCSRFSender_EveryMutatingCallSiteIsCovered(t *testing.T) {
	var hx, forms, fetches, fragments int
	var fragmentNames []string
	for name, src := range readTemplates(t) {
		nHx := len(reHxMutating.FindAllString(src, -1))
		nForm := len(reFormPost.FindAllString(src, -1))
		calls := reFetchCall.FindAllStringSubmatch(src, -1)
		hx += nHx
		forms += nForm
		fetches += len(calls)

		// Transports the sender does NOT wrap. The console does not use
		// them; this keeps it that way.
		// The field name is reserved for the sender (§6.3).
		if regexp.MustCompile(`name="?vornik_csrf`).MatchString(src) {
			t.Errorf("%s defines its own vornik_csrf field; that name is reserved for the CSRF sender", name)
		}
		if regexp.MustCompile(`(?i)<base[\s>]`).MatchString(src) {
			t.Errorf("%s has a <base> element; it would move every relative request target", name)
		}
		for _, banned := range []string{"XMLHttpRequest", "sendBeacon"} {
			if strings.Contains(src, banned) {
				t.Errorf("%s uses %s, which the CSRF sender does not cover; use fetch() or htmx", name, banned)
			}
		}
		// fetch() is covered only for same-origin targets. A literal
		// absolute or protocol-relative URL would be sent WITHOUT the
		// token (by design) and so would be refused if it pointed back
		// at the daemon under another spelling.
		for _, c := range calls {
			arg := strings.TrimSpace(c[1])
			if strings.HasPrefix(arg, "'http") || strings.HasPrefix(arg, `"http`) ||
				strings.HasPrefix(arg, "'//") || strings.HasPrefix(arg, `"//`) || strings.HasPrefix(arg, "`http") {
				t.Errorf("%s: fetch(%s) targets an absolute URL; console fetches must be same-origin paths", name, arg)
			}
		}
		// A control outside its form (form="id") serialises in TREE order,
		// so it must come after the form element or it would precede the
		// injected first field and the server's first-field peek would miss
		// the token.
		for _, m := range reFormAttr.FindAllStringSubmatchIndex(src, -1) {
			id := src[m[2]:m[3]]
			formAt := strings.Index(src, `id="`+id+`"`)
			if formAt < 0 || formAt > m[0] {
				t.Errorf("%s: a control with form=%q precedes (or has no) its form element", name, id)
			}
		}
		if nHx+nForm+len(calls) > 0 && !isFullPage(src) && name != "_partials.html" {
			fragments++
			fragmentNames = append(fragmentNames, name)
		}
	}
	sort.Strings(fragmentNames)
	t.Logf("examined: %d htmx mutating attributes, %d <form method=post>, %d fetch() calls; "+
		"%d fragment templates carry call sites and inherit the sender from the page they are swapped into: %v",
		hx, forms, fetches, fragments, fragmentNames)
	if hx == 0 || forms == 0 || fetches == 0 {
		t.Fatal("the inventory found no call sites of some transport; the patterns no longer match the templates")
	}
}

// The behavioural half: run the real sender under node against a DOM shim
// and drive each request shape. Skips where node is absent, but FAILS in CI
// so a runner without node cannot turn it into a silent pass.
func TestCSRFSender_Behaviour(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("node not found in CI; the CSRF sender's behaviour test cannot run")
		}
		t.Skip("node not installed; the CSRF sender behaviour test did NOT run")
	}
	out, err := exec.Command(node, "testdata/csrf_client_harness.js", "csrf_client.js").CombinedOutput()
	t.Logf("%s", out)
	if err != nil {
		t.Fatalf("CSRF sender harness failed: %v", err)
	}
}
