package service

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"vornik.io/vornik/internal/agentadmin"
	"vornik.io/vornik/internal/approverdevice"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/retention"
)

// No paired-phone service means preserve the data, never downgrade to deletion.
func (c *Container) fileMemoryRetentionRequests(ctx context.Context, s *retention.Sweeper, p *registry.Project) {
	if !p.Retention.MemoryRequireApproval {
		return
	}
	devices := c.approverDeviceService()
	if devices == nil {
		c.Logger.Warn().Str("project", p.ID).Msg("idle memory preserved: phone approval service unavailable")
		return
	}
	requests, err := s.MemoryApprovalRequests(ctx, p.ID, p.Retention.MemoryIdleDays, time.Now().UTC())
	if err == nil {
		err = devices.FileRequestBatch(ctx, requests, "Unused stored memory is waiting for your deletion decision")
	}
	if err != nil {
		c.Logger.Warn().Err(err).Str("project", p.ID).Msg("idle memory preserved: approval filing failed")
	}
}

func (c *Container) applyMemoryRetention(ctx context.Context, r persistence.AgentApprovalRequestRow) error {
	var proposed retention.MemoryApproval
	if err := json.Unmarshal(r.Rendered, &proposed); err != nil {
		return fmt.Errorf("%w: invalid memory approval", approverdevice.ErrPermanent)
	}
	if c.Registry == nil || c.DB == nil {
		return fmt.Errorf("%w: memory project unavailable", approverdevice.ErrPermanent)
	}
	// Bound the policy read lock even if database locks are contested.
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return c.Registry.WithProjectSnapshot(proposed.ProjectID, func(p *registry.Project) error {
		if p == nil || !p.Retention.MemoryRequireApproval || retention.MemoryIdleDays(p.Retention.MemoryIdleDays) != proposed.IdleDays {
			return fmt.Errorf("%w: memory retention policy changed", approverdevice.ErrPermanent)
		}
		s := retention.New(c.DB, c.Logger)
		n, err := s.ApplyMemoryApproval(ctx, r, p.Retention.MemoryIdleDays, time.Now().UTC())
		if err != nil {
			return err
		}
		c.Logger.Info().Str("project", p.ID).Str("request", r.ID).Int("deleted_chunks", n).Msg("phone-approved idle memory retention applied")
		return nil
	})
}

func describeMemoryRetention(r persistence.AgentApprovalRequestRow) *approverdevice.Description {
	var p retention.MemoryApproval
	if err := json.Unmarshal(r.Rendered, &p); err != nil {
		return &approverdevice.Description{Summary: "Delete an unused stored memory chunk.", Level: agentadmin.LevelHigh, Reasons: []string{"This permanently removes stored knowledge; invalid details cannot be applied."}}
	}
	used := p.CreatedAt
	if p.LastUsedAt != nil {
		used = *p.LastUsedAt
	}
	return &approverdevice.Description{Summary: fmt.Sprintf("Delete unused memory in project %s from source %s. Preview: %s", p.ProjectID, p.Source, p.Preview), Level: agentadmin.LevelHigh, Reasons: []string{fmt.Sprintf("Last use (or creation): %s. Minimum idle window: %d days; longer stored TTLs are preserved.", used.UTC().Format(time.RFC3339), p.IdleDays), "Approval permanently deletes this specific chunk. Reject or leave unanswered to retain it. A subsequent recall or changed content invalidates this request."}}
}
