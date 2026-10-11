package approverdevice

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// The approver device's memory page (LLD 22, "Memory the user can see: list,
// forget and export from any harness", change §3; GitHub #119 item 6). A
// person on the paired device sees what their assistants' memory projects
// hold and what broker workflows learned, forgets items, and downloads an
// export. Nothing here is reachable by a key: every route is behind
// RequireDevice, and every POST passes approval.CheckRequest there. This
// package holds no memory dependency: the daemon supplies a MemoryView.

// Memory sections: what an assistant's tools reach, and what broker
// workflows learned (read and forget only, no export).
const (
	MemorySectionAssistants = 1
	MemorySectionBroker     = 2
)

// MemoryProject is one project the page shows.
type MemoryProject struct {
	ID         string
	Namespace  string
	Section    int
	Holders    []string // "Hermes (memory key)", "Claude Code (laptop)"
	Exportable bool
}

// MemoryItemView is one live item, shaped for a person.
type MemoryItemView struct {
	ID, ShortID, Age, Source, Origin, Snippet, PolicyWarning string
}

// MemoryPageView is one page of items. Examined counts rows read before the
// firewall, so withheld = Examined - len(Items) on a listing page.
type MemoryPageView struct {
	Items      []MemoryItemView
	NextCursor string
	Examined   int
	Search     bool
}

// MemoryExportItem is one exported row, content verbatim.
type MemoryExportItem struct {
	ID           string `json:"id"`
	CreatedAt    string `json:"created_at"`
	SourceName   string `json:"source_name"`
	Origin       string `json:"origin"`
	ContentClass string `json:"content_class,omitempty"`
	RepoScope    string `json:"repo_scope,omitempty"`
	Content      string `json:"content"`
}

// MemoryExportCounts is the export's denominator.
type MemoryExportCounts struct{ Examined, Exported int }

// MemoryView is the daemon's memory surface for the device. Every method
// takes the device id, which the daemon records in its audit rows and uses
// as the firewall request's operator. The view re-checks that the project
// is one it lists; the page checks first.
type MemoryView interface {
	Projects(ctx context.Context) ([]MemoryProject, error)
	Page(ctx context.Context, projectID, cursor string, limit int, deviceID string) (MemoryPageView, error)
	Search(ctx context.Context, projectID, query string, limit int, deviceID string) (MemoryPageView, error)
	Forget(ctx context.Context, projectID, chunkID, deviceID string) (int, error)
	CountSource(ctx context.Context, projectID, source string) (int, error)
	ForgetSource(ctx context.Context, projectID, source, deviceID string) (int, error)
	Export(ctx context.Context, projectID, deviceID string, emit func(MemoryExportItem) error) (MemoryExportCounts, error)
}

// SetMemoryView installs the memory page's backend.
func (s *Service) SetMemoryView(v MemoryView) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.memory = v
}

func (s *Service) memoryView() MemoryView {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.memory
}

const (
	memoryPageSize = 50
	// MemorySourceCountCap bounds the forget-by-source count shown.
	MemorySourceCountCap = 10_000
)

var memoryProjectRe = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)

type memoryIndexData struct {
	Assistants, Broker []MemoryProject
}

type memoryProjectData struct {
	Project  MemoryProject
	Page     MemoryPageView
	Withheld int
	Query    string
	Sources  []string
	Flash    string
}

type memoryConfirmData struct {
	Project MemoryProject
	Source  string
	Count   int
	More    bool
}

