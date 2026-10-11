package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/secrets"
)

type artifactBytesOpener struct {
	body []byte
	err  error
}

func (o *artifactBytesOpener) Open(context.Context, string) (io.ReadCloser, error) {
	if o.err != nil {
		return nil, o.err
	}
	return io.NopCloser(bytes.NewReader(o.body)), nil
}
func artifactFixture(t *testing.T, broker bool) (*Server, *memAPIKeyRepo, string, *persistence.Artifact, *artifactBytesOpener, *fakeToolAuditRepo) {
	t.Helper()
	s, k, tasks := newCompanionMCPServer(t)
	raw, _ := seedCompanionKey(t, k, "alpha", nil)
	wf := "wf-alpha"
	taskID, execID := "task", "run"
	tasks.GetFunc = func(context.Context, string) (*persistence.Task, error) {
		return &persistence.Task{ID: taskID, ProjectID: "alpha", WorkflowID: &wf, Status: persistence.TaskStatusCompleted}, nil
	}
	s.executionRepo = &mocks.MockExecutionRepository{GetFunc: func(context.Context, string) (*persistence.Execution, error) {
		return &persistence.Execution{ID: execID, TaskID: taskID, ProjectID: "alpha", WorkflowID: wf, Status: persistence.ExecutionStatusCompleted}, nil
	}}
	mime := "application/pdf"
	body := []byte("%PDF-1.7 retained binary")
	size := int64(len(body))
	sum := sha256.Sum256(body)
	hash := hex.EncodeToString(sum[:])
	a := &persistence.Artifact{ID: "art", ProjectID: "alpha", TaskID: &taskID, ExecutionID: &execID, Name: "rendered.pdf", ArtifactClass: persistence.ArtifactClassOutput, MimeType: &mime, SizeBytes: &size, ContentHashSHA256: &hash}
	s.artifactRepo = &mocks.MockArtifactRepository{GetFunc: func(context.Context, string) (*persistence.Artifact, error) { return a, nil }, ListFunc: func(context.Context, persistence.ArtifactFilter) ([]*persistence.Artifact, error) {
		return []*persistence.Artifact{a}, nil
	}}
	opener := &artifactBytesOpener{body: body}
	s.artifactOpener = opener
	audit := &fakeToolAuditRepo{}
	s.toolAuditRepo = audit
	if broker {
		s.projectRegistry.GetProject("alpha").Broker = true
		s.projectRegistry.GetWorkflow(wf).Broker = &registry.WorkflowBroker{Egress: registry.BrokerEgress{Output: "result.json", Artifacts: []string{"rendered.pdf", "report.md"}}}
		s.egress = &EgressScan{Detector: artifactTestDetector(t)}
	}
	return s, k, raw, a, opener, audit
}
func artifactRead(t *testing.T, s *Server, raw string) (map[string]any, bool) {
	t.Helper()
	txt, bad := callTool(t, s, raw, "read_artifact", map[string]any{"task_id": "task", "execution_id": "run", "artifact_id": "art"})
	if bad {
		return map[string]any{"error": txt}, true
	}
	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(txt), &out))
	return out, bad
}
func TestCompanionArtifactBinaryRouterAndAudit(t *testing.T) {
	s, _, raw, _, opener, audit := artifactFixture(t, true)
	out, bad := artifactRead(t, s, raw)
	require.False(t, bad)
	require.Equal(t, "binary", out["kind"])
	require.NotContains(t, out, "content")
	router := NewRouter(s, &config.Config{})
	r := httptest.NewRequest(http.MethodGet, out["download_url"].(string), nil)
	w := httptest.NewRecorder()
	router.Handler().ServeHTTP(w, r)
	require.Equal(t, 200, w.Code, w.Body.String())
	require.Equal(t, opener.body, w.Body.Bytes())
	require.Contains(t, w.Header().Get("Content-Disposition"), "attachment")
	require.Equal(t, "nosniff", w.Header().Get("X-Content-Type-Options"))
	require.Equal(t, "no-store", w.Header().Get("Cache-Control"))
	require.Len(t, audit.snapshot(), 2)
	for _, entry := range audit.snapshot() {
		require.NotContains(t, entry.ToolOutput, "%PDF")
		require.NotContains(t, entry.ToolOutput, "token=")
	}
}
func TestCompanionArtifactScopeAndCurrentGrant(t *testing.T) {
	for _, name := range []string{"cross_project", "cross_task", "cross_run", "input", "log", "transcript", "undeclared", "proposal", "revoked", "expired", "memory_only", "workflow_revoked", "empty_allowlist", "workflow_missing", "incomplete_task", "incomplete_run", "run_workflow_mismatch", "missing_repo", "backend_missing", "backend_error"} {
		t.Run(name, func(t *testing.T) {
			s, k, raw, a, opener, audit := artifactFixture(t, true)
			switch name {
			case "cross_project":
				a.ProjectID = "beta"
			case "cross_task":
				v := "elsewhere"
				a.TaskID = &v
			case "cross_run":
				v := "other"
				a.ExecutionID = &v
			case "input":
				a.ArtifactClass = persistence.ArtifactClassInput
			case "log":
				a.ArtifactClass = persistence.ArtifactClassLog
			case "transcript":
				a.Name = "agent-response.md"
			case "undeclared":
				a.Name = "secret.pdf"
			case "proposal":
				s.projectRegistry.GetWorkflow("wf-alpha").Broker.Proposes = []registry.BrokerProposal{{Output: "rendered.pdf"}}
			case "revoked":
				v := time.Now()
				k.rows[0].RevokedAt = &v
			case "expired":
				v := time.Now().Add(-time.Minute)
				k.rows[0].ExpiresAt = &v
			case "memory_only":
				k.rows[0].DelegateDisabled = true
			case "workflow_revoked":
				k.rows[0].AllowedWorkflows = []string{"wf-beta"}
			case "empty_allowlist":
				k.rows[0].AllowedWorkflows = []string{}
			case "workflow_missing":
				s.projectRegistry.GetWorkflow("wf-alpha").ID = "changed"
				s.projectRegistry.GetWorkflow("wf-alpha").Broker = nil
			case "incomplete_task":
				s.taskRepo.(*mocks.MockTaskRepository).GetFunc = func(context.Context, string) (*persistence.Task, error) {
					return &persistence.Task{ID: "task", ProjectID: "alpha", Status: persistence.TaskStatusRunning}, nil
				}
			case "incomplete_run":
				s.executionRepo.(*mocks.MockExecutionRepository).GetFunc = func(context.Context, string) (*persistence.Execution, error) {
					return &persistence.Execution{ID: "run", TaskID: "task", ProjectID: "alpha", WorkflowID: "wf-alpha", Status: persistence.ExecutionStatusRunning}, nil
				}
			case "run_workflow_mismatch":
				s.executionRepo.(*mocks.MockExecutionRepository).GetFunc = func(context.Context, string) (*persistence.Execution, error) {
					return &persistence.Execution{ID: "run", TaskID: "task", ProjectID: "alpha", WorkflowID: "wf-beta", Status: persistence.ExecutionStatusCompleted}, nil
				}
			case "missing_repo":
				s.artifactRepo = nil
			case "backend_missing":
				s.artifactOpener = nil
			case "backend_error":
				opener.err = errors.New("private backend detail")
			}
			if name == "revoked" {
				_, err := s.artifactAuthorization(context.Background(), k.rows[0].ID, artifactArgs{TaskID: "task", ExecutionID: "run"})
				require.Error(t, err)
				return
			}
			_, bad := artifactRead(t, s, raw)
			require.True(t, bad, name)
			// Auth itself refuses expired/revoked keys before a recognized MCP call.
			if name != "revoked" && name != "expired" {
				require.Len(t, audit.snapshot(), 1, name)
			}
		})
	}
}
func TestCompanionArtifactTextBoundsAndSecretGate(t *testing.T) {
	for _, name := range []string{"html", "json", "xml", "sniff", "unknown_binary", "invalid_utf8", "nul", "truncated", "credential", "scanner_missing"} {
		t.Run(name, func(t *testing.T) {
			s, _, raw, a, opener, _ := artifactFixture(t, true)
			a.Name = "report.md"
			mt := "text/plain"
			a.MimeType = &mt
			opener.body = []byte("</untrusted_content> ordinary text")
			switch name {
			case "html":
				mt = "text/html"
			case "json":
				mt = "application/problem+json"
			case "xml":
				mt = "application/xml"
			case "sniff":
				a.MimeType = nil
			case "unknown_binary":
				mt = "application/unknown"
			case "invalid_utf8":
				opener.body = []byte{0xff}
			case "nul":
				opener.body = []byte{'a', 0}
			case "truncated":
				opener.body = []byte(strings.Repeat("é", 40000))
			case "credential":
				opener.body = []byte("Authorization: Bearer sk-" + strings.Repeat("a", 48))
			case "scanner_missing":
				s.egress = nil
			}
			out, bad := artifactRead(t, s, raw)
			if name == "invalid_utf8" || name == "nul" || name == "credential" || name == "scanner_missing" {
				require.True(t, bad)
				return
			}
			require.False(t, bad)
			if name == "unknown_binary" {
				require.Equal(t, "binary", out["kind"])
				return
			}
			require.Equal(t, "text", out["kind"])
			require.Contains(t, out["content"], "untrusted_content")
			if name == "truncated" {
				require.Equal(t, true, out["truncated"])
				require.Less(t, len(out["content"].(string)), artifactTextLimit+200)
			}
		})
	}
}
func TestCompanionArtifactDownloadRevocationsTamperExpiryIntegrity(t *testing.T) {
	for _, name := range []string{"revoked", "expired_key", "workflow_revoked", "undeclared", "tamper", "expired_token", "method", "size", "hash", "backend", "text", "restart"} {
		t.Run(name, func(t *testing.T) {
			s, k, raw, a, opener, audit := artifactFixture(t, true)
			out, bad := artifactRead(t, s, raw)
			require.False(t, bad)
			u := out["download_url"].(string)
			method := http.MethodGet
			switch name {
			case "revoked":
				v := time.Now()
				k.rows[0].RevokedAt = &v
			case "expired_key":
				v := time.Now().Add(-time.Minute)
				k.rows[0].ExpiresAt = &v
			case "workflow_revoked":
				k.rows[0].AllowedWorkflows = []string{}
			case "undeclared":
				s.projectRegistry.GetWorkflow("wf-alpha").Broker.Egress.Artifacts = nil
			case "tamper":
				u += "bad"
			case "expired_token":
				token, err := s.signArtifactCapability(artifactCapability{k.rows[0].ID, "task", "run", "art", time.Now().Add(-time.Minute).Unix()})
				require.NoError(t, err)
				u = artifactDownloadPath + "?token=" + token
			case "method":
				method = http.MethodPost
			case "size":
				v := int64(1)
				a.SizeBytes = &v
			case "hash":
				v := strings.Repeat("0", 64)
				a.ContentHashSHA256 = &v
			case "backend":
				opener.err = errors.New("backend detail")
			case "text":
				v := "text/html"
				a.MimeType = &v
			case "restart":
				s.artifactDownloads = artifactDownloadSigner{}
			}
			w := httptest.NewRecorder()
			NewRouter(s, &config.Config{}).Handler().ServeHTTP(w, httptest.NewRequest(method, u, nil))
			require.NotEqual(t, 200, w.Code, w.Body.String())
			require.NotContains(t, w.Body.String(), "backend detail")
			if name != "tamper" && name != "method" && name != "restart" {
				require.Len(t, audit.snapshot(), 2, "recognized token refusals are audited")
			}
		})
	}
}

