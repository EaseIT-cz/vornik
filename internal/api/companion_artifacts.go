package api

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"vornik.io/vornik/internal/egressscan"
	"vornik.io/vornik/internal/executor"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/untrusted"
)

const artifactDownloadPath = "/api/v1/companion/artifact-download"
const artifactTextLimit = 64 << 10
const artifactDownloadLimit = 32 << 20

var errArtifactUnavailable = errors.New("artifact unavailable or not authorized")

type artifactDownloadSigner struct {
	once  sync.Once
	key   [32]byte
	err   error
	slots chan struct{}
}
type artifactArgs struct {
	TaskID      string `json:"task_id"`
	ExecutionID string `json:"execution_id"`
	ArtifactID  string `json:"artifact_id"`
	PageSize    int    `json:"page_size"`
	Offset      int    `json:"offset"`
}
type artifactListArgs struct {
	TaskID      string `json:"task_id"`
	ExecutionID string `json:"execution_id"`
	PageSize    int    `json:"page_size"`
	Offset      int    `json:"offset"`
}
type artifactReadArgs struct {
	TaskID      string `json:"task_id"`
	ExecutionID string `json:"execution_id"`
	ArtifactID  string `json:"artifact_id"`
}
type artifactGrant struct {
	key    *persistence.APIKey
	task   *persistence.Task
	wf     *registry.Workflow
	broker bool
}
type artifactCapability struct {
	KeyID       string `json:"k"`
	TaskID      string `json:"t"`
	ExecutionID string `json:"e"`
	ArtifactID  string `json:"a"`
	Expires     int64  `json:"x"`
}

func artifactToolDefs() []mcpToolDef {
	var defs []mcpToolDef
	for _, name := range []string{"list_artifacts", "read_artifact"} {
		props := map[string]any{"task_id": map[string]any{"type": "string"}, "execution_id": map[string]any{"type": "string"}}
		required := []any{"task_id", "execution_id"}
		description := "List approved output metadata from one completed run. No storage paths or input/log/transcript files."
		if name == "read_artifact" {
			props["artifact_id"] = map[string]any{"type": "string"}
			required = append(required, "artifact_id")
			description = "Read one approved output from a completed run. Text is untrusted and capped at 64 KiB; binary returns a five-minute scoped download link, never bytes in model context."
		} else {
			props["page_size"] = map[string]any{"type": "integer", "minimum": 1, "maximum": 100}
			props["offset"] = map[string]any{"type": "integer", "minimum": 0, "maximum": 200}
		}
		defs = append(defs, mcpToolDef{Name: name, Description: description, InputSchema: map[string]any{"type": "object", "properties": props, "required": required, "additionalProperties": false}})
	}
	return defs
}

