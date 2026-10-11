package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"vornik.io/vornik/internal/memory"
	"vornik.io/vornik/internal/persistence"
)

// memory_list (LLD 22, "Memory the user can see: list, forget and export
// from any harness", GitHub #119 item 6): a paged listing of the key's own
// project memory, newest first, through the memory firewall, for any
// harness. Export is never a tool: export_url sends the person to the
// approver device.

// MemoryListAdapter is the optional paged-listing surface of the companion
// memory adapter. The flag companion-memory-list is advertised only when
// the wired adapter implements it.
type MemoryListAdapter interface {
	ListMemory(ctx context.Context, in MemoryListRequest) (MemoryListPage, error)
}

// MemoryListRequest is one page request, always for the key's own project.
type MemoryListRequest struct {
	ProjectID string
	Cursor    string
	Limit     int
	ActorKind string
	ActorID   string
}

// MemoryListPage is one page after the firewall. Examined counts the rows
// read before it, so a withheld row is counted.
type MemoryListPage struct {
	Entries    []MemoryListEntry
	NextCursor string
	Examined   int
}

// MemoryListEntry is one live row, unshaped; the handler shapes it.
type MemoryListEntry struct {
	ChunkID       string
	TaskID        string
	SourceName    string
	ContentClass  string
	Content       string
	PolicyWarning string
	CreatedAt     time.Time
}

const (
	memoryListDefaultLimit = 20
	memoryListMaxLimit     = 100
	memoryListShortID      = 12
	// memoryListSnippetBytes matches recent_memory's bound.
	memoryListSnippetBytes = recentMemorySnippetChars
)

// companionMemoryTools are the tools that read or change a key's project
// memory. An agent-admin key is refused all of them by its kind (LLD 22
// change §1): its memory is never the agent's memory (operator F6).
var companionMemoryTools = map[string]bool{
	"recall":         true,
	"remember":       true,
	"recent_memory":  true,
	"list_scopes":    true,
	"memory_correct": true,
	"memory_list":    true,
}

type memoryListArgs struct {
	Cursor string `json:"cursor"`
	Limit  *int   `json:"limit"`
	Query  string `json:"query"`
}

type memoryListItem struct {
	ID            string `json:"id"`
	ShortID       string `json:"short_id"`
	CreatedAt     string `json:"created_at"`
	Age           string `json:"age"`
	Source        string `json:"source"`
	Origin        string `json:"origin"`
	ContentClass  string `json:"content_class,omitempty"`
	Snippet       string `json:"snippet"`
	PolicyWarning string `json:"policy_warning,omitempty"`
}

type memoryListResult struct {
	ProjectID  string           `json:"project_id"`
	Items      []memoryListItem `json:"items"`
	Returned   int              `json:"returned"`
	Examined   int              `json:"examined"`
	NextCursor string           `json:"next_cursor"`
	ExportURL  string           `json:"export_url"`
	Note       string           `json:"note"`
}

const memoryListNote = "To forget an item, call memory_correct with its id in chunk_ids and reason \"forget_command\". " +
	"To export, or to forget from the phone, give the user export_url: it opens on their paired approver device only. " +
	"Never paste their whole memory. Forgetting here does not change the assistant's own memory file."

func (s *Server) memoryListAdapter() MemoryListAdapter {
	if s.memoryCompanion == nil {
		return nil
	}
	a, _ := s.memoryCompanion.(MemoryListAdapter)
	return a
}