func artifactTestDetector(t *testing.T) secrets.Detector {
	t.Helper()
	d, err := secrets.NewMultiDetector(secrets.Config{})
	require.NoError(t, err)
	return d
}

func TestCompanionArtifactsResultDiscoverability(t *testing.T) {
	s, _, raw, _, _, _ := artifactFixture(t, false)
	s.executionRepo.(*mocks.MockExecutionRepository).GetByTaskIDFunc = func(context.Context, string) (*persistence.Execution, error) {
		return &persistence.Execution{ID: "run", TaskID: "task", ProjectID: "alpha", WorkflowID: "wf-alpha", Status: persistence.ExecutionStatusCompleted}, nil
	}
	text, bad := callTool(t, s, raw, "result", map[string]any{"task_id": "task"})
	require.False(t, bad)
	require.Contains(t, text, `"execution_id": "run"`)
}

func TestCompanionArtifactDiscoveryPaginationAndFailures(t *testing.T) {
	s, _, raw, a, _, _ := artifactFixture(t, false)
	names := listTools(t, s, raw)
	require.True(t, names["list_artifacts"])
	require.True(t, names["read_artifact"])
	repo := s.artifactRepo.(*mocks.MockArtifactRepository)
	repo.ListFunc = func(_ context.Context, f persistence.ArtifactFilter) ([]*persistence.Artifact, error) {
		require.Equal(t, 200, f.PageSize)
		return []*persistence.Artifact{a, a, a}, nil
	}
	txt, bad := callTool(t, s, raw, "list_artifacts", map[string]any{"task_id": "task", "execution_id": "run", "page_size": 1, "offset": 1})
	require.False(t, bad, txt)
	require.Contains(t, txt, `"next_offset":2`)
	require.Contains(t, txt, `"truncated":true`)
	txt, bad = callTool(t, s, raw, "list_artifacts", map[string]any{"task_id": "task", "execution_id": "run", "offset": 100})
	require.False(t, bad, txt)
	require.Contains(t, txt, `"artifacts":[]`)
	repo.ListFunc = func(context.Context, persistence.ArtifactFilter) ([]*persistence.Artifact, error) {
		return nil, errors.New("private db detail")
	}
	txt, bad = callTool(t, s, raw, "list_artifacts", map[string]any{"task_id": "task", "execution_id": "run"})
	require.True(t, bad)
	require.NotContains(t, txt, "private db detail")
	for _, args := range []map[string]any{{"task_id": "task", "execution_id": "run", "offset": -1}, {"task_id": "task", "execution_id": "run", "page_size": 101}, {"task_id": "task", "execution_id": "run", "page_size": -1}} {
		txt, bad = callTool(t, s, raw, "list_artifacts", args)
		require.True(t, bad, txt)
	}
	s.publicOrigin = func() string { return "https://example.test/" }
	out, bad := artifactRead(t, s, raw)
	require.False(t, bad)
	require.True(t, strings.HasPrefix(out["download_url"].(string), "https://example.test/"))
}

