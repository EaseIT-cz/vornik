package service

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"vornik.io/vornik/internal/agentns"
	"vornik.io/vornik/internal/approverdevice"
	"vornik.io/vornik/internal/memory"
	"vornik.io/vornik/internal/memoryfirewall"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// The approver device's memory page backend (LLD 22, "Memory the user can
// see", change §3; GitHub #119 item 6). It reads the memory subsystem at call
// time (memory may start after the device service is built), lists the
// projects a device sees, and records every forget and export in admin_audit
// naming the device.

// approverMemoryDeps are the parts the view reads; a seam for tests.
type approverMemoryDeps struct {
	searcher func() *memory.Searcher
	repo     func() *memory.Repository
	keys     persistence.APIKeyRepository
	projects func() []*registry.Project
	audit    persistence.AdminAuditRepository
	now      func() time.Time
}

type approverMemoryView struct{ d approverMemoryDeps }

// approverMemoryRole is the firewall request role for the device's reads.
const approverMemoryRole = "approver_device"

// approverMemoryExportPage is the export's read size.
const approverMemoryExportPage = memory.ListPageMax

func (c *Container) approverMemoryView() approverdevice.MemoryView {
	if c.repos == nil || c.repos.APIKeys == nil {
		return nil
	}
	return &approverMemoryView{d: approverMemoryDeps{
		searcher: func() *memory.Searcher {
			if c.memoryManager == nil {
				return nil
			}
			return c.memoryManager.Searcher
		},
		repo: func() *memory.Repository {
			if c.memoryManager == nil {
				return nil
			}
			return c.memoryManager.Repository()
		},
		keys: c.repos.APIKeys,
		projects: func() []*registry.Project {
			if c.Registry == nil {
				return nil
			}
			return c.Registry.ListProjects()
		},
		audit: c.repos.AdminAudit,
		now:   time.Now,
	}}
}

var clientKindNames = map[string]string{"hermes": "Hermes", "claude-code": "Claude Code", "codex": "Codex", "claude-desktop": "Claude Desktop"}

