package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/agentbench"
)

// fakeWhoamiDaemon answers the companion MCP whoami with the given database
// and project, and fails the test on any other tool: the pre-run sequence
// must submit nothing.
func fakeWhoamiDaemon(t *testing.T, database, project string) *httptest.Server {
	t.Helper()
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Params struct {
				Name string `json:"name"`
			} `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Params.Name != "whoami" {
			t.Errorf("pre-run sequence called %q; only whoami is expected", req.Params.Name)
		}
		payload, _ := json.Marshal(map[string]any{"database": database, "project_id": project})
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0", "id": 1,
			"result": map[string]any{"content": []map[string]any{{"type": "text", "text": string(payload)}}},
		})
	}))
	t.Cleanup(hs.Close)
	return hs
}

// GitHub #378 (2026.10.6 delta audit, #334): bench agent --project X with a
// companion key for project Y wiped X's store, ran every task in Y and
// recorded the arm as X. The key's project must be proved before the clear.
func TestPrepareBenchStore_RefusesKeyForAnotherProjectBeforeClear(t *testing.T) {
	prevDB, prevProject := benchAgentDatabase, benchAgentProject
	t.Cleanup(func() { benchAgentDatabase, benchAgentProject = prevDB, prevProject })
	benchAgentDatabase = "vornik_bench"

	cases := []struct {
		name        string
		keyProject  string
		wantRefusal bool
	}{
		{"key for another project", "real-project", true},
		{"key reports no project", "", true},
		{"key for the named project", "bench-agent", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			benchAgentProject = "bench-agent"
			hs := fakeWhoamiDaemon(t, "vornik_bench", tc.keyProject)
			daemon := agentbench.NewDaemonTaskRunner(agentbench.DaemonConfig{
				BaseURL: hs.URL, Token: "tok", Project: benchAgentProject, HTTPClient: hs.Client(),
			})
			clears := 0
			err := prepareBenchStore(context.Background(), daemon, func(context.Context) error {
				clears++
				return nil
			})
			if !tc.wantRefusal {
				require.NoError(t, err)
				assert.Equal(t, 1, clears)
				return
			}
			require.Error(t, err, "a key for %q must not run --project bench-agent", tc.keyProject)
			assert.Equal(t, 0, clears, "the store was cleared before the key's project was checked")
			assert.Contains(t, err.Error(), "bench-agent")
			if tc.keyProject != "" {
				assert.Contains(t, err.Error(), tc.keyProject)
			}
		})
	}
}
