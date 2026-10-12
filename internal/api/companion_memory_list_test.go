package api

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/apikey"
	"vornik.io/vornik/internal/memory"
)

// GitHub #119 item 6 (2026-10-08): the user sees what is kept, paged, with
// source and age, from any harness (LLD 22, "Memory the user can see").

// fakeMemoryList is a MemoryListAdapter over in-memory rows per project, with
// the repository's keyset order (created_at DESC, id DESC).
type fakeMemoryList struct {
	*fakeMemoryCompanion
	rows     map[string][]MemoryListEntry
	requests []MemoryListRequest
}

func (f *fakeMemoryList) ListMemory(_ context.Context, in MemoryListRequest) (MemoryListPage, error) {
	f.requests = append(f.requests, in)
	after, err := memory.DecodeChunkCursor(in.Cursor)
	if err != nil {
		return MemoryListPage{}, err
	}
	all := append([]MemoryListEntry(nil), f.rows[in.ProjectID]...)
	sort.Slice(all, func(i, j int) bool {
		if !all[i].CreatedAt.Equal(all[j].CreatedAt) {
			return all[i].CreatedAt.After(all[j].CreatedAt)
		}
		return all[i].ChunkID > all[j].ChunkID
	})
	var page []MemoryListEntry
	for _, e := range all {
		if after != nil {
			older := e.CreatedAt.Before(after.CreatedAt) || (e.CreatedAt.Equal(after.CreatedAt) && e.ChunkID < after.ID)
			if !older {
				continue
			}
		}
		page = append(page, e)
		if len(page) == in.Limit {
			break
		}
	}
	out := MemoryListPage{Entries: page, Examined: len(page)}
	if len(page) == in.Limit {
		last := page[len(page)-1]
		out.NextCursor = memory.EncodeChunkCursor(memory.ChunkCursor{CreatedAt: last.CreatedAt, ID: last.ChunkID})
	}
	return out, nil
}

func seedMemoryRows(project string, n int, base time.Time) []MemoryListEntry {
	out := make([]MemoryListEntry, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, MemoryListEntry{
			ChunkID:    fmt.Sprintf("%s-%04d", project, i),
			SourceName: "companion:hermes:note",
			Content:    fmt.Sprintf("[MEMORY] fact number %d about %s ⟦vm:0123456789abcdef⟧", i, project),
			CreatedAt:  base.Add(time.Duration(i) * time.Second),
		})
	}
	return out
}

type memoryListOut struct {
	ProjectID  string `json:"project_id"`
	NextCursor string `json:"next_cursor"`
	Examined   int    `json:"examined"`
	Returned   int    `json:"returned"`
	ExportURL  string `json:"export_url"`
	Items      []struct {
		ID           string `json:"id"`
		ShortID      string `json:"short_id"`
		CreatedAt    string `json:"created_at"`
		Age          string `json:"age"`
		Source       string `json:"source"`
		Origin       string `json:"origin"`
		ContentClass string `json:"content_class"`
		Snippet      string `json:"snippet"`
	} `json:"items"`
}

func memoryListServer(t *testing.T) (*Server, *fakeMemoryList, string) {
	t.Helper()
	srv, keyRepo, _ := newCompanionMCPServer(t)
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	f := &fakeMemoryList{fakeMemoryCompanion: &fakeMemoryCompanion{}, rows: map[string][]MemoryListEntry{
		"alpha": seedMemoryRows("alpha", 250, base),
		"beta":  seedMemoryRows("beta", 30, base.Add(time.Hour)),
	}}
	srv.memoryCompanion = f
	srv.publicOrigin = func() string { return "https://vornik.example" }
	raw, _ := seedCompanionKeyWithCaps(t, keyRepo, "alpha", nil, true, false)
	return srv, f, raw
}

func callMemoryList(t *testing.T, srv *Server, raw string, args map[string]any) memoryListOut {
	t.Helper()
	text, isErr := callTool(t, srv, raw, "memory_list", args)
	require.False(t, isErr, text)
	var out memoryListOut
	require.NoError(t, json.Unmarshal([]byte(text), &out), text)
	return out
}

func TestMemoryList_PagesNewestFirstWithCursor(t *testing.T) {
	srv, _, raw := memoryListServer(t)
	seen := map[string]bool{}
	var sizes []int
	var prev time.Time
	cursor := ""
	for page := 0; page < 5; page++ {
		args := map[string]any{"limit": 100}
		if cursor != "" {
			args["cursor"] = cursor
		}
		out := callMemoryList(t, srv, raw, args)
		sizes = append(sizes, len(out.Items))
		for _, it := range out.Items {
			require.False(t, seen[it.ID], "duplicate %s", it.ID)
			seen[it.ID] = true
			at, err := time.Parse(time.RFC3339, it.CreatedAt)
			require.NoError(t, err)
			if !prev.IsZero() {
				require.False(t, at.After(prev), "not newest first")
			}
			prev = at
		}
		cursor = out.NextCursor
		if cursor == "" {
			break
		}
	}
	assert.Equal(t, []int{100, 100, 50}, sizes)
	assert.Len(t, seen, 250)
}