// artifactAuthorization deliberately reads the live key rather than trusting a
// task-creation grant or the key embedded in a forwarded download link.
func (s *Server) artifactAuthorization(ctx context.Context, keyID string, a artifactArgs) (*artifactGrant, error) {
	if s.apiKeyRepo == nil || s.taskRepo == nil || s.executionRepo == nil || s.artifactRepo == nil || s.projectRegistry == nil || a.TaskID == "" || a.ExecutionID == "" {
		return nil, errArtifactUnavailable
	}
	key, err := s.apiKeyRepo.GetByID(ctx, keyID)
	if err != nil || key == nil || key.RevokedAt != nil || key.DelegateDisabled || key.ClientKind == "" || (key.ExpiresAt != nil && !key.ExpiresAt.After(time.Now())) {
		return nil, errArtifactUnavailable
	}
	task, err := s.taskRepo.Get(ctx, a.TaskID)
	if err != nil || task == nil || !keyCoversProject(key, task.ProjectID) || task.Status != persistence.TaskStatusCompleted {
		return nil, errArtifactUnavailable
	}
	execution, err := s.executionRepo.Get(ctx, a.ExecutionID)
	wfID := derefString(task.WorkflowID)
	if err != nil || execution == nil || execution.ID != a.ExecutionID || execution.TaskID != task.ID || execution.ProjectID != task.ProjectID || execution.WorkflowID != wfID || execution.Status != persistence.ExecutionStatusCompleted {
		return nil, errArtifactUnavailable
	}
	wf := s.projectRegistry.GetWorkflow(wfID)
	if wf == nil {
		return nil, errArtifactUnavailable
	}
	if key.AllowedWorkflows != nil && !artifactContains(key.AllowedWorkflows, wfID) {
		return nil, errArtifactUnavailable
	}
	if key.AgentAdmin {
		if !s.agentAdminOffered(key) {
			return nil, errArtifactUnavailable
		}
		approved, e := s.agentAdmin.ApprovedWorkflows(ctx, key)
		if e != nil || !artifactContains(approved, wfID) {
			return nil, errArtifactUnavailable
		}
	}
	project := s.projectRegistry.GetProject(task.ProjectID)
	if project == nil {
		return nil, errArtifactUnavailable
	}
	broker := project.Broker || key.AgentAdmin || wf.Broker != nil
	if broker && (wf.Broker == nil || len(wf.Broker.Egress.Artifacts) == 0) {
		return nil, errArtifactUnavailable
	}
	return &artifactGrant{key: key, task: task, wf: wf, broker: broker}, nil
}
func artifactContains(names []string, name string) bool {
	for _, n := range names {
		if n == name {
			return true
		}
	}
	return false
}
func (g *artifactGrant) permits(a *persistence.Artifact, executionID string) bool {
	if a == nil || a.ProjectID != g.task.ProjectID || derefString(a.TaskID) != g.task.ID || derefString(a.ExecutionID) != executionID || a.ArtifactClass != persistence.ArtifactClassOutput || executor.IsTranscriptArtifact(a.Name) {
		return false
	}
	if !g.broker {
		return true
	}
	name := executor.OriginalArtifactName(a.Name)
	if name == strings.TrimSpace(g.wf.Broker.Egress.Output) {
		return false
	}
	for _, p := range g.wf.Broker.Proposes {
		if name == strings.TrimSpace(p.Output) {
			return false
		}
	}
	return artifactContains(g.wf.Broker.Egress.Artifacts, name)
}
func artifactMetadata(a *persistence.Artifact) map[string]any {
	return map[string]any{"artifact_id": a.ID, "name": a.Name, "mime_type": a.MimeType, "size_bytes": a.SizeBytes, "sha256": a.ContentHashSHA256}
}
func artifactJSON(v any) (string, error) { b, err := json.Marshal(v); return string(b), err }

func (s *Server) companionToolArtifact(ctx context.Context, key *persistence.APIKey, tool string, raw json.RawMessage) (string, error) {
	var args artifactArgs
	if err := decodeToolArgs(tool, raw, &args); err != nil {
		return "", err
	}
	g, err := s.artifactAuthorization(ctx, key.ID, args)
	if err != nil {
		return "", err
	}
	if tool == "list_artifacts" {
		if args.PageSize == 0 {
			args.PageSize = 100
		}
		if args.PageSize < 1 || args.PageSize > 100 || args.Offset < 0 || args.Offset > 200 {
			return "", errors.New("invalid artifact pagination")
		}
		rows, e := s.artifactRepo.List(ctx, persistence.ArtifactFilter{ProjectID: &g.task.ProjectID, TaskID: &args.TaskID, ExecutionID: &args.ExecutionID, PageSize: 200})
		if e != nil {
			return "", errArtifactUnavailable
		}
		filtered := make([]map[string]any, 0)
		for _, a := range rows {
			if g.permits(a, args.ExecutionID) {
				filtered = append(filtered, artifactMetadata(a))
			}
		}
		start := min(args.Offset, len(filtered))
		end := min(start+args.PageSize, len(filtered))
		return artifactJSON(map[string]any{"task_id": args.TaskID, "execution_id": args.ExecutionID, "artifacts": filtered[start:end], "offset": args.Offset, "next_offset": end, "truncated": len(rows) >= 200 || end < len(filtered), "scan_truncated": len(rows) >= 200})
	}
	a, err := s.artifactRepo.Get(ctx, args.ArtifactID)
	if err != nil || !g.permits(a, args.ExecutionID) {
		return "", errArtifactUnavailable
	}
	b, text, truncated, err := s.artifactPayload(ctx, a, artifactTextLimit)
	if err != nil {
		return "", err
	}
	out := artifactMetadata(a)
	out["task_id"] = args.TaskID
	out["execution_id"] = args.ExecutionID
	if text {
		if g.broker {
			doc, _ := json.Marshal(string(b))
			if why := s.egress.scanAgentDoc(egressscan.SurfaceEgressDoc, a.ProjectID, g.wf.ID, doc); why != "" {
				return "", fmt.Errorf("egress_secret: %s", why)
			}
		}
		out["content"] = untrusted.Wrap(string(b))
		out["truncated"] = truncated
		out["kind"] = "text"
	} else {
		if a.SizeBytes == nil || *a.SizeBytes < 0 || *a.SizeBytes > artifactDownloadLimit || a.ContentHashSHA256 == nil {
			return "", errArtifactUnavailable
		}
		expiry := time.Now().Add(5 * time.Minute)
		token, e := s.signArtifactCapability(artifactCapability{key.ID, args.TaskID, args.ExecutionID, a.ID, expiry.Unix()})
		if e != nil {
			return "", errArtifactUnavailable
		}
		origin := ""
		if s.publicOrigin != nil {
			origin = strings.TrimRight(s.publicOrigin(), "/")
		}
		out["kind"] = "binary"
		out["download_url"] = origin + artifactDownloadPath + "?token=" + url.QueryEscape(token)
		out["expires_at"] = expiry.UTC().Format(time.RFC3339)
	}
	return artifactJSON(out)
}

