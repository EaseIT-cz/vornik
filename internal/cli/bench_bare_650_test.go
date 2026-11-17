package cli

// vornikctl bench agent bare (benchmark design, Amendment 2026-11-16, GitHub
// #650): the bare-model baseline's model client and workspace, through the
// same seams the swarm arm's grading uses.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"vornik.io/vornik/internal/agentbench"
	"vornik.io/vornik/internal/spawn"
)

func TestHTTPBareModel_SendsTheMessagesAndReadsTheReplyAndTokens_650(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer k" {
			http.Error(w, "wrong request", http.StatusBadRequest)
			return
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"FILE: leap.go\n` + "```" + `\npackage leap\n` + "```" + `"}}],"usage":{"total_tokens":321}}`))
	}))
	defer srv.Close()
	m := httpBareModel{endpoint: srv.URL + "/v1", model: "qwen3.8-27b", apiKey: "k"}
	reply, err := m.Complete(context.Background(), []agentbench.BareMessage{{Role: "user", Content: "solve"}})
	if err != nil {
		t.Fatal(err)
	}
	if reply.Tokens != 321 || !strings.Contains(reply.Text, "package leap") {
		t.Fatalf("reply = %+v", reply)
	}
	if got["model"] != "qwen3.8-27b" || !strings.Contains(mustMarshal(t, got["messages"]), "solve") {
		t.Fatalf("request = %v", got)
	}

	fail := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "overloaded", http.StatusServiceUnavailable)
	}))
	defer fail.Close()
	if _, err := (httpBareModel{endpoint: fail.URL, model: "m"}).Complete(context.Background(), nil); err == nil ||
		!strings.Contains(err.Error(), "503") {
		t.Fatalf("a failed call is an error naming the status: %v", err)
	}
}

func mustMarshal(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestGitBareWorkspace_GradesTheCommittedReplyNotTheStub_650(t *testing.T) {
	root := t.TempDir()
	if err := spawn.RegisterWorkspaceRoot(root); err != nil {
		t.Fatal(err)
	}
	spec := seededSpec(t) // pg-go-leap: seed leap.go (panic) and go.mod
	base := filepath.Dir(mustSeedDir(t, spec))
	if err := os.MkdirAll(filepath.Join(base, "suite"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "suite", "zz_acceptance_leap_test.go"), []byte("package leap\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	spec.Acceptance = "suite"
	ws, err := newGitBareWorkspace(context.Background(), root, spec, "localhost/vornik-agent:bench")
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	if err := ws.Write("leap.go", "package leap\n\nfunc IsLeapYear(y int) bool { return y%4 == 0 } // reply\n"); err != nil {
		t.Fatal(err)
	}
	if err := ws.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	var graded string
	ws.grader.run = func(_ context.Context, name string, args ...string) ([]byte, error) {
		if name != "podman" {
			return nil, nil
		}
		for i, a := range args {
			if a == "-v" && i+1 < len(args) {
				tree := strings.SplitN(args[i+1], ":", 2)[0]
				b, _ := os.ReadFile(filepath.Join(tree, "polyglot/go/leap/leap.go"))
				graded = string(b)
			}
		}
		return []byte("ok  \tleap\t0.01s"), nil
	}
	if res := ws.Grade(context.Background(), spec); res.Outcome != agentbench.AcceptancePassed {
		t.Fatalf("outcome = %s: %s", res.Outcome, res.Output)
	}
	if !strings.Contains(graded, "// reply") || strings.Contains(graded, "panic") {
		t.Fatalf("the grader saw %q, not the committed reply", graded)
	}
}

func mustSeedDir(t *testing.T, spec agentbench.TaskSpec) string {
	t.Helper()
	d, err := spec.SeedDir()
	if err != nil || d == "" {
		t.Fatalf("seed dir: %q %v", d, err)
	}
	return d
}

func TestBenchAgentBare_RefusesWithoutItsRequiredFlags_650(t *testing.T) {
	bareArmName, bareEndpoint, bareModel, bareImage = "", "", "", ""
	if err := runBenchAgentBare(benchAgentBareCmd, nil); err == nil || !strings.Contains(err.Error(), "--arm") {
		t.Fatalf("missing flags: %v", err)
	}
}
