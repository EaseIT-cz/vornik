package ui

// Per-project filesystem-artifact management page. Lists every
// file under the project's persistent workspace tree
// (<workspaceRoot>/<projectID>/artifacts/) and exposes per-file
// view + delete operations so operators don't have to ssh to the
// box to manage agent outputs.
//
// The DB-backed task-artifact surface (s.artifactRepo) is a
// separate beast — it tracks per-execution artifacts captured at
// task completion. This page is about the *living* filesystem
// tree under the workspace: in/, out/, research/, and any other
// directory the agents drop files into.

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/safepath"
)

// ProjectArtifactsData backs the listing template.
type ProjectArtifactsData struct {
	Title       string
	CurrentPage string
	ProjectID   string

	// WorkspacePath is the absolute path to the project's
	// artifacts root (display-only — surfaced so operators can
	// see "what folder am I looking at?" without ssh'ing).
	WorkspacePath string

	// Files is the rendered list, sorted by relative path. Truncated
	// to Limit rows server-side; TotalFiles is the pre-truncate
	// count so the header still shows "showing N of M".
	Files      []ProjectArtifactRow
	TotalFiles int

	// Limit / LimitOptions drive the shared pageSizeSelector partial.
	// The validator (parsePageSize) clamps user input to the
	// allowlist; default is DefaultPageSize.
	Limit        int
	LimitOptions []int

	// Error surfaces any inline error (e.g. "delete failed:
	// permission denied"). Success surfaces a one-line confirmation
	// after a successful delete redirect-back.
	Error   string
	Success string
}

// ProjectArtifactRow is one row in the artifacts table.
type ProjectArtifactRow struct {
	// RelPath is the workspace-relative slash-separated path
	// (e.g. "out/note.md"). Used as the listing column AND as
	// the `path` form/query value the view + delete handlers
	// re-validate before touching disk.
	RelPath string
	Size    int64
	Mtime   time.Time
	// IsText flags the row for the inline-text-vs-attachment hint
	// on the view link. Currently derived from the filename
	// extension; the actual handler re-decides at serve time so
	// this is purely cosmetic.
	IsText bool
}

// textExtensions is the set of filename suffixes we serve as
// text/plain inline. Everything else streams as octet-stream
// attachment so the browser can't be tricked into rendering
// arbitrary bytes from the workspace.
var textExtensions = map[string]struct{}{
	".md":   {},
	".txt":  {},
	".log":  {},
	".json": {},
	".yaml": {},
	".yml":  {},
	".csv":  {},
}

// WithProjectWorkspaceRoot wires the base directory under which
// per-project persistent workspaces live. The artifact-management
// page resolves files as `<root>/<projectID>/artifacts/<relPath>`
// — same shape the executor uses on the write side. Without this
// option the page renders 503 (no workspace path configured)
// rather than silently hiding the operator's files.
func WithProjectWorkspaceRoot(path string) ServerOption {
	return func(s *Server) {
		s.projectWorkspaceRoot = path
	}
}

// ProjectArtifacts renders the listing page. Path:
//
//	GET /ui/projects/{id}/artifacts
//
// Empty projects and projects whose artifacts/ subtree doesn't
// exist yet render an empty-state row rather than 404, because
// operators land here from the project detail page before any
// task has ever run.
func (s *Server) ProjectArtifacts(w http.ResponseWriter, r *http.Request, projectID string) {
	if s.projectWorkspaceRoot == "" {
		http.Error(w, "Project workspace path not configured. Set runtime.project_workspace_path in vornik.yaml to enable this page.", http.StatusServiceUnavailable)
		return
	}
	if err := validateProjectIDComponent(projectID); err != nil {
		http.Error(w, "Invalid project id: "+err.Error(), http.StatusBadRequest)
		return
	}
	// Project-scope check — a scoped key for project A must not list,
	// read, or delete project B's workspace artifacts by guessing the
	// id. validateProjectIDComponent above already rejected malformed
	// ids (so they still 400); this gates tenant ownership. NotFound
	// (not 403) avoids confirming existence of other-tenant projects,
	// consistent with ArtifactDownload. No-op when auth is off or the
	// key is unscoped (RequestAllowsProject returns true).
	if !api.RequestAllowsProject(r, projectID) {
		http.NotFound(w, r)
		return
	}

	files, err := listArtifactFiles(s.projectWorkspaceRoot, projectID)
	if err != nil {
		s.logger.Error().Err(err).Str("project_id", projectID).Msg("ProjectArtifacts: list failed")
		http.Error(w, "Failed to list artifacts: "+err.Error(), http.StatusInternalServerError)
		return
	}

	limit := parsePageSize(r.URL.Query().Get("limit"))
	total := len(files)
	if limit > 0 && len(files) > limit {
		files = files[:limit]
	}

	data := ProjectArtifactsData{
		Title:         "Artifacts: " + projectID,
		CurrentPage:   "projects",
		ProjectID:     projectID,
		WorkspacePath: filepath.Join(s.projectWorkspaceRoot, projectID, "artifacts"),
		Files:         files,
		TotalFiles:    total,
		Limit:         limit,
		LimitOptions:  PageSizeOptions,
		Success:       r.URL.Query().Get("ok"),
	}
	if errMsg := r.URL.Query().Get("err"); errMsg != "" {
		data.Error = errMsg
	}
	s.render(w, "project_artifacts.html", data)
}