// Projects lists section 1 (each project an active companion key with
// memory_read names; not an agent-admin key) and section 2 (each registered
// broker project). A project in both is listed in section 2 only.
func (v *approverMemoryView) Projects(ctx context.Context) ([]approverdevice.MemoryProject, error) {
	broker := map[string]*registry.Project{}
	for _, p := range v.d.projects() {
		if p != nil && p.Broker {
			broker[p.ID] = p
		}
	}
	keys, err := v.d.keys.ListAttributable(ctx)
	if err != nil {
		return nil, err
	}
	now := v.d.now()
	holders := map[string][]string{}
	for _, k := range keys {
		if k == nil || k.ClientKind == "" || !k.MemoryRead || k.AgentAdmin || k.RevokedAt != nil ||
			(k.ExpiresAt != nil && !k.ExpiresAt.After(now)) || broker[k.ProjectID] != nil {
			continue
		}
		name := clientKindNames[k.ClientKind]
		if name == "" {
			name = k.ClientKind
		}
		if k.SessionLabel != "" {
			name += " (" + k.SessionLabel + ")"
		}
		holders[k.ProjectID] = append(holders[k.ProjectID], name)
	}
	out := make([]approverdevice.MemoryProject, 0, len(holders)+len(broker))
	for id, hs := range holders {
		sort.Strings(hs)
		out = append(out, approverdevice.MemoryProject{ID: id, Section: approverdevice.MemorySectionAssistants, Holders: hs, Exportable: true})
	}
	for id := range broker {
		ns, _ := agentns.FromID(id)
		out = append(out, approverdevice.MemoryProject{ID: id, Namespace: ns, Section: approverdevice.MemorySectionBroker})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Section != out[j].Section {
			return out[i].Section < out[j].Section
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// listed re-checks, inside the view, that the project is one the page shows.
func (v *approverMemoryView) listed(ctx context.Context, projectID string) (approverdevice.MemoryProject, error) {
	ps, err := v.Projects(ctx)
	if err != nil {
		return approverdevice.MemoryProject{}, err
	}
	for _, p := range ps {
		if p.ID == projectID {
			return p, nil
		}
	}
	return approverdevice.MemoryProject{}, approverdevice.ErrMemoryNotListed
}

func deviceRequest(deviceID string) memoryfirewall.RequestContext {
	return memoryfirewall.RequestContext{Purpose: memoryfirewall.PurposeAuditReview, Role: approverMemoryRole, OperatorID: "device:" + deviceID}
}

func (v *approverMemoryView) ready() (*memory.Searcher, *memory.Repository, error) {
	s, r := v.d.searcher(), v.d.repo()
	if s == nil || r == nil {
		return nil, nil, fmt.Errorf("memory is not available on this daemon")
	}
	return s, r, nil
}

func shapeDeviceItem(id, taskID, source, content, warning string, createdAt, now time.Time) approverdevice.MemoryItemView {
	short := id
	if len(short) > 12 {
		short = short[:12]
	}
	it := approverdevice.MemoryItemView{ID: id, ShortID: short, Source: strings.TrimSpace(memory.StripMirrorTokens(source)),
		Origin: memory.OriginOf(taskID, source, content), Snippet: memory.Snippet(content, 240), PolicyWarning: warning}
	if !createdAt.IsZero() {
		it.Age = memory.HumanAge(now.Sub(createdAt))
	}
	return it
}

func (v *approverMemoryView) Page(ctx context.Context, projectID, cursor string, limit int, deviceID string) (approverdevice.MemoryPageView, error) {
	if _, err := v.listed(ctx, projectID); err != nil {
		return approverdevice.MemoryPageView{}, err
	}
	s, _, err := v.ready()
	if err != nil {
		return approverdevice.MemoryPageView{}, err
	}
	after, err := memory.DecodeChunkCursor(cursor)
	if err != nil {
		return approverdevice.MemoryPageView{}, err
	}
	page, err := s.ListPageWithContext(ctx, projectID, after, limit, deviceRequest(deviceID))
	if err != nil {
		return approverdevice.MemoryPageView{}, err
	}
	out := approverdevice.MemoryPageView{Examined: page.Examined}
	if page.Next != nil {
		out.NextCursor = memory.EncodeChunkCursor(*page.Next)
	}
	now := v.d.now()
	for _, r := range page.Rows {
		out.Items = append(out.Items, shapeDeviceItem(r.ChunkID, r.TaskID, r.SourceName, r.Content, r.PolicyWarning, r.CreatedAt, now))
	}
	return out, nil
}

func (v *approverMemoryView) Search(ctx context.Context, projectID, query string, limit int, deviceID string) (approverdevice.MemoryPageView, error) {
	if _, err := v.listed(ctx, projectID); err != nil {
		return approverdevice.MemoryPageView{}, err
	}
	s, _, err := v.ready()
	if err != nil {
		return approverdevice.MemoryPageView{}, err
	}
	hits, err := s.RecallWithContext(ctx, projectID, query, memory.SearchOptions{Limit: limit}, deviceRequest(deviceID))
	if err != nil {
		return approverdevice.MemoryPageView{}, err
	}
	if len(hits) > limit {
		hits = hits[:limit]
	}
	out := approverdevice.MemoryPageView{Search: true, Examined: len(hits)}
	now := v.d.now()
	for _, h := range hits {
		out.Items = append(out.Items, shapeDeviceItem(h.ChunkID, h.TaskID, h.SourceName, h.Content, "", h.CreatedAt, now))
	}
	return out, nil
}

func (v *approverMemoryView) Forget(ctx context.Context, projectID, chunkID, deviceID string) (int, error) {
	if _, err := v.listed(ctx, projectID); err != nil {
		return 0, err
	}
	_, r, err := v.ready()
	if err != nil {
		return 0, err
	}
	n, err := r.MarkRefutedByIDs(ctx, projectID, []string{chunkID}, memory.RefuteRouteDeviceForget)
	if err != nil {
		return 0, err
	}
	v.record(ctx, deviceID, "device_memory_forget", projectID, map[string]any{"ids": []string{chunkID}, "refuted": n})
	return n, nil
}

func (v *approverMemoryView) CountSource(ctx context.Context, projectID, source string) (int, error) {
	if _, err := v.listed(ctx, projectID); err != nil {
		return 0, err
	}
	_, r, err := v.ready()
	if err != nil {
		return 0, err
	}
	n, after := 0, ""
	for n < approverdevice.MemorySourceCountCap {
		ids, err := r.ListLiveChunkIDsBySource(ctx, projectID, source, after, memory.ListPageMax)
		if err != nil {
			return 0, err
		}
		n += len(ids)
		if len(ids) < memory.ListPageMax {
			break
		}
		after = ids[len(ids)-1]
	}
	return min(n, approverdevice.MemorySourceCountCap), nil
}

func (v *approverMemoryView) ForgetSource(ctx context.Context, projectID, source, deviceID string) (int, error) {
	if _, err := v.listed(ctx, projectID); err != nil {
		return 0, err
	}
	_, r, err := v.ready()
	if err != nil {
		return 0, err
	}
	total, after := 0, ""
	for {
		ids, err := r.ListLiveChunkIDsBySource(ctx, projectID, source, after, memory.ListPageMax)
		if err != nil {
			v.record(ctx, deviceID, "device_memory_forget_source", projectID, map[string]any{"source": source, "refuted": total, "error": "interrupted"})
			return total, err
		}
		if len(ids) == 0 {
			break
		}
		n, err := r.MarkRefutedByIDs(ctx, projectID, ids, memory.RefuteRouteDeviceForget)
		total += n
		if err != nil {
			v.record(ctx, deviceID, "device_memory_forget_source", projectID, map[string]any{"source": source, "refuted": total, "error": "interrupted"})
			return total, err
		}
		if len(ids) < memory.ListPageMax {
			break
		}
		after = ids[len(ids)-1]
	}
	v.record(ctx, deviceID, "device_memory_forget_source", projectID, map[string]any{"source": source, "refuted": total})
	return total, nil
}

func (v *approverMemoryView) Export(ctx context.Context, projectID, deviceID string, emit func(approverdevice.MemoryExportItem) error) (approverdevice.MemoryExportCounts, error) {
	var counts approverdevice.MemoryExportCounts
	p, err := v.listed(ctx, projectID)
	if err != nil {
		return counts, err
	}
	if !p.Exportable {
		return counts, approverdevice.ErrMemoryNotListed
	}
	s, _, err := v.ready()
	if err != nil {
		return counts, err
	}
	var after *memory.ChunkCursor
	for {
		page, err := s.ListPageWithContext(ctx, projectID, after, approverMemoryExportPage, deviceRequest(deviceID))
		if err != nil {
			v.recordExport(ctx, deviceID, projectID, counts, err)
			return counts, err
		}
		counts.Examined += page.Examined
		for _, r := range page.Rows {
			if r.PolicyWarning != "" {
				// Advisory mode: the firewall would have blocked it. An export
				// leaves the device, so it follows enforce here.
				continue
			}
			if err := emit(approverdevice.MemoryExportItem{ID: r.ChunkID, CreatedAt: r.CreatedAt.UTC().Format(time.RFC3339),
				SourceName: r.SourceName, Origin: memory.OriginOf(r.TaskID, r.SourceName, r.Content),
				ContentClass: r.ContentClass, RepoScope: r.RepoScope, Content: r.Content}); err != nil {
				v.recordExport(ctx, deviceID, projectID, counts, err)
				return counts, err
			}
			counts.Exported++
		}
		if page.Next == nil {
			break
		}
		after = page.Next
	}
	v.recordExport(ctx, deviceID, projectID, counts, nil)
	return counts, nil
}

func (v *approverMemoryView) recordExport(ctx context.Context, deviceID, projectID string, c approverdevice.MemoryExportCounts, err error) {
	after := map[string]any{"examined": c.Examined, "exported": c.Exported}
	if err != nil {
		after["error"] = "interrupted"
	}
	v.record(ctx, deviceID, "device_memory_export", projectID, after)
}

// record writes one admin_audit row naming the device. A failed audit write
// does not undo the forget (the row records it as refute_route) and is not
// surfaced to the person.
func (v *approverMemoryView) record(ctx context.Context, deviceID, action, projectID string, after map[string]any) {
	if v.d.audit == nil {
		return
	}
	b, _ := json.Marshal(after)
	_ = v.d.audit.Insert(ctx, &persistence.AdminAuditEntry{
		ID: persistence.GenerateID("admaud"), Timestamp: v.d.now().UTC(), Principal: "device:" + deviceID,
		Source: "device", Action: action, Target: projectID, After: string(b),
	})
}
