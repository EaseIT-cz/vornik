package cli

// GitHub #621, LLD 28 B5: `vornikctl companion session-start` is the Cursor
// plugin's sessionStart hook. It prints the session's added context (the
// outstanding delegations and recent project memory) as JSON, calls only the
// tools the daemon offers, and never fails the session: any error prints a
// short note and exits 0.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeDigestCompanion offers the tools named, and records which were called.
func fakeDigestCompanion(t *testing.T, offer []string, called *[]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testCompanionKey {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				Name string `json:"name"`
			} `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		var result any
		switch req.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "serverInfo": map[string]any{"name": "vornik"}}
		case "tools/list":
			var tools []map[string]any
			for _, n := range offer {
				tools = append(tools, map[string]any{"name": n})
			}
			result = map[string]any{"tools": tools}
		case "tools/call":
			*called = append(*called, req.Params.Name)
			text := map[string]string{"list": `{"tasks":[{"task_id":"task_1","status":"COMPLETED"}]}`, "recent_memory": `{"hits":[{"snippet":"the parser bug was in the tokenizer"}]}`}[req.Params.Name]
			result = map[string]any{"content": []map[string]any{{"type": "text", "text": text}}}
		default:
			w.WriteHeader(http.StatusAccepted)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
	}))
}

func writeTestKeyFile(t *testing.T) {
	t.Helper()
	keyPath := filepath.Join(userConfigDir(t), "vornik", "companion", "default.key")
	if err := os.MkdirAll(filepath.Dir(keyPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, []byte(testCompanionKey+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func sessionStartOutput(t *testing.T, out []byte) string {
	t.Helper()
	var got struct {
		Context string `json:"additional_context"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("session-start must print one JSON object: %v\n%s", err, out)
	}
	return got.Context
}

func TestCompanionSessionStart_621_PrintsTheDigest(t *testing.T) {
	cursorHome(t)
	writeTestKeyFile(t)
	var called []string
	srv := fakeDigestCompanion(t, []string{"list", "recent_memory", "whoami"}, &called)
	defer srv.Close()
	companionDeskURL = srv.URL
	var out bytes.Buffer
	if err := runCompanionSessionStart(context.Background(), &out); err != nil {
		t.Fatalf("session-start never fails: %v", err)
	}
	ctx := sessionStartOutput(t, out.Bytes())
	for _, want := range []string{"task_1", "COMPLETED", "the parser bug was in the tokenizer"} {
		if !strings.Contains(ctx, want) {
			t.Errorf("digest misses %q:\n%s", want, ctx)
		}
	}
	if strings.Contains(out.String(), testCompanionKey) {
		t.Fatal("the digest must not carry the key")
	}
	if strings.Join(called, ",") != "list,recent_memory" {
		t.Fatalf("called %v, want list then recent_memory", called)
	}
}

// LLD 21 §D2 item 9: a tool the daemon does not offer is not called.
func TestCompanionSessionStart_621_CallsOnlyOfferedTools(t *testing.T) {
	cursorHome(t)
	writeTestKeyFile(t)
	var called []string
	srv := fakeDigestCompanion(t, []string{"recent_memory"}, &called)
	defer srv.Close()
	companionDeskURL = srv.URL
	var out bytes.Buffer
	if err := runCompanionSessionStart(context.Background(), &out); err != nil {
		t.Fatal(err)
	}
	if strings.Join(called, ",") != "recent_memory" {
		t.Fatalf("called %v, want only the offered recent_memory", called)
	}
}

// A missing key or an unreachable daemon prints a short note, never an error
// that would surface as a failed hook.
func TestCompanionSessionStart_621_NeverFails(t *testing.T) {
	cursorHome(t)
	companionDeskURL = "http://127.0.0.1:1"
	var out bytes.Buffer
	if err := runCompanionSessionStart(context.Background(), &out); err != nil {
		t.Fatalf("session-start must not fail: %v", err)
	}
	ctx := sessionStartOutput(t, out.Bytes())
	if !strings.Contains(ctx, "recent_memory") {
		t.Fatalf("the fallback note must point at recent_memory: %q", ctx)
	}
	// Review 737d finding 3: the model's context gets a fixed note, not the
	// error's detail (here the daemon address).
	if strings.Contains(ctx, "127.0.0.1") {
		t.Fatalf("the fallback note carries error detail: %q", ctx)
	}
}