// ProjectArtifactView streams the bytes of a single workspace
// file. Path:
//
//	GET /ui/projects/{id}/artifacts/raw?path=<relPath>
//
// Text-like extensions stream inline as text/plain so operators
// can read agent output directly in the browser. Anything else
// is forced into an octet-stream attachment so the browser can't
// be tricked into executing markup planted in a workspace file.
func (s *Server) ProjectArtifactView(w http.ResponseWriter, r *http.Request, projectID string) {
	if s.projectWorkspaceRoot == "" {
		http.Error(w, "Project workspace path not configured", http.StatusServiceUnavailable)
		return
	}
	if err := validateProjectIDComponent(projectID); err != nil {
		http.Error(w, "Invalid project id: "+err.Error(), http.StatusBadRequest)
		return
	}
	if !api.RequestAllowsProject(r, projectID) {
		http.NotFound(w, r)
		return
	}
	rel := strings.TrimSpace(r.URL.Query().Get("path"))
	if rel == "" {
		http.Error(w, "Missing path parameter", http.StatusBadRequest)
		return
	}
	full, err := resolveArtifactPath(s.projectWorkspaceRoot, projectID, rel)
	if err != nil {
		http.Error(w, "Invalid path: "+err.Error(), http.StatusBadRequest)
		return
	}
	info, err := os.Stat(full)
	if err != nil {
		if os.IsNotExist(err) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "Stat failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// Directories and non-regular files (symlinks already
	// resolved by safepath.JoinUnder; anything still abnormal
	// here is rejected for defense-in-depth).
	if info.IsDir() {
		http.Error(w, "Path is a directory", http.StatusBadRequest)
		return
	}
	if !info.Mode().IsRegular() {
		http.Error(w, "Path is not a regular file", http.StatusBadRequest)
		return
	}

	base := filepath.Base(full)
	if isText(base) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Content-Disposition", fmt.Sprintf("inline; filename=%q", base))
	} else {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", base))
	}
	f, err := os.Open(full)
	if err != nil {
		http.Error(w, "Open failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer func() { _ = f.Close() }()
	if _, err := io.Copy(w, f); err != nil {
		s.logger.Warn().Err(err).Str("path", full).Msg("ProjectArtifactView: stream failed")
	}
}

// ProjectArtifactDelete removes a single artifact file. POST-only;
// form payload:
//
//	path=<relPath>
//
// On success redirects back to the listing with an `ok=...` query
// so the operator sees confirmation. Errors redirect back with an
// `err=...` query so the inline banner can surface them.
func (s *Server) ProjectArtifactDelete(w http.ResponseWriter, r *http.Request, projectID string) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.projectWorkspaceRoot == "" {
		http.Error(w, "Project workspace path not configured", http.StatusServiceUnavailable)
		return
	}
	if err := validateProjectIDComponent(projectID); err != nil {
		http.Error(w, "Invalid project id: "+err.Error(), http.StatusBadRequest)
		return
	}
	if !api.RequestAllowsProject(r, projectID) {
		http.NotFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Failed to parse form: "+err.Error(), http.StatusBadRequest)
		return
	}
	rel := strings.TrimSpace(r.FormValue("path"))
	if rel == "" {
		http.Error(w, "Missing path", http.StatusBadRequest)
		return
	}
	full, err := resolveArtifactPath(s.projectWorkspaceRoot, projectID, rel)
	if err != nil {
		http.Error(w, "Invalid path: "+err.Error(), http.StatusBadRequest)
		return
	}
	info, err := os.Lstat(full)
	if err != nil {
		if os.IsNotExist(err) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "Stat failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if info.IsDir() {
		http.Error(w, "Refusing to delete a directory", http.StatusBadRequest)
		return
	}
	if !info.Mode().IsRegular() {
		// Symlinks / sockets / devices — refuse so we can't be
		// tricked into following a planted symlink during the
		// unlink (even though safepath already resolved it).
		http.Error(w, "Refusing to delete non-regular file", http.StatusBadRequest)
		return
	}
	// Lock-on-mutation: the unlink is a workspace writer, so it takes the
	// SAME shared per-project workspace lock the executor and the
	// git-over-HTTPS handler take, serialising against concurrent task
	// execution and pushes on this project. The lock is a leaf, released
	// before the HTTP redirect, never held across it.
	//
	// No git runs here (process-spawn law, S3,
	// https://docs.vornik.io). Until
	// 2026-09-26 this handler committed the deletion; a request may not make
	// the daemon run a program. The deletion stays a working-tree change
	// until the next task's merge-time auto-commit records it.
	removeErr := func() error {
		unlock := s.workspaceLock.Lock(projectID)
		defer unlock()
		return os.Remove(full)
	}()
	if err := removeErr; err != nil {
		s.logger.Error().Err(err).Str("path", full).Msg("ProjectArtifactDelete: remove failed")
		dest := fmt.Sprintf("/ui/projects/%s/artifacts?err=%s",
			projectID, urlQueryEscape("Delete failed: "+err.Error()))
		http.Redirect(w, r, dest, http.StatusSeeOther)
		return
	}
	s.logger.Info().Str("project_id", projectID).Str("rel_path", rel).Msg("artifact deleted")
	dest := fmt.Sprintf("/ui/projects/%s/artifacts?ok=%s",
		projectID, urlQueryEscape("Deleted "+rel))
	http.Redirect(w, r, dest, http.StatusSeeOther)
}

// listArtifactFiles walks the per-project artifacts/ tree and
// returns one row per regular file. Hidden files (basename
// starting with '.') and directories are skipped. Results sort
// by relative path for stable rendering.
//
// Missing artifacts/ dir returns (nil, nil) — operators land here
// from a new project before any task has run.
func listArtifactFiles(workspaceRoot, projectID string) ([]ProjectArtifactRow, error) {
	if workspaceRoot == "" {
		return nil, fmt.Errorf("workspace root is empty")
	}
	// CodeQL go/path-injection (2026-07-18): validate + confine the
	// request-derived project id under the workspace tree. CleanPathComponent
	// is the recognised barrier feeding JoinUnder; validateProjectIDComponent
	// discarded the cleaned value so it was not itself a barrier.
	cleanID, err := safepath.CleanPathComponent(projectID)
	if err != nil {
		return nil, err
	}
	base, err := safepath.JoinUnder(workspaceRoot, cleanID, "artifacts")
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(base)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("artifacts path %q is not a directory", base)
	}

	var rows []ProjectArtifactRow
	walkErr := filepath.WalkDir(base, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		// Skip the root itself.
		if path == base {
			return nil
		}
		name := d.Name()
		// Hidden entries: skip the whole subtree if it's a
		// directory so we don't fall into a hidden config dir.
		if strings.HasPrefix(name, ".") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		// Use Type() rather than Lstat()'s mode to avoid an extra
		// syscall — fs.DirEntry already knows whether this is a
		// symlink, and we refuse to surface symlinks for the same
		// reason the delete handler does.
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return nil // best-effort: skip rows we can't stat
		}
		if !fi.Mode().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(base, path)
		if err != nil {
			return nil
		}
		// Normalise to slash for both display and the form/query
		// values. Otherwise the same file would have a different
		// URL on Windows vs Linux.
		rel = filepath.ToSlash(rel)
		rows = append(rows, ProjectArtifactRow{
			RelPath: rel,
			Size:    fi.Size(),
			Mtime:   fi.ModTime(),
			IsText:  isText(name),
		})
		return nil
	})
	if walkErr != nil {
		return rows, walkErr
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].RelPath < rows[j].RelPath })
	return rows, nil
}

