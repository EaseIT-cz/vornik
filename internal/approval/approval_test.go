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
		{"none (typed URL)", map[string]string{"Sec-Fetch-Site": "none"}, true},
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
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		if strings.Contains(path, string(filepath.Separator)+"approval"+string(filepath.Separator)) {
			return nil
		}
		b, _ := os.ReadFile(path)
		if strings.Contains(string(b), `case "same-origin", "same-site", "none":`) {
			offenders = append(offenders, path)
		}
		return nil
	})
	if len(offenders) > 0 {
		t.Fatalf("same-origin ladder duplicated outside internal/approval: %v — call approval.SameOrigin", offenders)
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