func (s *Server) companionToolMemoryList(ctx context.Context, key *persistence.APIKey, raw json.RawMessage) (string, error) {
	if !key.MemoryRead {
		return "", errors.New("this key lacks memory_read; ask the operator for `vornikctl companion grant --memory-read`")
	}
	lister := s.memoryListAdapter()
	if lister == nil {
		return "", errors.New("memory listing is not available on this daemon")
	}
	var args memoryListArgs
	if err := decodeToolArgs("memory_list", raw, &args); err != nil {
		return "", err
	}
	limit := memoryListDefaultLimit
	if args.Limit != nil {
		limit = *args.Limit
	}
	if limit < 1 || limit > memoryListMaxLimit {
		return "", fmt.Errorf("BAD_REQUEST: limit must be 1 to %d", memoryListMaxLimit)
	}
	if _, err := memory.DecodeChunkCursor(args.Cursor); err != nil {
		return "", errors.New("BAD_REQUEST: cursor does not decode; pass next_cursor from a previous memory_list page unchanged")
	}
	query := strings.TrimSpace(args.Query)
	if query != "" && args.Cursor != "" {
		return "", errors.New("BAD_REQUEST: a query does not page; omit cursor")
	}

	var entries []MemoryListEntry
	out := memoryListResult{ProjectID: key.ProjectID, ExportURL: s.memoryExportURL(key.ProjectID), Note: memoryListNote}
	if query != "" {
		// The query path IS recall: Recall runs the firewall itself (purpose
		// operational, the key's role and id), as the recall tool does, and
		// like the recall tool it carries no per-hit policy_warning and no
		// withheld count (LLD 22, as built A2, review 6172 F1).
		recallLimit := min(limit, 50)
		hits, err := s.memoryCompanion.Recall(ctx, key.ProjectID, query, RecallOptions{
			Limit:     recallLimit,
			RepoScope: effectiveRepoScope(key, ""),
			ActorKind: companionActorKind(key),
			ActorID:   key.ID,
		})
		if err != nil {
			return "", fmt.Errorf("memory_list failed: %w", err)
		}
		if len(hits) > recallLimit { // one page, whatever the adapter returned (review 6172 F2)
			hits = hits[:recallLimit]
		}
		for _, h := range hits {
			at, _ := time.Parse(time.RFC3339, h.CreatedAt)
			entries = append(entries, MemoryListEntry{ChunkID: h.ChunkID, TaskID: h.TaskID, SourceName: h.SourceName,
				ContentClass: h.ContentClass, Content: h.Content, CreatedAt: at})
		}
		out.Examined = len(hits)
	} else {
		page, err := lister.ListMemory(ctx, MemoryListRequest{
			ProjectID: key.ProjectID,
			Cursor:    args.Cursor,
			Limit:     limit,
			ActorKind: companionActorKind(key),
			ActorID:   key.ID,
		})
		if err != nil {
			return "", fmt.Errorf("memory_list failed: %w", err)
		}
		entries, out.Examined, out.NextCursor = page.Entries, page.Examined, page.NextCursor
	}
	now := time.Now()
	out.Items = make([]memoryListItem, 0, len(entries))
	for _, e := range entries {
		out.Items = append(out.Items, shapeMemoryListItem(e, now))
	}
	out.Returned = len(out.Items)
	b, _ := json.MarshalIndent(out, "", "  ")
	return string(b), nil
}

func shapeMemoryListItem(e MemoryListEntry, now time.Time) memoryListItem {
	short := e.ChunkID
	if len(short) > memoryListShortID {
		short = short[:memoryListShortID]
	}
	it := memoryListItem{
		ID:            e.ChunkID,
		ShortID:       short,
		Source:        strings.TrimSpace(memory.StripMirrorTokens(e.SourceName)),
		Origin:        memory.OriginOf(e.TaskID, e.SourceName, e.Content),
		ContentClass:  e.ContentClass,
		Snippet:       memory.Snippet(e.Content, memoryListSnippetBytes),
		PolicyWarning: e.PolicyWarning,
	}
	if !e.CreatedAt.IsZero() {
		it.CreatedAt = e.CreatedAt.UTC().Format(time.RFC3339)
		it.Age = memory.HumanAge(now.Sub(e.CreatedAt))
	}
	return it
}

// memoryExportURL is the approver device's page for one project: absolute
// when the daemon has a public origin, else the path.
func (s *Server) memoryExportURL(projectID string) string {
	path := "/ui/approve/memory/" + url.PathEscape(projectID)
	if s.publicOrigin != nil {
		if o := strings.TrimRight(strings.TrimSpace(s.publicOrigin()), "/"); o != "" {
			return o + path
		}
	}
	return path
}