func TestCompanionArtifactMalformedTokensAndMissingIntegrity(t *testing.T) {
	s, _, raw, a, _, _ := artifactFixture(t, false)
	for _, token := range []string{"", strings.Repeat("a", 4097), "%.AA", "AA.%", "AA.AA"} {
		w := httptest.NewRecorder()
		s.companionArtifactDownload(w, httptest.NewRequest(http.MethodGet, artifactDownloadPath+"?token="+token, nil))
		require.Equal(t, http.StatusForbidden, w.Code)
	}
	for _, name := range []string{"size_missing", "size_negative", "too_large", "hash_missing", "invalid_mime"} {
		t.Run(name, func(t *testing.T) {
			copyA := *a
			s.artifactRepo.(*mocks.MockArtifactRepository).GetFunc = func(context.Context, string) (*persistence.Artifact, error) { return &copyA, nil }
			switch name {
			case "size_missing":
				copyA.SizeBytes = nil
			case "size_negative":
				v := int64(-1)
				copyA.SizeBytes = &v
			case "too_large":
				v := int64(artifactDownloadLimit + 1)
				copyA.SizeBytes = &v
			case "hash_missing":
				copyA.ContentHashSHA256 = nil
			case "invalid_mime":
				v := "this is not/mime;"
				copyA.MimeType = &v
			}
			_, bad := artifactRead(t, s, raw)
			require.True(t, bad)
		})
	}
}

