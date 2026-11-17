package statusfmt

// Acceptance suite for sw-01-flag (benchmark LLD §12.24). Written from the
// task's specification, never shown to the agents. Uses only the pinned API:
//   RenderStatus(name string, healthy bool, checks map[string]bool) string
//   RenderStatusJSON(name string, healthy bool, checks map[string]bool) string

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestAcceptance_TextNamesTheServiceAndEveryCheck(t *testing.T) {
	out := RenderStatus("billing-api", true, map[string]bool{"db": true, "cache": false})
	for _, want := range []string{"billing-api", "db", "cache"} {
		if !strings.Contains(out, want) {
			t.Errorf("text status lacks %q:\n%s", want, out)
		}
	}
}

func TestAcceptance_TextDistinguishesHealthyFromUnhealthy(t *testing.T) {
	checks := map[string]bool{"db": true}
	if RenderStatus("svc", true, checks) == RenderStatus("svc", false, checks) {
		t.Error("a healthy and an unhealthy status render identically")
	}
}

func TestAcceptance_TextHandlesEmptyAndNilChecks(t *testing.T) {
	for _, c := range []map[string]bool{{}, nil} {
		if out := RenderStatus("svc", true, c); !strings.Contains(out, "svc") {
			t.Errorf("status with no checks lacks the name: %q", out)
		}
	}
}

// accWalk collects every (key, bool) pair and every string anywhere in a
// decoded JSON value, so the suite does not pin field names the prompt left open.
func accWalk(v any, bools map[string]bool, strs map[string]bool) {
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			if b, ok := e.(bool); ok {
				bools[k] = b
			}
			accWalk(e, bools, strs)
		}
	case []any:
		for _, e := range x {
			accWalk(e, bools, strs)
		}
	case string:
		strs[x] = true
	}
}

func TestAcceptance_JSONRoundTripsTheSameData(t *testing.T) {
	checks := map[string]bool{"db": true, "cache": false}
	for _, healthy := range []bool{true, false} {
		raw := RenderStatusJSON("billing-api", healthy, checks)
		var v any
		if err := json.Unmarshal([]byte(raw), &v); err != nil {
			t.Fatalf("not valid JSON: %v\n%s", err, raw)
		}
		bools, strs := map[string]bool{}, map[string]bool{}
		accWalk(v, bools, strs)
		if !strs["billing-api"] {
			t.Errorf("JSON lacks the name: %s", raw)
		}
		for name, want := range checks {
			got, ok := bools[name]
			if !ok || got != want {
				t.Errorf("JSON check %q = %v (present %v), want %v: %s", name, got, ok, want, raw)
			}
		}
		// The overall health must be carried as a boolean somewhere other
		// than the checks.
		found := false
		for k, b := range bools {
			if _, isCheck := checks[k]; !isCheck && b == healthy {
				found = true
			}
		}
		if !found {
			t.Errorf("JSON does not carry healthy=%v: %s", healthy, raw)
		}
	}
}