// artifactPayload never interprets binary bytes as text. A sniff is bounded to
// 512 bytes and only used when metadata is absent or generic octet-stream.
func (s *Server) artifactPayload(ctx context.Context, a *persistence.Artifact, limit int) ([]byte, bool, bool, error) {
	if s.artifactOpener == nil {
		return nil, false, false, errArtifactUnavailable
	}
	r, err := s.artifactOpener.Open(ctx, a.ID)
	if err != nil {
		return nil, false, false, errArtifactUnavailable
	}
	defer func() { _ = r.Close() }()
	prefix, err := io.ReadAll(io.LimitReader(r, 512))
	if err != nil {
		return nil, false, false, errArtifactUnavailable
	}
	mt, _, e := mime.ParseMediaType(derefString(a.MimeType))
	if e != nil && derefString(a.MimeType) != "" {
		return nil, false, false, errArtifactUnavailable
	}
	if mt == "" || mt == "application/octet-stream" {
		mt, _, _ = mime.ParseMediaType(http.DetectContentType(prefix))
	}
	text := strings.HasPrefix(mt, "text/") || mt == "application/json" || mt == "application/xml" || strings.HasSuffix(mt, "+json") || strings.HasSuffix(mt, "+xml")
	if !text {
		return nil, false, false, nil
	}
	tail, e := io.ReadAll(io.LimitReader(r, int64(limit+1-len(prefix))))
	if e != nil {
		return nil, false, false, errArtifactUnavailable
	}
	prefix = append(prefix, tail...)
	b := prefix
	truncated := len(b) > limit
	if truncated {
		b = b[:limit]
		for len(b) > 0 && !utf8.Valid(b) && len(b) > limit-4 {
			b = b[:len(b)-1]
		}
	}
	if !utf8.Valid(b) || strings.ContainsRune(string(b), 0) {
		return nil, false, false, errors.New("artifact is not valid UTF-8 text")
	}
	return b, true, truncated, nil
}
func (s *Server) signArtifactCapability(c artifactCapability) (string, error) {
	s.artifactDownloads.once.Do(func() {
		s.artifactDownloads.slots = make(chan struct{}, 4)
		_, s.artifactDownloads.err = rand.Read(s.artifactDownloads.key[:])
	})
	if s.artifactDownloads.err != nil {
		return "", s.artifactDownloads.err
	}
	b, e := json.Marshal(c)
	if e != nil {
		return "", e
	}
	mac := hmac.New(sha256.New, s.artifactDownloads.key[:])
	mac.Write(b)
	return base64.RawURLEncoding.EncodeToString(b) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}
