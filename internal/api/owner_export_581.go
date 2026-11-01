package api

// The owner export (GitHub #581 D5, enterprise-packaging-design.md amendment
// 2026-10-29, D5 and its correction). The control plane writes an export to
// the instance's backup prefix and registers its id over the operations
// listener; the instance's owner downloads it from the public listener with
// their own OpenID session. The object key is built from the daemon's own
// instance id, never from the request, and the 7-day expiry is the daemon's
// own record, not S3's state.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"vornik.io/vornik/internal/artifacts"
	"vornik.io/vornik/internal/auth"
	"vornik.io/vornik/internal/listener"
	"vornik.io/vornik/internal/persistence"
)

const (
	// ownerExportTTL is how long a registered export is served.
	ownerExportTTL = 7 * 24 * time.Hour
	// ownerExportMaxStreams bounds the downloads in flight per instance.
	ownerExportMaxStreams = 2
	// ownerExportWriteDeadline is this response's own deadline: a large
	// export over a slow link outlasts the public listener's general one.
	ownerExportWriteDeadline = 30 * time.Minute
	ownerExportPathPrefix    = "/api/v1/export/"
)

// ownerExportID is vornik-cloud's export id: "x-" and 16 base32 characters.
// It holds no "/", "." or "..", so it cannot carry a path.
var ownerExportID = regexp.MustCompile(`^x-[a-z2-7]{16}$`)

// ExportObjectStore reads an export object by key (the S3 backend of the
// backup bucket in production).
type ExportObjectStore interface {
	Get(ctx context.Context, key string) (io.ReadCloser, error)
}

// ownerExports is the export wiring; nil fields mean not configured.
type ownerExports struct {
	repo  persistence.InstanceExportRepository
	store ExportObjectStore
	slots chan struct{}
	now   func() time.Time
}

// WithOwnerExports wires the owner export: the registry and the backup
// bucket's object store.
func WithOwnerExports(repo persistence.InstanceExportRepository, store ExportObjectStore) ServerOption {
	return func(s *Server) {
		s.exports = &ownerExports{repo: repo, store: store, slots: make(chan struct{}, ownerExportMaxStreams), now: time.Now}
	}
}

func (s *Server) exportsReady() bool {
	return s.exports != nil && s.exports.repo != nil && s.exports.store != nil
}

// ownerExportKey is the object's key under the backup bucket.
func (s *Server) ownerExportKey(id string) string {
	return "instances/" + s.config.Hosting.InstanceID + "/exports/" + id + ".tar.zst"
}

// RegisterOperationsExport serves POST /api/v1/operations/exports: an
// admin-class key (the control plane's) registers an export it has written.
func (s *Server) RegisterOperationsExport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "use POST")
		return
	}
	if s.config == nil || !s.config.Hosting.Managed() {
		respondError(w, http.StatusConflict, "NOT_MANAGED", "exports are an operation of managed hosting")
		return
	}
	presented := extractAPIKey(r)
	if presented == "" || !s.isAdminClassRequest(r) || !s.isAdminClassKey(presented) {
		respondError(w, http.StatusForbidden, "ADMIN_SCOPE_REQUIRED", "registering an export needs the instance_admin key")
		return
	}
	if !s.exportsReady() {
		respondError(w, http.StatusServiceUnavailable, "EXPORT_NOT_CONFIGURED", "hosting.backup_bucket is not set")
		return
	}
	limitJSONBody(w, r)
	var req struct {
		ExportID string `json:"export_id"`
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil || !ownerExportID.MatchString(req.ExportID) {
		respondError(w, http.StatusBadRequest, "BAD_EXPORT_ID", `the body is {"export_id": "x-" and 16 base32 characters}`)
		return
	}
	now := s.exports.now().UTC()
	switch err := s.exports.repo.RegisterExport(r.Context(), req.ExportID, now); {
	case errors.Is(err, persistence.ErrExportExists):
		respondError(w, http.StatusConflict, "EXPORT_EXISTS", "this export id is already registered")
		return
	case err != nil:
		respondError(w, http.StatusInternalServerError, "EXPORT_REGISTER", "the export could not be registered")
		return
	}
	respondJSON(w, http.StatusCreated, map[string]any{
		"export_id":  req.ExportID,
		"expires_at": now.Add(ownerExportTTL).Format(time.RFC3339),
		"path":       ownerExportPathPrefix + req.ExportID,
	})
}

// ServeOwnerExport serves GET /api/v1/export/<id> on the public listener, to
// the instance owner's tenant_admin session only.
func (s *Server) ServeOwnerExport(w http.ResponseWriter, r *http.Request) {
	if s.config == nil || !s.config.Hosting.Managed() || listener.FromContext(r.Context()) != listener.Public {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "use GET")
		return
	}
	// The owner only: not an admin key, not another session role
	// (vornik-cloud's §7: the daemon refuses the download to anyone but the
	// owner). operatorOnly would admit an admin key, so it is not used.
	if SessionRoleFromContext(r.Context()) != auth.RoleTenantAdmin {
		respondError(w, http.StatusForbidden, "OWNER_ONLY", "an export is downloaded by the instance's owner, signed in")
		return
	}
	if !s.exportsReady() {
		respondError(w, http.StatusServiceUnavailable, "EXPORT_NOT_CONFIGURED", "exports are not configured on this instance")
		return
	}
	id := strings.TrimPrefix(r.URL.Path, ownerExportPathPrefix)
	notFound := func() { respondError(w, http.StatusNotFound, "EXPORT_NOT_FOUND", "no such export, or it has expired") }
	if !ownerExportID.MatchString(id) {
		notFound()
		return
	}
	createdAt, ok, err := s.exports.repo.ExportCreatedAt(r.Context(), id)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "EXPORT_LOOKUP", "the export record could not be read")
		return
	}
	if !ok || s.exports.now().Sub(createdAt) >= ownerExportTTL {
		notFound()
		return
	}
	select {
	case s.exports.slots <- struct{}{}:
		defer func() { <-s.exports.slots }()
	default:
		w.Header().Set("Retry-After", "60")
		respondError(w, http.StatusTooManyRequests, "EXPORT_BUSY", "two exports are already downloading; retry in a minute")
		return
	}
	body, err := s.exports.store.Get(r.Context(), s.ownerExportKey(id))
	if errors.Is(err, artifacts.ErrNotFound) {
		notFound()
		return
	}
	if err != nil {
		// Neither the bucket nor the key is logged.
		s.logger.Warn().Str("export_id", id).Str("error_kind", exportErrorKind(err)).Msg("owner export: the object could not be read")
		respondError(w, http.StatusBadGateway, "EXPORT_UNAVAILABLE", "the export could not be read; retry later")
		return
	}
	defer func() { _ = body.Close() }()
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(ownerExportWriteDeadline))
	w.Header().Set("Content-Type", "application/zstd")
	w.Header().Set("Content-Disposition", `attachment; filename="`+id+`.tar.zst"`)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	if _, err := io.CopyBuffer(w, body, make([]byte, 32<<10)); err != nil {
		s.logger.Info().Str("export_id", id).Msg("owner export: the download ended early")
	}
}

// exportErrorKind names an S3 failure without its message, which carries
// the bucket and key.
func exportErrorKind(err error) string {
	var coded interface{ ErrorCode() string }
	if errors.As(err, &coded) {
		return coded.ErrorCode()
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return "context"
	}
	return "other"
}
