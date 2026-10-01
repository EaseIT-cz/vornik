package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/repotest"
)

// Broker write-actions design §7a: delegate accepts notify {url, token}. A
// refused notify creates no task; an accepted one is stored against the new
// task; a store failure still returns the task, with push: not_registered.

type memPushConfigs struct {
	mu   sync.Mutex
	set  []persistence.A2APushConfig
	fail error
}

func (m *memPushConfigs) Set(_ context.Context, c persistence.A2APushConfig) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		return m.fail
	}
	m.set = append(m.set, c)
	return nil
}

func (m *memPushConfigs) Get(_ context.Context, id string) (*persistence.A2APushConfig, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, c := range m.set {
		if c.TaskID == id {
			cp := c
			return &cp, nil
		}
	}
	return nil, persistence.ErrNotFound
}

func TestMemPushConfigs_HonoursTheMissContract(t *testing.T) {
	repotest.AssertMissRepo(t, "A2APushConfigRepository.Get", (&memPushConfigs{}).Get)
}

func brokerDelegateArgs(notify map[string]any) map[string]any {
	args := map[string]any{"workflow": "mail-digest", "inputs": map[string]any{"since": "2026-09-28T00:00:00Z"}}
	if notify != nil {
		args["notify"] = notify
	}
	return args
}

func TestDelegateNotify_RefusedCreatesNoTask(t *testing.T) {
	srv, keyRepo, taskRepo := newBrokerMCPServer(t)
	cfgs := &memPushConfigs{}
	srv.companionPushConfigs = cfgs
	raw, _ := seedCompanionKey(t, keyRepo, "broker-acme", nil)
	for _, notify := range []map[string]any{
		{"url": "http://127.0.0.1:9000/hook"},
		{"url": "http://192.168.1.5/hook"}, // private, project lists nothing
		{"url": "ftp://hooks.example/"},
		{"url": "https://hooks.example/x", "token": "has space"},
		{"url": "https://hooks.example/x", "token": strings.Repeat("a", 513)},
	} {
		text, isErr := callTool(t, srv, raw, "delegate", brokerDelegateArgs(notify))
		assert.True(t, isErr, "%v: %s", notify, text)
		assert.Contains(t, text, "NOTIFY_REJECTED", "%v", notify)
	}
	assert.Equal(t, 0, taskRepo.CallCount.Create, "a refused notify must create nothing")
	assert.Empty(t, cfgs.set)
}

func TestDelegateNotify_RegisteredAgainstTheNewTask(t *testing.T) {
	srv, keyRepo, taskRepo := newBrokerMCPServer(t)
	cfgs := &memPushConfigs{}
	srv.companionPushConfigs = cfgs
	raw, _ := seedCompanionKey(t, keyRepo, "broker-acme", nil)
	text, isErr := callTool(t, srv, raw, "delegate", brokerDelegateArgs(map[string]any{"url": "https://hooks.example/vornik", "token": "tok-1"}))
	require.False(t, isErr, text)
	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(text), &out))
	assert.Equal(t, "registered", out["push"])
	require.Len(t, cfgs.set, 1)
	assert.Equal(t, out["task_id"], cfgs.set[0].TaskID)
	assert.Equal(t, "https://hooks.example/vornik", cfgs.set[0].URL)
	assert.Equal(t, "tok-1", cfgs.set[0].Token)
	assert.Equal(t, 1, taskRepo.CallCount.Create)

	// Without notify the response has no push key.
	text, _ = callTool(t, srv, raw, "delegate", brokerDelegateArgs(nil))
	out = map[string]any{}
	require.NoError(t, json.Unmarshal([]byte(text), &out))
	assert.NotContains(t, out, "push")
}

func TestDelegateNotify_StoreFailureStillReturnsTheTask(t *testing.T) {
	srv, keyRepo, _ := newBrokerMCPServer(t)
	srv.companionPushConfigs = &memPushConfigs{fail: errors.New("db down")}
	raw, _ := seedCompanionKey(t, keyRepo, "broker-acme", nil)
	text, isErr := callTool(t, srv, raw, "delegate", brokerDelegateArgs(map[string]any{"url": "https://hooks.example/vornik"}))
	require.False(t, isErr, text)
	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(text), &out))
	assert.Equal(t, "not_registered", out["push"])
	assert.NotEmpty(t, out["task_id"])
}

// A daemon without push refuses notify rather than silently dropping it.
func TestDelegateNotify_UnsupportedIsRefused(t *testing.T) {
	srv, keyRepo, taskRepo := newBrokerMCPServer(t)
	raw, _ := seedCompanionKey(t, keyRepo, "broker-acme", nil)
	text, isErr := callTool(t, srv, raw, "delegate", brokerDelegateArgs(map[string]any{"url": "https://hooks.example/vornik"}))
	assert.True(t, isErr, text)
	assert.Contains(t, text, "NOTIFY_REJECTED")
	assert.Equal(t, 0, taskRepo.CallCount.Create)
}

// The ordinary (non-broker) delegate path takes notify too.
func TestDelegateNotify_OrdinaryDelegate(t *testing.T) {
	srv, keyRepo, taskRepo := newBrokerMCPServer(t)
	cfgs := &memPushConfigs{}
	srv.companionPushConfigs = cfgs
	raw, _ := seedCompanionKey(t, keyRepo, "memory-acme", []string{"wf-plain"})
	text, isErr := callTool(t, srv, raw, "delegate", map[string]any{
		"workflow": "wf-plain", "prompt": "do the plain thing",
		"notify": map[string]any{"url": "https://hooks.example/plain"},
	})
	require.False(t, isErr, text)
	assert.Contains(t, text, `"push": "registered"`)
	require.Len(t, cfgs.set, 1)
	assert.Equal(t, 1, taskRepo.CallCount.Create)
}

func TestGetCapabilities_CompanionPushFollowsWiring(t *testing.T) {
	srv, _, _ := newCompanionMCPServer(t)
	get := func() bool {
		rec := httptest.NewRecorder()
		srv.GetCapabilities(rec, httptest.NewRequest(http.MethodGet, "/api/v1/capabilities", nil))
		v, ok := decodeCapabilities(t, rec.Body.Bytes()).Features["companion-push"]
		require.True(t, ok, "the flag must always be present")
		return v
	}
	assert.False(t, get())
	srv.companionPushConfigs = &memPushConfigs{}
	assert.True(t, get())
}