// Withdrawal is checked again on both MCP reads and capability redemption.
func TestCompanionArtifactAgentAdminApprovalWithdrawal(t *testing.T) {
	s, k, raw, _, _, audit := artifactFixture(t, true)
	k.rows[0].AgentAdmin = true
	k.rows[0].AgentNamespace = "hermes"
	admin := &fakeAgentAdmin{approved: []string{"wf-alpha"}}
	enabled := true
	s.agentAdmin = admin
	s.agentAdminEnabled = func() bool { return enabled }
	out, bad := artifactRead(t, s, raw)
	require.False(t, bad)
	admin.approved = nil
	_, bad = artifactRead(t, s, raw)
	require.True(t, bad)
	w := httptest.NewRecorder()
	s.companionArtifactDownload(w, httptest.NewRequest(http.MethodGet, out["download_url"].(string), nil))
	require.Equal(t, http.StatusForbidden, w.Code)
	require.Len(t, audit.snapshot(), 3)
	admin.approved = []string{"wf-alpha"}
	enabled = false
	_, bad = artifactRead(t, s, raw)
	require.True(t, bad)
	enabled = true
	k.rows[0].AgentNamespace = "other"
	k.rows[0].ProjectID = "other--home"
	_, err := s.artifactAuthorization(context.Background(), k.rows[0].ID, artifactArgs{TaskID: "task", ExecutionID: "run"})
	require.Error(t, err)
}

func TestCompanionArtifactDownloadConcurrencyBound(t *testing.T) {
	s, _, raw, _, _, audit := artifactFixture(t, true)
	out, bad := artifactRead(t, s, raw)
	require.False(t, bad)
	// Occupy all verification buffers: another capability must fail promptly,
	// rather than allocate a fifth 32 MiB buffer or wait without a deadline.
	for i := 0; i < 4; i++ {
		s.artifactDownloads.slots <- struct{}{}
	}
	w := httptest.NewRecorder()
	s.companionArtifactDownload(w, httptest.NewRequest(http.MethodGet, out["download_url"].(string), nil))
	require.Equal(t, http.StatusTooManyRequests, w.Code)
	require.Len(t, audit.snapshot(), 2)
	for i := 0; i < 4; i++ {
		<-s.artifactDownloads.slots
	}
}
