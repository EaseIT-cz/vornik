package approval

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"vornik.io/vornik/internal/testutil/repowalk"
)

// Broker write-actions design (2026-09-29) §5.3: one gate and one hash for
// every human approval of a write. These tests pin both, and pin that the
// same-origin ladder exists in exactly one place.

func req(method string, headers map[string]string) *http.Request {
	r := httptest.NewRequest(method, "http://vornik.test/ui/inbox/x/approve", nil)
	r.Host = "vornik.test"
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	return r
}

func TestSameOrigin_Ladder(t *testing.T) {
	cases := []struct {
		name    string
		headers map[string]string
		want    bool
	}{
		{"same-origin", map[string]string{"Sec-Fetch-Site": "same-origin"}, true},
		{"same-site", map[string]string{"Sec-Fetch-Site": "same-site"}, true},
		// GitHub #448: none (no initiating page) is not a same-origin signal;
		// it falls through to the Origin rung.
		{"none (typed URL) without Origin", map[string]string{"Sec-Fetch-Site": "none"}, false},
		{"none with a matching Origin", map[string]string{"Sec-Fetch-Site": "none", "Origin": "http://vornik.test"}, true},
		{"none with a foreign Origin", map[string]string{"Sec-Fetch-Site": "none", "Origin": "http://evil.test"}, false},
		{"cross-site", map[string]string{"Sec-Fetch-Site": "cross-site", "Origin": "http://vornik.test"}, false},
		{"origin matches host", map[string]string{"Origin": "http://vornik.test"}, true},
		{"origin differs", map[string]string{"Origin": "http://evil.test"}, false},
		{"malformed origin", map[string]string{"Origin": "::::"}, false},
		{"no signal fails closed", nil, false},
	}
	for _, tc := range cases {
		if got := SameOrigin(req(http.MethodPost, tc.headers)); got != tc.want {
			t.Errorf("%s: SameOrigin = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestCheckRequest(t *testing.T) {
	if err := CheckRequest(req(http.MethodGet, map[string]string{"Sec-Fetch-Site": "same-origin"})); !errors.Is(err, ErrMethod) {
		t.Fatalf("GET must be refused before anything else, got %v", err)
	}
	if err := CheckRequest(req(http.MethodPost, map[string]string{"Sec-Fetch-Site": "cross-site"})); !errors.Is(err, ErrCrossSite) {
		t.Fatalf("cross-site POST must be refused, got %v", err)
	}
	if err := CheckRequest(req(http.MethodPost, map[string]string{"Sec-Fetch-Site": "same-origin"})); err != nil {
		t.Fatalf("same-origin POST must pass, got %v", err)
	}
}

func TestAuthorize(t *testing.T) {
	allow := func(_ *http.Request, p string) bool { return p == "mine" }
	who := func(*http.Request) string { return "op-1" }
	r := req(http.MethodPost, nil)
	if _, err := Authorize(r, "theirs", allow, who); !errors.Is(err, ErrScope) {
		t.Fatalf("another project's approval must be refused, got %v", err)
	}
	approver, err := Authorize(r, "mine", allow, who)
	if err != nil || approver != "op-1" {
		t.Fatalf("Authorize = %q, %v", approver, err)
	}
	// Auth-off deployments have no identity: attributed, not refused.
	if who, err := Authorize(r, "mine", allow, func(*http.Request) string { return "" }); err != nil || who != Unauthenticated {
		t.Fatalf("no identity must be recorded as %q, got %q, %v", Unauthenticated, who, err)
	}
}

func TestWriteError_StatusCodes(t *testing.T) {
	for err, want := range map[error]int{ErrMethod: 405, ErrCrossSite: 403, ErrScope: 404} {
		rec := httptest.NewRecorder()
		WriteError(rec, req(http.MethodPost, nil), err)
		if rec.Code != want {
			t.Errorf("%v -> %d, want %d", err, rec.Code, want)
		}
	}
}

func TestCanonicalSHA256_IgnoresKeyOrderAndWhitespace(t *testing.T) {
	a, err := CanonicalSHA256([]byte(`{"b": 1, "a": {"y": [1, 2], "x": "é"}}`))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := CanonicalSHA256([]byte(`{"a":{"x":"é","y":[1,2]},"b":1}`))
	if a != b || len(a) != 64 {
		t.Fatalf("canonical hashes differ: %s vs %s", a, b)
	}
	c, _ := CanonicalSHA256([]byte(`{"a":{"x":"é","y":[2,1]},"b":1}`))
	if a == c {
		t.Fatal("array order is significant")
	}
	if _, err := CanonicalSHA256([]byte(`{not json`)); err == nil {
		t.Fatal("invalid JSON must be an error")
	}
	canon, _ := Canonical([]byte(`{ "b":1, "a":2 }`))
	if string(canon) != `{"a":2,"b":1}` {
		t.Fatalf("Canonical = %s", canon)
	}
}

// One ladder: the Sec-Fetch-Site decision lives here and nowhere else under
// internal/. A second copy is how the inbox and the middleware could drift.
func TestSameOriginLadderHasOneImplementation(t *testing.T) {
	_, here, _, _ := runtime.Caller(0)
	root := filepath.Join(filepath.Dir(here), "..")
	var offenders []string
	scanned := 0
	// #334 item 6 (audit 2026-10-12): the walk's error and every read error
	// were discarded, so an unreadable tree reported "no duplicate" over zero
	// files. Errors fail now, and the law publishes how many files it read.
	werr := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() && repowalk.SkipDir(root, path, info.Name()) {
			return filepath.SkipDir
		}
		if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		if strings.Contains(path, string(filepath.Separator)+"approval"+string(filepath.Separator)) {
			return nil
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		scanned++
		// GitHub #494 (cloudflare-real-ip-design amendment, change item 4):
		// the law matched one exact spelling of the ladder, so a copy spelled
		// differently passed. Any non-comment line naming a Sec-Fetch-Site
		// verdict literal outside internal/approval is a second ladder.
		for _, line := range strings.Split(string(b), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") {
				continue
			}
			if strings.Contains(trimmed, `"cross-site"`) || strings.Contains(trimmed, `"same-origin"`) || strings.Contains(trimmed, `"same-site"`) {
				offenders = append(offenders, path)
				break
			}
		}
		return nil
	})
	if werr != nil {
		t.Fatalf("walk %s: %v", root, werr)
	}
	t.Logf("same-origin ladder law: examined %d non-test Go files under internal/", scanned)
	if scanned < 100 {
		t.Fatalf("same-origin ladder law: examined %d files (floor 100); a law that looked at nothing has not passed", scanned)
	}
	if len(offenders) > 0 {
		t.Fatalf("same-origin ladder duplicated outside internal/approval: %v — call approval.SameOrigin", offenders)
	}
	// The law must also see the ladder where it lives, so it fails if the
	// ladder moves out of this package rather than passing over nothing.
	own, err := os.ReadFile(filepath.Join(filepath.Dir(here), "approval.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(own), `"cross-site"`) || !strings.Contains(string(own), `"same-origin"`) || !strings.Contains(string(own), `"same-site"`) {
		t.Fatal("same-origin ladder law: internal/approval/approval.go no longer holds the ladder's literals; update the law with the ladder")
	}
}

// GitHub #494 (review abd2 F3): CrossSiteWrite exempts a request carrying
// X-API-Key or a non-Basic Authorization header because a cross-site page
// cannot set either without a CORS preflight, and the daemon grants none. A
// later Access-Control-Allow-* header anywhere would open that exemption
// silently, so its absence is pinned here.
func TestNoCORSAllowHeaderOutsideTests_Issue494(t *testing.T) {
	_, here, _, _ := runtime.Caller(0)
	repoRoot := filepath.Join(filepath.Dir(here), "..", "..")
	var offenders []string
	scanned := 0
	for _, dir := range []string{"internal", "cmd"} {
		root := filepath.Join(repoRoot, dir)
		werr := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() && repowalk.SkipDir(root, path, info.Name()) {
				return filepath.SkipDir
			}
			if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			b, rerr := os.ReadFile(path)
			if rerr != nil {
				return rerr
			}
			scanned++
			if strings.Contains(string(b), "Access-Control-Allow") {
				offenders = append(offenders, path)
			}
			return nil
		})
		if werr != nil {
			t.Fatalf("walk %s: %v", root, werr)
		}
	}
	t.Logf("CORS law: examined %d non-test Go files under internal/ and cmd/", scanned)
	if scanned < 100 {
		t.Fatalf("CORS law: examined %d files (floor 100); a law that looked at nothing has not passed", scanned)
	}
	if len(offenders) > 0 {
		t.Fatalf("Access-Control-Allow-* set in %v: approval.CrossSiteWrite's header-credential exemption assumes no CORS grant; revisit it before granting one", offenders)
	}
}