func (s *Server) decodeArtifactCapability(token string) (artifactCapability, error) {
	var c artifactCapability
	if len(token) > 4096 {
		return c, errArtifactUnavailable
	}
	parts := strings.Split(token, ".")
	if len(parts) != 2 {
		return c, errArtifactUnavailable
	}
	b, e := base64.RawURLEncoding.DecodeString(parts[0])
	if e != nil {
		return c, errArtifactUnavailable
	}
	signature, e := base64.RawURLEncoding.DecodeString(parts[1])
	if e != nil {
		return c, errArtifactUnavailable
	}
	// Initialize a fresh signing key on restart, never accepting an old token.
	s.artifactDownloads.once.Do(func() {
		s.artifactDownloads.slots = make(chan struct{}, 4)
		_, s.artifactDownloads.err = rand.Read(s.artifactDownloads.key[:])
	})
	if s.artifactDownloads.err != nil {
		return c, errArtifactUnavailable
	}
	mac := hmac.New(sha256.New, s.artifactDownloads.key[:])
	mac.Write(b)
	if !hmac.Equal(signature, mac.Sum(nil)) || json.Unmarshal(b, &c) != nil {
		return c, errArtifactUnavailable
	}
	return c, nil
}
func (s *Server) companionArtifactDownload(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	c, e := s.decodeArtifactCapability(r.URL.Query().Get("token"))
	if e != nil {
		http.Error(w, errArtifactUnavailable.Error(), http.StatusForbidden)
		return
	}
	start := time.Now()
	var result string
	var callErr error
	// A signed but expired/revoked/narrowed grant is still a recognized-key audit.
	if s.apiKeyRepo == nil {
		http.Error(w, errArtifactUnavailable.Error(), http.StatusForbidden)
		return
	}
	key, _ := s.apiKeyRepo.GetByID(r.Context(), c.KeyID)
	defer func() {
		s.recordCompanionToolAudit(r.Context(), key, "artifact_download", nil, result, callErr, time.Since(start))
	}()
	fail := func() { callErr = errArtifactUnavailable; http.Error(w, callErr.Error(), http.StatusForbidden) }
	if c.Expires <= time.Now().Unix() {
		fail()
		return
	}
	args := artifactArgs{TaskID: c.TaskID, ExecutionID: c.ExecutionID, ArtifactID: c.ArtifactID}
	g, e := s.artifactAuthorization(r.Context(), c.KeyID, args)
	if e != nil {
		fail()
		return
	}
	a, e := s.artifactRepo.Get(r.Context(), c.ArtifactID)
	if e != nil || !g.permits(a, c.ExecutionID) || a.SizeBytes == nil || *a.SizeBytes < 0 || *a.SizeBytes > artifactDownloadLimit || a.ContentHashSHA256 == nil || s.artifactOpener == nil {
		fail()
		return
	}
	// Bound aggregate integrity-buffer memory to four 32 MiB payloads. A
	// reusable capability cannot turn concurrent requests into an unlimited
	// allocation queue; overload is prompt, audited and retryable.
	select {
	case s.artifactDownloads.slots <- struct{}{}:
		defer func() { <-s.artifactDownloads.slots }()
	default:
		callErr = errors.New("artifact download capacity reached")
		w.Header().Set("Retry-After", "1")
		http.Error(w, callErr.Error(), http.StatusTooManyRequests)
		return
	}
	_, isText, _, e := s.artifactPayload(r.Context(), a, artifactTextLimit)
	if e != nil || isText {
		fail()
		return
	}
	reader, e := s.artifactOpener.Open(r.Context(), a.ID)
	if e != nil {
		fail()
		return
	}
	defer func() { _ = reader.Close() }()
	b, e := io.ReadAll(io.LimitReader(reader, artifactDownloadLimit+1))
	sum := sha256.Sum256(b)
	if e != nil || int64(len(b)) != *a.SizeBytes || len(b) > artifactDownloadLimit || hex.EncodeToString(sum[:]) != *a.ContentHashSHA256 {
		fail()
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": a.Name}))
	w.Header().Set("Content-Length", fmt.Sprint(len(b)))
	result = string(b)
	_, callErr = w.Write(b)
}

// completedArtifactRun makes the required immutable run identity discoverable
// from result; failed/stale/cross-task executions are never advertised.
func (s *Server) completedArtifactRun(ctx context.Context, task *persistence.Task, out map[string]any) {
	if s.executionRepo == nil || task.Status != persistence.TaskStatusCompleted {
		return
	}
	e, err := s.executionRepo.GetByTaskID(ctx, task.ID)
	if err == nil && e != nil && e.TaskID == task.ID && e.ProjectID == task.ProjectID && e.WorkflowID == derefString(task.WorkflowID) && e.Status == persistence.ExecutionStatusCompleted {
		out["execution_id"] = e.ID
	}
}
