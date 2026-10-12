package api

import (
	"context"
	"errors"

	"vornik.io/vornik/internal/persistence"
)

// taskToolAuditRows reads one task's tool-audit rows for the named tools:
// one ToolName-filtered page of pageSize rows per name. It is the one read
// every per-task audit rebuild uses (the query_api / list_apis budget, the
// forge issue budget and the forge one-repo check).
//
// Why a filter per name: a single page of all the task's rows lets the
// task's other tool calls crowd the wanted rows off the page, and the
// rebuilt count silently undercounts (audit 2026-10-12, found twice:
// deriveForgeIssueSpend and deriveTaskAPISpend).
//
// complete is false when any page came back full: the set cannot be
// established from one page, and the caller must fail closed. Offset paging
// is not used, because both drivers order by created_at only, so rows with
// equal timestamps can be skipped across a page boundary.
//
// see LLD § https://docs.vornik.io "Audit rebuild reads per tool name (2026-10-12)"
func (s *Server) taskToolAuditRows(ctx context.Context, taskID string, names []string, pageSize int) (rows []*persistence.ToolAuditEntry, complete bool, err error) {
	if s.toolAuditRepo == nil || taskID == "" {
		return nil, false, errors.New("tool audit repository unavailable")
	}
	complete = true
	for _, name := range names {
		page, err := s.toolAuditRepo.List(ctx, persistence.ToolAuditFilter{TaskID: &taskID, ToolName: &name, PageSize: pageSize})
		if err != nil {
			return nil, false, err
		}
		if len(page) >= pageSize {
			complete = false
			s.logger.Warn().Str("task_id", taskID).Str("tool", name).Int("page_size", pageSize).
				Msg("tool audit rebuild: page full; the caller fails closed (budget exhausted or check refused)")
		}
		for _, row := range page {
			if row != nil && row.ToolName == name {
				rows = append(rows, row)
			}
		}
	}
	return rows, complete, nil
}
