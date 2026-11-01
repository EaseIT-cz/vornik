package api

import (
	"context"
	"fmt"
	"strings"

	"vornik.io/vornik/internal/secretstore"
)

// checkMCPOAuthTokensSealed reports OAuth grants whose token, and task
// credentials whose value, is stored unsealed (GitHub #571 U3a;
// mcp-server-authentication-design.md §15.2 A5; #578 B4).
// The startup seal pass leaves none; a row it could not seal, or one a
// pre-U3a daemon sharing the database wrote, shows here. Such a grant is not
// served (the read path refuses plaintext), so the remedy is to reconnect it,
// and to stop any daemon still on an older release.
func (h *DoctorHandlers) checkMCPOAuthTokensSealed(ctx context.Context) DoctorCheck {
	const name = "mcp_oauth_tokens_sealed"
	if h == nil || h.db == nil {
		return DoctorCheck{Name: name, Status: "SKIPPED", Message: "no database; skipping"}
	}
	pattern := secretstore.SealedPrefix + "%"
	var total, unsealed int
	err := h.db.QueryRowContext(ctx, `
		SELECT COUNT(*),
		       COALESCE(SUM(CASE WHEN (access_token <> '' AND access_token NOT LIKE $1)
		                           OR (refresh_token <> '' AND refresh_token NOT LIKE $1) THEN 1 ELSE 0 END), 0)
		  FROM mcp_oauth_tokens`, pattern).Scan(&total, &unsealed)
	if err != nil {
		// The table is created by migration; a deployment mid-upgrade (or a
		// partial test schema) has nothing to check, as connector_auth treats
		// the same query failure.
		return DoctorCheck{Name: name, Status: "OK", Message: "no OAuth grants stored"} //nolint:nilerr // absent table = nothing to check
	}
	// GitHub #578 B4: task credentials are sealed the same way. A failed
	// query (the table absent mid-upgrade) counts as none.
	var credTotal, credUnsealed int
	_ = h.db.QueryRowContext(ctx, `
		SELECT COUNT(*), COALESCE(SUM(CASE WHEN value <> '' AND value NOT LIKE $1 THEN 1 ELSE 0 END), 0)
		  FROM task_credentials`, pattern).Scan(&credTotal, &credUnsealed)
	if unsealed == 0 && credUnsealed == 0 {
		return DoctorCheck{Name: name, Status: "OK", Message: fmt.Sprintf("%d OAuth grant(s), every token sealed; %d task credential(s), every value sealed", total, credTotal)}
	}
	var msgs []string
	if unsealed > 0 {
		msgs = append(msgs, fmt.Sprintf("%d of %d OAuth grant(s) hold an unsealed token, which is never served: reconnect them", unsealed, total))
	}
	if credUnsealed > 0 {
		msgs = append(msgs, fmt.Sprintf("%d of %d task credential(s) are stored unsealed and are not shown: restart the daemon so its seal pass seals them", credUnsealed, credTotal))
	}
	return DoctorCheck{
		Name:    name,
		Status:  "ERROR",
		Message: strings.Join(msgs, "; ") + "; and stop any daemon sharing this database that runs an older release",
	}
}