func TestMemoryList_StripsMirrorTokens(t *testing.T) {
	srv, f, raw := memoryListServer(t)
	f.rows["alpha"][249].SourceName = "companion:hermes:note ⟦vm:0123456789abcdef⟧"
	out := callMemoryList(t, srv, raw, map[string]any{"limit": 5})
	b, _ := json.Marshal(out)
	assert.NotContains(t, string(b), "⟦vm:")
	assert.NotContains(t, string(b), "vm:0123")
	require.NotEmpty(t, out.Items)
	it := out.Items[0]
	assert.Equal(t, memory.OriginMirrored, it.Origin)
	assert.Equal(t, memory.ShortID(it.ID), it.ShortID)
	assert.NotEmpty(t, it.Age)
	assert.Equal(t, "companion:hermes:note", it.Source)
	assert.Equal(t, "https://vornik.example/ui/approve/memory/alpha", out.ExportURL)
}

// audit 2026-10-12: short_id was the id's first 12 characters, so two
// correction chunks (chunk_YYYYMMDDhhmmss_<hex>) from the same month shared
// "chunk_202610" (design 22, "Short ids for every id shape").
func TestMemoryList_CorrectionRowsGetDistinctShortIDs(t *testing.T) {
	now := time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC)
	a := shapeMemoryListItem(MemoryListEntry{ChunkID: "chunk_20261012101500_00112233aabbccdd", CreatedAt: now}, now)
	b := shapeMemoryListItem(MemoryListEntry{ChunkID: "chunk_20261003080000_44556677eeff0011", CreatedAt: now}, now)
	assert.NotEqual(t, a.ShortID, b.ShortID, "two correction rows from one month share a short id")
	assert.Regexp(t, `^[0-9a-f]{12}$`, a.ShortID)
	h := shapeMemoryListItem(MemoryListEntry{ChunkID: "0123456789abcdef0123456789abcdef", CreatedAt: now}, now)
	assert.Equal(t, "0123456789ab", h.ShortID, "a hex id keeps the short id users have seen")
}

func TestMemoryList_OtherProjectUnreachable(t *testing.T) {
	srv, f, raw := memoryListServer(t)
	beta := callMemoryList(t, srv, raw, map[string]any{"limit": 1}) // a cursor from alpha's own listing...
	_ = beta
	// ...and a cursor positioned in beta's rows: it is a position, never a project.
	betaRows := f.rows["beta"]
	foreign := memory.EncodeChunkCursor(memory.ChunkCursor{CreatedAt: betaRows[len(betaRows)-1].CreatedAt.Add(time.Hour), ID: "zzz"})
	out := callMemoryList(t, srv, raw, map[string]any{"limit": 100, "cursor": foreign})
	for _, it := range out.Items {
		assert.True(t, strings.HasPrefix(it.ID, "alpha-"), "row %s of another project", it.ID)
	}
	for _, r := range f.requests {
		assert.Equal(t, "alpha", r.ProjectID)
	}
	assert.Equal(t, "alpha", out.ProjectID)
}

func TestMemoryList_RefusesWithoutMemoryReadAndOnBadArguments(t *testing.T) {
	srv, keyRepo, _ := newCompanionMCPServer(t)
	srv.memoryCompanion = &fakeMemoryList{fakeMemoryCompanion: &fakeMemoryCompanion{}}
	noRead, _ := seedCompanionKeyWithCaps(t, keyRepo, "gamma", nil, false, false)
	text, isErr := callTool(t, srv, noRead, "memory_list", map[string]any{})
	require.True(t, isErr)
	assert.Contains(t, text, "memory_read")

	srv2, _, raw := memoryListServer(t)
	for _, args := range []map[string]any{
		{"cursor": "%%%"},
		{"limit": 101},
		{"limit": 0},
		{"project": "beta"},
		{"query": "tea", "cursor": memory.EncodeChunkCursor(memory.ChunkCursor{CreatedAt: time.Now(), ID: "x"})},
	} {
		text, isErr := callTool(t, srv2, raw, "memory_list", args)
		assert.True(t, isErr, "args %v: %s", args, text)
	}
}

