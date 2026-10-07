package retention

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/lib/pq"
	"vornik.io/vornik/internal/approval"
	"vornik.io/vornik/internal/persistence"
)

// DefaultMemoryIdleDays is the idle window for projects requiring phone approval.
const DefaultMemoryIdleDays = 365
const memoryApprovalBatchSize = 20

// MemoryIdleDays is shared by proposal and application, including the default.
func MemoryIdleDays(days int) int {
	if days == 0 {
		return DefaultMemoryIdleDays
	}
	return days
}

// MemoryApproval is the immutable read set shown on the paired phone. Preview
// is stored only on the protected approval page, never in the pushed sentence.
type MemoryApproval struct {
	ProjectID   string     `json:"project_id"`
	ChunkID     string     `json:"chunk_id"`
	Source      string     `json:"source"`
	Preview     string     `json:"preview"`
	ContentHash string     `json:"content_hash"`
	CreatedAt   time.Time  `json:"created_at"`
	LastUsedAt  *time.Time `json:"last_used_at"`
	ExpiresAt   time.Time  `json:"expires_at"`
	IdleDays    int        `json:"idle_days"`
}

func idleEligibility(days int) string {
	return `expires_at IS NOT NULL AND COALESCE(last_used_at,created_at) + GREATEST(INTERVAL '1 day' * ` + strconv.Itoa(days) + `, expires_at-created_at) < $2`
}

// MemoryApprovalRequests proposes finite-TTL idle chunks, not deletes. Requests
// of any status suppress re-asking for 30 days; stable IDs deduplicate replicas.
func (s *Sweeper) MemoryApprovalRequests(ctx context.Context, projectID string, days int, now time.Time) ([]persistence.AgentApprovalRequestRow, error) {
	days = MemoryIdleDays(days)
	if projectID == "" || days < 1 || days > 36500 {
		return nil, errors.New("invalid memory idle retention policy")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,source_name,LEFT(content,240),content_hash,created_at,last_used_at,expires_at
 FROM project_memory_chunks WHERE project_id=$1 AND `+idleEligibility(days)+`
 AND NOT EXISTS (SELECT 1 FROM agent_approval_requests a WHERE a.kind='memory_retention' AND a.rendered::jsonb->>'project_id'=$1 AND a.rendered::jsonb->>'chunk_id'=project_memory_chunks.id AND a.created_at>$2 - INTERVAL '30 days')
 ORDER BY COALESCE(last_used_at,created_at),id LIMIT 20`, projectID, now)
	if err != nil {
		return nil, fmt.Errorf("find idle memory: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make([]persistence.AgentApprovalRequestRow, 0, memoryApprovalBatchSize)
	for rows.Next() {
		p := MemoryApproval{ProjectID: projectID, IdleDays: days}
		if err = rows.Scan(&p.ChunkID, &p.Source, &p.Preview, &p.ContentHash, &p.CreatedAt, &p.LastUsedAt, &p.ExpiresAt); err != nil {
			return nil, err
		}
		raw, e := json.Marshal(p)
		if e != nil {
			return nil, e
		}
		canonical, e := approval.Canonical(raw)
		if e != nil {
			return nil, e
		}
		digest, e := approval.CanonicalSHA256(canonical)
		if e != nil {
			return nil, e
		}
		idHash := sha256.Sum256([]byte(projectID + "\x00" + p.ChunkID + "\x00" + now.UTC().Format("2006-01-02")))
		out = append(out, persistence.AgentApprovalRequestRow{ID: "apr_" + hex.EncodeToString(idHash[:8]), Namespace: "memory:" + projectID, Kind: persistence.ApprovalKindMemoryRetention,
			Sentence: "Stored memory has not been used for its retention period. Review the source and preview on your paired phone before deleting it.", Rendered: canonical, RenderedSHA256: digest, Status: persistence.ApprovalPending, CreatedAt: now, ExpiresAt: now.Add(7 * 24 * time.Hour)})
	}
	return out, rows.Err()
}

// ApplyMemoryApproval is idempotent and shares the existing chunk/link/graph
// transaction. The phone decision, exact displayed read set and idle horizon
// must still match; changed or used chunks produce no deletion.
func (s *Sweeper) ApplyMemoryApproval(ctx context.Context, r persistence.AgentApprovalRequestRow, days int, now time.Time) (int, error) {
	if r.Kind != persistence.ApprovalKindMemoryRetention || r.Status != persistence.ApprovalApproved || r.DecidedByDevice == "" {
		return 0, errors.New("memory deletion requires a paired-device approval")
	}
	digest, err := approval.CanonicalSHA256(r.Rendered)
	if err != nil || digest != r.RenderedSHA256 {
		return 0, errors.New("memory approval content does not match its hash")
	}
	var p MemoryApproval
	if err = json.Unmarshal(r.Rendered, &p); err != nil {
		return 0, err
	}
	days = MemoryIdleDays(days)
	if p.ProjectID == "" || p.ChunkID == "" || p.IdleDays != days || days < 1 || days > 36500 || r.Namespace != "memory:"+p.ProjectID {
		return 0, errors.New("memory approval policy or scope changed")
	}
	used := p.CreatedAt
	if p.LastUsedAt != nil {
		used = *p.LastUsedAt
	}
	quoteTime := func(t time.Time) string { return pq.QuoteLiteral(t.UTC().Format(time.RFC3339Nano)) + "::timestamptz" }
	// All fragments are daemon-built; even payload strings are SQL-quoted.
	lastUsedSQL := "NULL"
	if p.LastUsedAt != nil {
		lastUsedSQL = quoteTime(*p.LastUsedAt)
	}
	where := `id=` + pq.QuoteLiteral(p.ChunkID) + ` AND content_hash=` + pq.QuoteLiteral(p.ContentHash) + ` AND created_at=` + quoteTime(p.CreatedAt) + ` AND expires_at=` + quoteTime(p.ExpiresAt) + ` AND last_used_at IS NOT DISTINCT FROM ` + lastUsedSQL + ` AND COALESCE(last_used_at,created_at)=$2 AND COALESCE(last_used_at,created_at)+GREATEST(INTERVAL '1 day' * ` + strconv.Itoa(days) + `,expires_at-created_at)<` + quoteTime(now)
	n, _, err := s.pruneChunkBatch(ctx, p.ProjectID, where, used)
	return n, err
}