// GitHub #494 (2026-10-21, cloudflare-real-ip-design "Amendment 2026-10-21
// (#494, #448)", change item 2): the auth-off CSRF rule. An unsafe method
// that carries a browser origin signal, fails the ladder and carries no
// header-borne credential is a cross-site write.
func TestCrossSiteWrite_Issue494(t *testing.T) {
	cases := []struct {
		name    string
		method  string
		headers map[string]string
		want    bool
	}{
		{"cross-site POST", http.MethodPost, map[string]string{"Sec-Fetch-Site": "cross-site"}, true},
		{"foreign Origin POST", http.MethodPost, map[string]string{"Origin": "http://evil.test"}, true},
		{"Origin null (sandboxed frame)", http.MethodPost, map[string]string{"Origin": "null"}, true},
		{"none without Origin", http.MethodPost, map[string]string{"Sec-Fetch-Site": "none"}, true},
		{"cross-site PUT", http.MethodPut, map[string]string{"Sec-Fetch-Site": "cross-site"}, true},
		{"cross-site DELETE", http.MethodDelete, map[string]string{"Origin": "http://evil.test"}, true},
		{"cross-site with Basic", http.MethodPost, map[string]string{"Sec-Fetch-Site": "cross-site", "Authorization": "Basic YXBpOng="}, true},
		// Review d85e F1: same-site is true for another port of the same IP
		// or host (a page on 127.0.0.1:3000 posting to 127.0.0.1:8080), so the
		// auth-off gate requires same-ORIGIN.
		{"same-site (another port)", http.MethodPost, map[string]string{"Sec-Fetch-Site": "same-site"}, true},
		{"same-site with Origin equal to Host", http.MethodPost, map[string]string{"Sec-Fetch-Site": "same-site", "Origin": "http://vornik.test"}, true},
		{"no browser signal (CLI)", http.MethodPost, nil, false},
		{"same-origin", http.MethodPost, map[string]string{"Sec-Fetch-Site": "same-origin"}, false},
		{"Origin equals Host", http.MethodPost, map[string]string{"Origin": "http://vornik.test"}, false},
		{"cross-site with X-API-Key", http.MethodPost, map[string]string{"Sec-Fetch-Site": "cross-site", "X-API-Key": "k"}, false},
		{"cross-site with Bearer", http.MethodPost, map[string]string{"Sec-Fetch-Site": "cross-site", "Authorization": "Bearer k"}, false},
		{"cross-site GET", http.MethodGet, map[string]string{"Sec-Fetch-Site": "cross-site"}, false},
		{"cross-site HEAD", http.MethodHead, map[string]string{"Sec-Fetch-Site": "cross-site"}, false},
		{"cross-site OPTIONS", http.MethodOptions, map[string]string{"Sec-Fetch-Site": "cross-site"}, false},
	}
	for _, tc := range cases {
		if got := CrossSiteWrite(req(tc.method, tc.headers)); got != tc.want {
			t.Errorf("%s: CrossSiteWrite = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// review-20260930-32ae: escapes normalise, duplicate keys are refused.
func TestCanonical_EscapesAndDuplicateKeys(t *testing.T) {
	a, _ := CanonicalSHA256([]byte(`{"x":"\u00e9"}`))
	b, _ := CanonicalSHA256([]byte(`{"x":"é"}`))
	if a != b {
		t.Fatal("a \\u escape and the literal rune must hash equal")
	}
	for _, dup := range []string{`{"a":1,"a":2}`, `{"o":{"k":1,"k":1}}`, `[{"a":1,"a":1}]`} {
		if _, err := Canonical([]byte(dup)); err == nil {
			t.Fatalf("%s: duplicate keys must be refused, not collapsed", dup)
		}
	}
	if _, err := Canonical([]byte(`{"a":{"k":1},"b":{"k":1}}`)); err != nil {
		t.Fatalf("the same key in sibling objects is not a duplicate: %v", err)
	}
}
