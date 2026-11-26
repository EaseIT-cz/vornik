package api

// The provider-profile API (GitHub #880, project and swarm registry design
// §11.2 R6). All three endpoints are admin-class. The apply endpoint writes
// only runtime.agent_llm.tiers and .roles, and every value it writes comes
// from a shipped profile file under <configs>/model-profiles, never from the
// request body.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/configlock"
	"vornik.io/vornik/internal/featuredoctor"
	"vornik.io/vornik/internal/modelcheck"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// profileNameRe is a shipped profile's file name: no path separators or dots,
// so a name can never leave the profiles directory.
var profileNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

type modelProfileDTO struct {
	Name          string                        `json:"name"`
	Description   string                        `json:"description"`
	EndpointHosts []string                      `json:"endpoint_hosts"`
	Matches       bool                          `json:"matches_endpoint"`
	Tiers         map[string]config.ModelChoice `json:"tiers"`
	Roles         map[string]config.ModelChoice `json:"roles"`
}

func (h *DoctorHandlers) loadModelProfiles() ([]config.ModelProfile, error) {
	return config.LoadModelProfiles(filepath.Join(h.configDir, config.ModelProfileDirName))
}

// ListModelProfiles handles GET /api/v1/models/profiles.
func (h *DoctorHandlers) ListModelProfiles(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "use GET")
		return
	}
	if !h.requireAdminClass(w, r) {
		return
	}
	profiles, err := h.loadModelProfiles()
	if err != nil {
		respondError(w, http.StatusInternalServerError, "PROFILES_UNREADABLE", err.Error())
		return
	}
	match := config.MatchModelProfile(profiles, h.chatEndpoint)
	out := make([]modelProfileDTO, 0, len(profiles))
	for _, p := range profiles {
		out = append(out, modelProfileDTO{
			Name: p.Name, Description: p.Description, EndpointHosts: p.EndpointHosts,
			Matches: match != nil && match.Name == p.Name, Tiers: p.Tiers, Roles: p.Roles,
		})
	}
	respondJSON(w, http.StatusOK, map[string]any{"profiles": out})
}

// routeModelProfilePath handles POST /api/v1/models/profiles/{name}/apply.
func (h *DoctorHandlers) routeModelProfilePath(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/v1/models/profiles/")
	name, action, _ := strings.Cut(rest, "/")
	if action != "apply" || !profileNameRe.MatchString(name) {
		respondError(w, http.StatusNotFound, "NOT_FOUND", "no such profile endpoint")
		return
	}
	if r.Method != http.MethodPost {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "use POST")
		return
	}
	if !h.requireAdminClass(w, r) {
		return
	}
	var req struct {
		Apply bool `json:"apply"`
		// Force skips the model check only (#881); the values still come
		// from the shipped file. Audited.
		Force bool `json:"force"`
	}
	if r.Body != nil && r.ContentLength != 0 {
		if err := decodeJSONBody(w, r, maxOptionalBodyBytes, &req); err != nil {
			respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "invalid request body: "+err.Error())
			return
		}
	}
	profiles, err := h.loadModelProfiles()
	if err != nil {
		respondError(w, http.StatusInternalServerError, "PROFILES_UNREADABLE", err.Error())
		return
	}
	var profile *config.ModelProfile
	for i := range profiles {
		if profiles[i].Name == name {
			profile = &profiles[i]
		}
	}
	if profile == nil {
		respondError(w, http.StatusNotFound, "NOT_FOUND", "no shipped profile named "+name)
		return
	}
	if h.configPath == "" {
		respondError(w, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "config path not wired; cannot write the profile")
		return
	}
	// GitHub #881: check every model the profile names before the lock, so
	// no model call runs under it.
	if off := h.offCatalogue(*profile); len(off) > 0 {
		respondError(w, http.StatusConflict, "MODEL_NOT_IN_CATALOGUE",
			"these models are not in this instance's model catalogue, so nothing was written: "+strings.Join(off, ", "))
		return
	}
	warnings, refused := h.checkProfileModels(r.Context(), *profile, req.Apply && !req.Force)
	if len(refused) > 0 {
		respondError(w, http.StatusConflict, "MODEL_UNREACHABLE",
			"these models did not answer, so nothing was written (force:true skips the check): "+strings.Join(refused, "; "))
		return
	}
	if req.Apply && req.Force {
		warnings = append(warnings, "the profile's models were not checked (force)")
	}

	unlock, lerr := configlock.LockAsWriter(r.Context())
	if lerr != nil {
		respondNotConfigWriter(w, lerr)
		return
	}
	defer unlock()
	writer := &featuredoctor.FileConfigWriter{Path: h.configPath}
	before, err := writer.Read()
	if err != nil {
		respondError(w, http.StatusInternalServerError, "CONFIG_READ", "the config could not be read; nothing changed")
		return
	}
	after, err := config.ApplyModelProfile(before, *profile)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "CONFIG_PATCH", "the config could not be patched; nothing changed")
		return
	}
	body := map[string]any{
		"name": profile.Name, "tiers": profile.Tiers, "roles": profile.Roles,
		"applied": false, "restart_required": false, "warnings": warnings,
	}
	if !req.Apply {
		respondJSON(w, http.StatusOK, body)
		return
	}
	if _, ok := commitConfig(w, writer, after); !ok {
		h.auditModelProfile(r.Context(), r, profile.Name, false, req.Force)
		return
	}
	h.auditModelProfile(r.Context(), r, profile.Name, true, req.Force)
	// runtime.agent_llm is read at process start.
	body["applied"], body["restart_required"] = true, true
	respondJSON(w, http.StatusOK, body)
}

