package cli

// GitHub #669 item 3 (design 22, §22.2.16b): the verbs call the operator
// door, print the full owner the recover verb needs, and pass the scope,
// epoch and owner through unchanged. Found at: production (2026-10-28,
// recovered by hand in SQL). Regression test at: unit (a fake door). Lower
// because the door and the CAS behind it are tested at integration.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAccountTransitions669_ListAndRecover(t *testing.T) {
	owner := "0123456789abcdef0123456789abcdef"
	var recovered map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "GET /api/v1/operator/account-transitions":
			_ = json.NewEncoder(w).Encode(map[string]any{"rows": []map[string]any{{"scope": "ns:hermes", "epoch": 4, "owner": owner,
				"operation": "connection-config", "ageSeconds": 3600, "heldHere": false}}, "count": 1, "examined": 3})
		case "POST /api/v1/operator/account-transitions/recover":
			_ = json.NewDecoder(r.Body).Decode(&recovered)
			_ = json.NewEncoder(w).Encode(map[string]any{"recovered": true, "epochBefore": 4, "epochAfter": 5, "operation": "connection-config",
				"note": "suspended grants stay suspended; confirm them on the approver device"})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	t.Setenv("VORNIK_API_KEY", "sk-vornik-test")

	out, err := captureStdoutFn(t, runAccountTransitionsList)
	if err != nil || !strings.Contains(out, owner) || !strings.Contains(out, "1 busy of 3 rows examined") {
		t.Fatalf("list printed %q (err %v)", out, err)
	}
	out, err = captureStdoutFn(t, func() error { return runAccountTransitionsRecover("ns:hermes", "4", owner) })
	if err != nil || !strings.Contains(out, "epoch is now 5") {
		t.Fatalf("recover printed %q (err %v)", out, err)
	}
	if recovered["scope"] != "ns:hermes" || recovered["owner"] != owner || recovered["epoch"] != float64(4) {
		t.Errorf("recover sent %v", recovered)
	}
	if err := runAccountTransitionsRecover("ns:hermes", "four", owner); err == nil {
		t.Error("a non-numeric epoch was sent")
	}
}