// resolveArtifactPath validates an operator-supplied relative
// path and returns the canonical absolute path under the
// project's artifacts/ tree. Rejects anything that:
//   - is empty / contains a `..` component (path traversal)
//   - resolves to a hidden basename (.env, .secret, …)
//   - escapes the artifacts/ root via symlink
//
// safepath.JoinUnder owns the traversal + symlink-escape checks;
// this wrapper layers the explicit `..` rejection (matches the
// task brief's "before joining" requirement) and the
// hidden-basename refusal.
func resolveArtifactPath(workspaceRoot, projectID, rel string) (string, error) {
	if workspaceRoot == "" {
		return "", fmt.Errorf("workspace root is empty")
	}
	if err := validateProjectIDComponent(projectID); err != nil {
		return "", err
	}
	rel = strings.TrimSpace(rel)
	if rel == "" {
		return "", fmt.Errorf("relative path is empty")
	}
	// Hard-reject any path with a '..' segment up front. safepath
	// would catch the escape too, but doing the rejection here
	// means we never even touch the disk for an obviously hostile
	// payload — and the error message can be specific.
	cleaned := filepath.Clean(filepath.FromSlash(rel))
	if cleaned == "." || cleaned == "/" {
		return "", fmt.Errorf("path %q is not a file", rel)
	}
	for _, segment := range strings.Split(filepath.ToSlash(cleaned), "/") {
		if segment == ".." {
			return "", fmt.Errorf("path %q contains '..' segment", rel)
		}
		if strings.HasPrefix(segment, ".") {
			return "", fmt.Errorf("path %q contains hidden segment %q", rel, segment)
		}
	}
	base := filepath.Join(workspaceRoot, projectID, "artifacts")
	full, err := safepath.JoinUnder(base, cleaned)
	if err != nil {
		return "", err
	}
	return full, nil
}

