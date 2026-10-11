package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/registry"
)

// GitHub #100 (2026-10-03, task_20261003135506_c7bec90cfe425d9e): a response
// cut off at the output cap was visible only in one step's result.json
// (usage.output_cap_hits). The proxy now counts finish_reason=length per
// (role, model): vornik_chat_proxy_length_cutoffs_total. Design:
// 09-agent-runtime-contract.md §8.4, amendment 2026-10-08 (D5).

func finishStub(reason string) *overridableStubProvider {
	st := okStub()
	st.resp.Choices[0].FinishReason = reason
	return &overridableStubProvider{stubProvider: *st, model: "route-model"}
}

// lengthServer serves project "p" whose swarm declares the roles writer and
// critic (models route-model and other-model).
func lengthServer() (*Server, *APIMetrics) {
	reg := registry.New()
	registry.SeedForTest(reg, map[string]*registry.Project{"p": {ID: "p", SwarmID: "sw"}})
	registry.SeedSwarmsForTest(reg, map[string]*registry.Swarm{"sw": {ID: "sw", Roles: []registry.SwarmRole{
		{Name: "writer", Model: "route-model"},
		{Name: "critic", Model: "other-model", ModelFallback: "fallback-model"},
	}}})
	m := NewAPIMetrics(prometheus.NewRegistry())
	return &Server{logger: zerolog.Nop(), chatProvider: finishStub("length"), apiMetrics: m, projectRegistry: reg}, m
}

func postChat(t *testing.T, s *Server, project, role, model string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat/completions",
		strings.NewReader(`{"model":"`+model+`","messages":[{"role":"user","content":"hi"}]}`))
	if project != "" {
		req.Header.Set("X-Vornik-Project-ID", project)
	}
	if role != "" {
		req.Header.Set("X-Vornik-Role", role)
	}
	w := httptest.NewRecorder()
	s.ChatCompletions(w, req)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
}

func TestChatProxy_LengthFinishIncrementsCounterByRoleAndModel(t *testing.T) {
	s, m := lengthServer()
	postChat(t, s, "p", "writer", "route-model")
	require.Equal(t, 1.0, testutil.ToFloat64(m.ChatProxyLengthCutoffsTotal.WithLabelValues("writer", "route-model")))

	s.chatProvider = finishStub("stop")
	postChat(t, s, "p", "writer", "route-model")
	s.chatProvider = finishStub("tool_calls")
	postChat(t, s, "p", "writer", "route-model")
	require.Equal(t, 1.0, testutil.ToFloat64(m.ChatProxyLengthCutoffsTotal.WithLabelValues("writer", "route-model")),
		"stop and tool_calls finishes must not count")
	require.Equal(t, 1, testutil.CollectAndCount(m.ChatProxyLengthCutoffsTotal), "only the one length series exists")
}

func TestChatProxy_InvalidRoleHeaderCountsAsOther(t *testing.T) {
	s, m := lengthServer()
	postChat(t, s, "p", "bad:role", "route-model")
	postChat(t, s, "p", "", "route-model")
	postChat(t, s, "", "writer", "route-model") // no project: no declared roles
	require.Equal(t, 3.0, testutil.ToFloat64(m.ChatProxyLengthCutoffsTotal.WithLabelValues("other", "route-model")))
}

// Security review (2026-10-08, controller finding on 4f3ac3806): the role
// label came from the caller-supplied X-Vornik-Role, which validCacheRole
// checks only for form, and the model label from the request body via the
// override, so an agent container could mint unbounded Prometheus series.
// Both labels are now bounded by the calling project's swarm: N distinct
// role headers create at most (declared roles + 1) series per model, and an
// undeclared model collapses to "other".
func TestChatProxy_LengthCounterLabelsAreBoundedByTheSwarm(t *testing.T) {
	s, m := lengthServer()
	for i := 0; i < 50; i++ {
		postChat(t, s, "p", fmt.Sprintf("minted-%d", i), "route-model")
		postChat(t, s, "p", "writer", fmt.Sprintf("minted-model-%d", i))
	}
	postChat(t, s, "p", "critic", "other-model")
	postChat(t, s, "p", "critic", "fallback-model")

	series := testutil.CollectAndCount(m.ChatProxyLengthCutoffsTotal)
	// roles: writer, critic, other; models: route-model, other-model, fallback-model, other.
	require.LessOrEqual(t, series, 3*4, "series must be bounded by declared roles and models")
	require.Equal(t, 50.0, testutil.ToFloat64(m.ChatProxyLengthCutoffsTotal.WithLabelValues("other", "route-model")))
	require.Equal(t, 50.0, testutil.ToFloat64(m.ChatProxyLengthCutoffsTotal.WithLabelValues("writer", "other")))
	require.Equal(t, 1.0, testutil.ToFloat64(m.ChatProxyLengthCutoffsTotal.WithLabelValues("critic", "other-model")))
	require.Equal(t, 1.0, testutil.ToFloat64(m.ChatProxyLengthCutoffsTotal.WithLabelValues("critic", "fallback-model")))
	require.Equal(t, 4, series)
}

func TestChatProxy_LengthCounterNilMetricsIsSafe(t *testing.T) {
	s, _ := lengthServer()
	s.apiMetrics = nil
	postChat(t, s, "p", "writer", "route-model")
	var nilMetrics *APIMetrics
	nilMetrics.RecordChatProxyLengthCutoff("writer", "m")
}