// checkProfileModels runs the #881 model check over every model and fallback
// the profile names, when check is set and a checker is wired. It returns the
// warnings and the refusals ("<model>: <reason>").
func (h *DoctorHandlers) checkProfileModels(ctx context.Context, p config.ModelProfile, check bool) (warnings, refused []string) {
	if !check || h.modelChecker == nil {
		return nil, nil
	}
	var models []string
	for _, m := range []map[string]config.ModelChoice{p.Tiers, p.Roles} {
		for _, c := range m {
			models = append(models, c.Model, c.Fallback)
		}
	}
	res := h.modelChecker.CheckAll(ctx, models)
	for _, m := range sortedResultKeys(res) {
		switch r := res[m]; {
		case !r.OK:
			refused = append(refused, m+": "+r.Reason)
		case r.Warning != "":
			warnings = append(warnings, r.Warning)
		}
	}
	return warnings, refused
}

// offCatalogue lists the profile's models outside a managed instance's
// catalogue; nil when the instance is not managed (#881 C3).
func (h *DoctorHandlers) offCatalogue(p config.ModelProfile) []string {
	if h.managedCatalogue == nil {
		return nil
	}
	cat := h.managedCatalogue()
	seen := map[string]bool{}
	var off []string
	for _, m := range []map[string]config.ModelChoice{p.Tiers, p.Roles} {
		for _, c := range m {
			for _, id := range []string{c.Model, c.Fallback} {
				if id != "" && !seen[id] && !slices.Contains(cat, id) {
					seen[id] = true
					off = append(off, id)
				}
			}
		}
	}
	sort.Strings(off)
	return off
}

func sortedResultKeys(m map[string]modelcheck.Result) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (h *DoctorHandlers) auditModelProfile(ctx context.Context, r *http.Request, name string, ok, forced bool) {
	if h.server == nil || h.server.adminAuditRepo == nil {
		return
	}
	principal := apiKeyPrincipalFromContext(r.Context())
	if principal == "" {
		principal = "anonymous-admin"
	}
	after, _ := json.Marshal(map[string]any{"profile": name, "ok": ok, "model_check_skipped": forced})
	_ = h.server.adminAuditRepo.Insert(ctx, &persistence.AdminAuditEntry{
		Principal: principal, Source: "api", Action: "models.profile.apply", Target: name,
		After: string(after), IP: clientIPFromRequest(r), UserAgent: r.UserAgent(),
	})
}

type roleTierDTO struct {
	Swarm    string `json:"swarm"`
	Role     string `json:"role"`
	Tier     string `json:"tier"`
	Model    string `json:"model"`
	Fallback string `json:"fallback"`
	Source   string `json:"source"`
}

// ModelTiers handles GET /api/v1/models/tiers: each loaded role's tier and
// the model it resolved to. An empty model means the setup model runs it.
func (h *DoctorHandlers) ModelTiers(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "use GET")
		return
	}
	if !h.requireAdminClass(w, r) {
		return
	}
	reg := registry.New()
	if err := reg.Load(h.configDir); err != nil {
		var ve *registry.ValidationError
		if !errors.As(err, &ve) {
			respondError(w, http.StatusInternalServerError, "REGISTRY_LOAD", err.Error())
			return
		}
	}
	var out []roleTierDTO
	for _, sw := range reg.ListSwarms() {
		for _, role := range sw.Roles {
			out = append(out, roleTierDTO{Swarm: sw.ID, Role: role.Name, Tier: role.ModelTier,
				Model: role.Model, Fallback: role.ModelFallback, Source: role.ModelSource})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Swarm != out[j].Swarm {
			return out[i].Swarm < out[j].Swarm
		}
		return out[i].Role < out[j].Role
	})
	respondJSON(w, http.StatusOK, map[string]any{"roles": out})
}