// memoryRouter serves everything under /ui/approve/memory.
func (s *Service) memoryRouter(w http.ResponseWriter, r *http.Request, rest string) {
	v := s.memoryView()
	if v == nil {
		s.renderStatus(w, http.StatusServiceUnavailable, "Memory is not available", "Memory is not available on this daemon.")
		return
	}
	rest = strings.TrimPrefix(rest, "/")
	if rest == "" {
		s.memoryIndex(w, r, v)
		return
	}
	projectID, action, _ := strings.Cut(rest, "/")
	if !memoryProjectRe.MatchString(projectID) {
		http.NotFound(w, r)
		return
	}
	p, ok, err := memoryProjectOf(r.Context(), v, projectID)
	if err != nil {
		s.renderStatus(w, http.StatusInternalServerError, "Something went wrong", "Try again in a moment.")
		return
	}
	if !ok {
		s.renderStatus(w, http.StatusNotFound, "Not found", "This is not a memory this page shows.")
		return
	}
	d, _ := FromRequest(r)
	switch action {
	case "":
		s.memoryProjectPage(w, r, v, p)
	case "forget":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		id := strings.TrimSpace(r.PostFormValue("id"))
		if id == "" {
			http.Error(w, "missing id", http.StatusBadRequest)
			return
		}
		n, err := v.Forget(r.Context(), p.ID, id, d.ID)
		if err != nil {
			s.renderStatus(w, http.StatusInternalServerError, "Not forgotten", "Vornik could not forget it. Try again in a moment.")
			return
		}
		http.Redirect(w, r, memoryProjectPath(p.ID)+"?done="+url.QueryEscape(fmt.Sprintf("forgot %d", n)), http.StatusSeeOther)
	case "forget-source":
		s.memoryForgetSource(w, r, v, p, d.ID)
	case "export":
		s.memoryExport(w, r, v, p, d.ID)
	default:
		http.NotFound(w, r)
	}
}

func memoryProjectPath(id string) string { return "/ui/approve/memory/" + url.PathEscape(id) }

// memoryProjectOf returns the listed project, or false: the same answer for
// "unknown" and "not listed".
func memoryProjectOf(ctx context.Context, v MemoryView, id string) (MemoryProject, bool, error) {
	ps, err := v.Projects(ctx)
	if err != nil {
		return MemoryProject{}, false, err
	}
	for _, p := range ps {
		if p.ID == id {
			return p, true, nil
		}
	}
	return MemoryProject{}, false, nil
}

func (s *Service) memoryIndex(w http.ResponseWriter, r *http.Request, v MemoryView) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ps, err := v.Projects(r.Context())
	if err != nil {
		s.renderStatus(w, http.StatusInternalServerError, "Something went wrong", "Try again in a moment.")
		return
	}
	var data memoryIndexData
	for _, p := range ps {
		if p.Section == MemorySectionBroker {
			data.Broker = append(data.Broker, p)
		} else {
			data.Assistants = append(data.Assistants, p)
		}
	}
	s.render(w, http.StatusOK, "memory.html", data)
}

func (s *Service) memoryProjectPage(w http.ResponseWriter, r *http.Request, v MemoryView, p MemoryProject) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	d, _ := FromRequest(r)
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	var page MemoryPageView
	var err error
	if q != "" {
		page, err = v.Search(r.Context(), p.ID, q, memoryPageSize, d.ID)
	} else {
		page, err = v.Page(r.Context(), p.ID, r.URL.Query().Get("cursor"), memoryPageSize, d.ID)
	}
	if err != nil {
		s.renderStatus(w, http.StatusBadRequest, "Could not read this memory", "Go back to the first page and try again.")
		return
	}
	data := memoryProjectData{Project: p, Page: page, Query: q, Flash: r.URL.Query().Get("done")}
	if !page.Search && page.Examined > len(page.Items) {
		data.Withheld = page.Examined - len(page.Items)
	}
	seen := map[string]bool{}
	for _, it := range page.Items {
		if it.Source != "" && !seen[it.Source] {
			seen[it.Source] = true
			data.Sources = append(data.Sources, it.Source)
		}
	}
	s.render(w, http.StatusOK, "memory_project.html", data)
}

