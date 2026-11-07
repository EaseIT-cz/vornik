package api

import (
	"net/http"

	"vornik.io/vornik/internal/auth"
)

// Surface names one operator-surface gate site, so managed hosting's instance
// owner (tenant_admin) can be granted it by name (GitHub #560;
// 18-security-and-tenancy.md, "Amendment 2026-10-28 (#560, slice M1b)", B2
// and B3). The constants are a closed set: every surface-taking gate passes
// one, the completeness test pins that each is used, and tenantAdminSurfaces
// grants a closed subset. A surface not granted is refused to tenant_admin.
type Surface string

// The operator surfaces (B3), one per gate site.
const (
	SurfaceWizardConverse       Surface = "wizard_converse"
	SurfaceWizard               Surface = "wizard"
	SurfaceMCPOAuthConnect      Surface = "mcp_oauth_connect"
	SurfaceOperatorAudit        Surface = "operator_audit"
	SurfaceConfigReload         Surface = "config_reload"
	SurfaceConfigReloadStatus   Surface = "config_reload_status"
	SurfaceSetupStatus          Surface = "setup_status"
	SurfaceSetupModels          Surface = "setup_models"
	SurfaceSetupDispatcher      Surface = "setup_dispatcher"
	SurfaceSetupSession         Surface = "setup_session"
	SurfaceSetupSessionRouter   Surface = "setup_session_router"
	SurfaceSetupManaged         Surface = "setup_managed"
	SurfaceConfigAssist         Surface = "config_assist"
	SurfaceConfigShow           Surface = "config_show"
	SurfaceInstinctRetireGlobal Surface = "instinct_retire_global"
	SurfaceMemoryFirewall       Surface = "memory_firewall"
	SurfaceOperatorAccounts     Surface = "operator_accounts"
	SurfaceOperatorLinks        Surface = "operator_links"
	SurfaceOperatorProposals    Surface = "operator_proposals"
	SurfaceOperators            Surface = "operators"
	SurfaceSkillSetGlobal       Surface = "skill_set_global"
	// GitHub #503: the last three admin-only routes that had their own gate
	// (requireAdminClassGate) now pass the one decision. None is granted to
	// tenant_admin.
	SurfaceConfigJournal Surface = "config_journal"
	SurfaceInflight      Surface = "inflight"
	SurfaceAgentGrant    Surface = "agent_grant"
	// SurfaceOperatorCredentials is the console's credentials page
	// (/ui/credentials, GitHub #623; deliberately outside /ui/admin, which admin.Middleware reserves for admins): the operator, or a managed
	// instance's owner with no shell, enters the values that
	// un-namespaced secret:// references resolve to. Its gate is in the UI
	// package (api.OperatorSurfaceAccessFor).
	SurfaceOperatorCredentials Surface = "operator_credentials"
)

// tenantAdminSurfaces is the closed allowlist of B3: granted surfaces only.
var tenantAdminSurfaces = map[Surface]bool{
	SurfaceWizardConverse:  true,
	SurfaceWizard:          true,
	SurfaceMCPOAuthConnect: true,
	SurfaceOperatorAudit:   true,
	// GitHub #623: valid while managed mode is single-customer
	// (tenant_admin = the instance's owner); a multi-tenant instance
	// (multi-tenancy design, Stage T) must revisit it.
	SurfaceOperatorCredentials: true,
}

// TenantAdminMay reports whether a tenant_admin session may use surface.
func TenantAdminMay(surface Surface) bool { return tenantAdminSurfaces[surface] }

// logTenantAdminDecision records each operator-surface decision made for a
// tenant_admin session, so the allowlist's behaviour is observable (B2c).
func (s *Server) logTenantAdminDecision(r *http.Request, surface Surface, allowed bool) {
	if s == nil || SessionRoleFromContext(r.Context()) != auth.RoleTenantAdmin {
		return
	}
	outcome := "refused"
	if allowed {
		outcome = "allowed"
	}
	s.logger.Info().Str("surface", string(surface)).Str("outcome", outcome).Str("path", r.URL.Path).
		Msg("operator surface: tenant_admin " + outcome)
}