func TestMemoryList_QueryUsesTheRecallPathOnTheKeysProject(t *testing.T) {
	srv, f, raw := memoryListServer(t)
	f.recallReturn = []MemorySearchResult{{ChunkID: "alpha-hit-000001", SourceName: "companion:claude-code:note", Content: "tea is good", CreatedAt: "2026-10-01T00:00:00Z"}}
	out := callMemoryList(t, srv, raw, map[string]any{"query": "tea", "limit": 10})
	require.Len(t, f.recallCalls, 1)
	assert.Equal(t, "alpha", f.recallCalls[0].ProjectID)
	require.Len(t, out.Items, 1)
	assert.Equal(t, memory.OriginRemembered, out.Items[0].Origin)
	assert.Empty(t, out.NextCursor)

	// One page whatever the adapter returns (review 6172 F2).
	f.recallReturn = nil
	for i := 0; i < 60; i++ {
		f.recallReturn = append(f.recallReturn, MemorySearchResult{ChunkID: fmt.Sprintf("alpha-hit-%06d", i), Content: "tea"})
	}
	out = callMemoryList(t, srv, raw, map[string]any{"query": "tea", "limit": 10})
	assert.Len(t, out.Items, 10)
	assert.Equal(t, 10, f.recallCalls[len(f.recallCalls)-1].Opts.Limit)
}

// Review 6172 F7: the agent-admin refusal (companionMemoryTools) and the
// broker-project refusal (brokerProjectTools) must agree that no memory tool
// is ever allowed to a broker-project key.
func TestMemoryTools_NeverInTheBrokerAllowlist(t *testing.T) {
	n := 0
	for tool := range companionMemoryTools {
		assert.False(t, brokerProjectTools[tool], "%s is a memory tool in the broker allowlist", tool)
		n++
	}
	defs := map[string]bool{}
	for _, d := range companionToolDefs() {
		defs[d.Name] = true
	}
	for tool := range companionMemoryTools {
		assert.True(t, defs[tool], "%s is listed as a memory tool but is not a companion tool", tool)
	}
	t.Logf("examined %d memory tools", n)
}

func TestMemoryList_BrokerAndAdminKeysNotOffered(t *testing.T) {
	srv, keys, _ := newBrokerMCPServer(t)
	srv.memoryCompanion = &fakeMemoryList{fakeMemoryCompanion: &fakeMemoryCompanion{}}
	srv.agentAdmin, srv.agentAdminEnabled = &fakeAgentAdmin{}, func() bool { return true }
	brokerRaw, _ := seedCompanionKeyWithCaps(t, keys, "broker-acme", nil, true, true)
	adminRaw, _ := seedCompanionKeyWithCaps(t, keys, "broker-acme", nil, true, true)
	keys.rows[len(keys.rows)-1].AgentAdmin, keys.rows[len(keys.rows)-1].AgentNamespace = true, "acme"
	// An agent-admin key whose home project is absent from the registry
	// reads as a plain key to isBrokerProjectKey (A0 record); the gate now
	// refuses memory tools by the key's kind.
	ghostRaw, _ := seedCompanionKeyWithCaps(t, keys, "ghost--home", nil, true, true)
	keys.rows[len(keys.rows)-1].AgentAdmin, keys.rows[len(keys.rows)-1].AgentNamespace = true, "ghost"
	memRaw, _ := seedCompanionKeyWithCaps(t, keys, "memory-acme", nil, true, true)
	ghostKey, err := keys.LookupActiveByHash(context.Background(), apikey.Hash(ghostRaw))
	require.NoError(t, err)
	require.True(t, ghostKey.AgentAdmin)
	require.False(t, srv.isBrokerProjectKey(ghostKey), "test setup: the ghost key's home must be unregistered")

	assert.True(t, listTools(t, srv, memRaw)["memory_list"], "a plain memory key is offered memory_list")
	for name, raw := range map[string]string{"broker": brokerRaw, "admin": adminRaw, "admin-unregistered-home": ghostRaw} {
		tools := listTools(t, srv, raw)
		for _, tool := range []string{"memory_list", "recall", "remember", "recent_memory", "list_scopes", "memory_correct"} {
			assert.False(t, tools[tool], "%s key offered %s", name, tool)
		}
		_, isErr := callTool(t, srv, raw, "memory_list", map[string]any{})
		assert.True(t, isErr, "%s key may call memory_list", name)
	}
}

func TestMemoryList_CapabilityAdvertisedOnlyWithAListingAdapter(t *testing.T) {
	srv, _, _ := memoryListServer(t)
	assert.True(t, capabilitiesAs(t, srv, "", false).Features["companion-memory-list"])
	srv.memoryCompanion = &fakeMemoryCompanion{}
	assert.False(t, capabilitiesAs(t, srv, "", false).Features["companion-memory-list"])
}