func (s *Service) memoryForgetSource(w http.ResponseWriter, r *http.Request, v MemoryView, p MemoryProject, deviceID string) {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		source := r.URL.Query().Get("source")
		if source == "" {
			http.Error(w, "missing source", http.StatusBadRequest)
			return
		}
		n, err := v.CountSource(r.Context(), p.ID, source)
		if err != nil {
			s.renderStatus(w, http.StatusInternalServerError, "Something went wrong", "Try again in a moment.")
			return
		}
		s.render(w, http.StatusOK, "memory_confirm.html", memoryConfirmData{Project: p, Source: source, Count: n, More: n >= MemorySourceCountCap})
	case http.MethodPost:
		source := r.PostFormValue("source")
		if source == "" {
			http.Error(w, "missing source", http.StatusBadRequest)
			return
		}
		n, err := v.ForgetSource(r.Context(), p.ID, source, deviceID)
		if err != nil {
			s.renderStatus(w, http.StatusInternalServerError, "Not forgotten", "Vornik could not forget them all. Reload the page to see what is left.")
			return
		}
		http.Redirect(w, r, memoryProjectPath(p.ID)+"?done="+url.QueryEscape(fmt.Sprintf("forgot %d", n)), http.StatusSeeOther)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// memoryExport streams a project's live rows as an attachment. No process
// and no archive tool: the handler writes rows to the response (the
// process-spawn law).
func (s *Service) memoryExport(w http.ResponseWriter, r *http.Request, v MemoryView, p MemoryProject, deviceID string) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !p.Exportable {
		s.renderStatus(w, http.StatusForbidden, "No export", "What broker workflows learned can be read and forgotten here, not exported.")
		return
	}
	format := r.URL.Query().Get("format")
	if format != "json" && format != "md" {
		http.Error(w, "format must be json or md", http.StatusBadRequest)
		return
	}
	now := s.now().UTC()
	name := fmt.Sprintf("vornik-memory-%s-%s.%s", p.ID, now.Format("2006-01-02"), format)
	if format == "json" {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
	} else {
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	}
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	bw := bufio.NewWriter(w)
	var counts MemoryExportCounts
	var err error
	if format == "json" {
		counts, err = exportJSON(r.Context(), bw, v, p.ID, deviceID, now)
	} else {
		counts, err = exportMarkdown(r.Context(), bw, v, p.ID, deviceID, now)
	}
	// A failure mid-stream cannot change the status any more: the file
	// itself says so (complete=false in JSON, an "interrupted" line in
	// Markdown), and the counts say how far it got.
	_, _ = counts, err
	_ = bw.Flush()
}

func exportJSON(ctx context.Context, w *bufio.Writer, v MemoryView, projectID, deviceID string, now time.Time) (MemoryExportCounts, error) {
	head, _ := json.Marshal(projectID)
	_, _ = fmt.Fprintf(w, `{"project":%s,"exported_at":%q,"items":[`, head, now.Format(time.RFC3339))
	first := true
	counts, err := v.Export(ctx, projectID, deviceID, func(it MemoryExportItem) error {
		b, err := json.Marshal(it)
		if err != nil {
			return err
		}
		if !first {
			if err := w.WriteByte(','); err != nil {
				return err
			}
		}
		first = false
		_, err = w.Write(b)
		return err
	})
	_, _ = fmt.Fprintf(w, `],"examined":%d,"exported":%d,"withheld":%d,"complete":%t}`,
		counts.Examined, counts.Exported, max(0, counts.Examined-counts.Exported), err == nil)
	return counts, err
}

func exportMarkdown(ctx context.Context, w *bufio.Writer, v MemoryView, projectID, deviceID string, now time.Time) (MemoryExportCounts, error) {
	_, _ = fmt.Fprintf(w, "# Vornik memory: %s\n\nExported %s. What Vornik's memory held for this project at that moment; not your assistant's own memory file.\n\n",
		projectID, now.Format(time.RFC3339))
	counts, err := v.Export(ctx, projectID, deviceID, func(it MemoryExportItem) error {
		_, err := fmt.Fprintf(w, "## %s · %s · %s\n\nid: %s\n\n%s\n\n", it.CreatedAt, it.Origin, it.SourceName, it.ID, it.Content)
		return err
	})
	if err != nil {
		_, _ = fmt.Fprintf(w, "---\n\nExport interrupted after %d items. Download it again.\n", counts.Exported)
		return counts, err
	}
	_, _ = fmt.Fprintf(w, "---\n\n%d examined, %d exported, %d withheld by the memory firewall.\n",
		counts.Examined, counts.Exported, max(0, counts.Examined-counts.Exported))
	return counts, nil
}

// ErrMemoryNotListed is a view's refusal for a project it does not list.
var ErrMemoryNotListed = errors.New("not a memory this page shows")