// validateProjectIDComponent enforces that projectID is a single
// path component — no separators, no traversal, no shell tricks.
// Same shape the rest of the UI uses for project IDs.
func validateProjectIDComponent(projectID string) error {
	if projectID == "" {
		return fmt.Errorf("project id is empty")
	}
	if _, err := safepath.CleanPathComponent(projectID); err != nil {
		return err
	}
	return nil
}

// isText reports whether the filename's extension is in the
// inline-render allowlist.
func isText(name string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	_, ok := textExtensions[ext]
	return ok
}

// humanizeSize formats a byte count as B / KB / MB / GB with
// one decimal for the multi-digit tiers. Used by the listing
// template so the column reads "3.5 MB" instead of "3670016".
func humanizeSize(n int64) string {
	const (
		_  = iota
		kb = 1 << (10 * iota)
		mb
		gb
		tb
	)
	switch {
	case n < kb:
		return fmt.Sprintf("%d B", n)
	case n < mb:
		return fmt.Sprintf("%.1f KB", float64(n)/float64(kb))
	case n < gb:
		return fmt.Sprintf("%.1f MB", float64(n)/float64(mb))
	case n < tb:
		return fmt.Sprintf("%.1f GB", float64(n)/float64(gb))
	default:
		return fmt.Sprintf("%.1f TB", float64(n)/float64(tb))
	}
}

// urlQueryEscape wraps net/url.QueryEscape so the call sites
// don't need the import (artifacts.go already pulls plenty).
// Inlined as a function because the redirect builders need
// %-encoded values and net/url isn't imported elsewhere in this
// file.
func urlQueryEscape(raw string) string {
	// Minimal escaping — we control the input shape (it's our
	// own error text), so we just hex-encode the few characters
	// that break query parsing. Anything else passes through.
	var b strings.Builder
	for _, r := range raw {
		switch r {
		case ' ':
			b.WriteByte('+')
		case '&', '=', '?', '#', '%', '+':
			fmt.Fprintf(&b, "%%%02X", r)
		default:
			if r < 0x20 || r == 0x7f {
				fmt.Fprintf(&b, "%%%02X", r)
				continue
			}
			b.WriteRune(r)
		}
	}
	return b.String()
}
