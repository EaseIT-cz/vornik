package api

// Managed setup (GitHub #582 P2, operations-api-contract-design.md §6.2 P2,
// §6.3 F4, F8): one idempotent operation binds a managed instance to its
// customer. It writes the control plane as the OpenID provider, the owner as
// that provider's bootstrap admin, and the provided models (the gateway as the
// chat endpoint, the virtual key in the secrets dir, the catalogue as
// agent_admin.models). A repeat with the same values writes nothing; a
// different owner is refused, since changing an owner is not setup.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/configlock"
	"vornik.io/vornik/internal/featuredoctor"
	"vornik.io/vornik/internal/firstvalue"
	"vornik.io/vornik/internal/onboarding"
)

// IssuerProbe checks that an OpenID issuer answers discovery. The OpenID
// implementation is Enterprise, so the container injects it; nil leaves managed
// setup unavailable.
type IssuerProbe func(ctx context.Context, issuer string) error

// WithIssuerProbe wires managed setup's issuer check (GitHub #582 P2).
func WithIssuerProbe(p IssuerProbe) ServerOption {
	return func(s *Server) { s.issuerProbe = p }
}

// WithFirstValue wires the first-value tracker (GitHub #587): the first
// companion recall with a hit marks it.
func WithFirstValue(t *firstvalue.Tracker) ServerOption {
	return func(s *Server) { s.firstValue = t }
}

const managedCatalogueMax = 64

type managedSetupRequest struct {
	Identity struct {
		Issuer       string `json:"issuer"`
		ClientID     string `json:"client_id"`
		OwnerSubject string `json:"owner_subject"`
		OwnerDisplay string `json:"owner_display"`
	} `json:"identity"`
	Models struct {
		GatewayURL   string `json:"gateway_url"`
		VirtualKey   string `json:"virtual_key"`
		DefaultModel string `json:"default_model"`
		Catalogue    []struct {
			ID      string `json:"id"`
			GoodFor string `json:"good_for"`
		} `json:"catalogue"`
	} `json:"models"`
	// Templates are the sign-up templates to seed (GitHub #587).
	Templates []string `json:"templates,omitempty"`
	// PublicBaseURL and OAuthRedirectURI may arrive here rather than at
	// provision, which then runs before a customer has a handle (GitHub #591).
	PublicBaseURL    string `json:"public_base_url,omitempty"`
	OAuthRedirectURI string `json:"oauth_redirect_uri,omitempty"`
}

// SetupManaged serves POST /api/v1/setup/managed.
func (s *Server) SetupManaged(w http.ResponseWriter, r *http.Request) {
	lister, ok := s.managedSetupReady(w, r)
	if !ok {
		return
	}
	limitJSONBody(w, r)
	req, code, msg := s.parseManagedSetup(r)
	if code != "" {
		respondError(w, http.StatusBadRequest, code, msg)
		return
	}

	name := s.config.Auth.Providers.OIDC.ChannelName()
	owner := name + ":" + req.Identity.OwnerSubject
	seeds, _ := managedSeedsFor(req.Templates)
	if len(seeds) > 0 && (s.projectTemplates == nil || s.configsDir == "") {
		respondError(w, http.StatusServiceUnavailable, "TEMPLATES_NOT_CONFIGURED", "managed setup cannot seed templates on this daemon")
		return
	}

	writer := &featuredoctor.FileConfigWriter{Path: s.setupConfigPath}
	// Phase 1, under the config lock: the key, owner and no-change checks,
	// from the file as written. Nothing changes: answer and stop.
	unlock := configlock.Lock()
	chk := s.managedSetupCheck(w, r, writer, &req, name, owner, seeds)
	unlock()
	if !chk.proceed {
		return
	}
	// The probe makes network calls, so it runs outside the lock: a slow
	// issuer or gateway must not hold up every config writer (GitHub #584,
	// operations-api-contract-design.md §6.6, review 9fd6 F1). A call that
	// only seeds a project changes no identity or model, so it probes
	// nothing.
	if chk.configChanged {
		if status, code, msg := s.probeManagedSetup(r.Context(), lister, &req); code != "" {
			respondError(w, status, code, msg)
			return
		}
	}
	// Phase 2, under the lock again: the file may have changed meanwhile, so
	// every check runs again on what is there now, then the writes.
	unlock = configlock.Lock()
	defer unlock()
	chk = s.managedSetupCheck(w, r, writer, &req, name, owner, seeds)
	if !chk.proceed {
		return
	}
	if chk.configChanged {
		out, err := managedSetupContent(chk.content, &req, name, owner, chk.admins, managedDispatcher(seeds))
		if err != nil {
			respondError(w, http.StatusInternalServerError, "CONFIG_PATCH", "the config could not be patched; nothing changed")
			return
		}
		backup, ok := commitConfig(w, writer, out)
		if !ok {
			return
		}
		// The secret last: it is never rolled back, so it is written only once the
		// config that names it is in place.
		if chk.keyChanged {
			if _, err := onboarding.WriteEnvSecret(s.setupSecretsDir, "chat.env", "VORNIK_CHAT_API_KEY", req.Models.VirtualKey); err != nil {
				_ = writer.Restore(backup)
				respondError(w, http.StatusInternalServerError, "SECRET_WRITE", "the virtual key could not be written; the config was restored")
				return
			}
		}
	}
	// The projects after the config: a template writes files of its own,
	// never config.yaml. A failure leaves the config in place, valid by
	// itself, and the next setup call writes what is missing (§7.2.1).
	seeded, err := s.seedProjects(chk.missing)
	if err != nil {
		s.logger.Error().Err(err).Strs("seeded", seeded).Msg("managed setup: a template could not be seeded")
		respondError(w, http.StatusInternalServerError, "TEMPLATE_WRITE", "a template could not be seeded; the next setup call completes it: "+err.Error())
		return
	}
	if !chk.configChanged && len(seeded) > 0 && s.reloadHook != nil {
		if err := s.reloadHook(); err != nil {
			s.logger.Warn().Err(err).Msg("managed setup: the registry reload after seeding failed; the file watcher picks the projects up")
		}
	}
	s.logger.Info().Str("owner_channel", name).Int("catalogue", len(req.Models.Catalogue)).
		Bool("config_changed", chk.configChanged).Strs("seeded", seeded).Msg("managed setup written")
	respondJSON(w, http.StatusOK, map[string]any{"changed": true, "restart_required": chk.configChanged, "seeded": seeded})
}

// managedCheck is managedSetupCheck's result: proceed false means the
// request was answered.
type managedCheck struct {
	content       []byte
	admins        []string
	keyChanged    bool
	configChanged bool
	missing       []managedSeed
	proceed       bool
}

// managedSetupCheck reads the config and runs managed setup's checks, the
// caller holding configlock: the current instance_admin key, the bound
// owner, and whether anything would change, the config or a seeded project.
// It answers the request itself when it stops it (proceed false),
// "changed": false included.
func (s *Server) managedSetupCheck(w http.ResponseWriter, r *http.Request, writer *featuredoctor.FileConfigWriter, req *managedSetupRequest, name, owner string, seeds []managedSeed) managedCheck {
	content, err := writer.Read()
	if err != nil {
		respondError(w, http.StatusInternalServerError, "CONFIG_READ", "the config file could not be read")
		return managedCheck{}
	}
	// Only the control plane's current instance_admin key binds a customer:
	// never a session, never the previous key of a rotation (commit security
	// review 2026-10-29, finding 3).
	if !isCurrentAdminKeyInFile(content, extractAPIKey(r)) {
		respondError(w, http.StatusForbidden, "NOT_CURRENT_KEY", "managed setup takes the current instance_admin key only")
		return managedCheck{}
	}
	admins := config.GetYAMLStringList(content, "auth.bootstrap_admins")
	if ownerConflict(content, admins, name, owner, req) {
		respondError(w, http.StatusConflict, "OWNER_BOUND", "this instance is bound to another owner or issuer; changing an owner is not setup")
		return managedCheck{}
	}
	if status, code, msg := originCheck(content, req); code != "" {
		respondError(w, status, code, msg)
		return managedCheck{}
	}
	keyChanged := readEnvSecret(filepath.Join(s.setupSecretsDir, "chat.env"), "VORNIK_CHAT_API_KEY") != req.Models.VirtualKey
	configChanged := keyChanged || !managedConfigCurrent(content, req, name, owner, admins, managedDispatcher(seeds))
	missing := s.missingSeeds(seeds)
	if !configChanged && len(missing) == 0 {
		respondJSON(w, http.StatusOK, map[string]any{"changed": false, "seeded": []string{}})
		return managedCheck{}
	}
	return managedCheck{content: content, admins: admins, keyChanged: keyChanged, configChanged: configChanged, missing: missing, proceed: true}
}

// ownerConflict reports whether the request would change a bound owner: another
// subject under the provider's channel, or, once an owner is bound, another
// issuer or client id, under which the same subject names someone else
// (commit security review 2026-10-29, finding 4).
func ownerConflict(content []byte, admins []string, name, owner string, req *managedSetupRequest) bool {
	bound := false
	for _, a := range admins {
		if strings.HasPrefix(a, name+":") {
			bound = true
			if a != owner {
				return true
			}
		}
	}
	if !bound {
		return false
	}
	iss := config.GetYAMLString(content, "auth.providers.oidc.issuer")
	cid := config.GetYAMLString(content, "auth.providers.oidc.client_id")
	return (iss != "" && iss != req.Identity.Issuer) || (cid != "" && cid != req.Identity.ClientID)
}

// originCheck holds the body's origin and router URI against the file (GitHub
// #591): a bound value is never changed, a router URI passes §16 R1 beside the
// origin it will sit with, and a login provider needs some origin. code is ""
// when the body may proceed.
func originCheck(content []byte, req *managedSetupRequest) (int, string, string) {
	origin := strings.TrimRight(config.GetYAMLString(content, "server.public_base_url"), "/")
	if origin == "" {
		origin = strings.TrimRight(config.GetYAMLString(content, "auth.external_base_url"), "/")
	}
	router := config.GetYAMLString(content, "hosting.oauth_redirect_uri")
	if (req.PublicBaseURL != "" && origin != "" && req.PublicBaseURL != origin) ||
		(req.OAuthRedirectURI != "" && router != "" && req.OAuthRedirectURI != router) {
		return http.StatusConflict, "ORIGIN_BOUND", "this instance already has another public origin or router URI; renaming an instance is not setup"
	}
	if req.PublicBaseURL != "" {
		origin = req.PublicBaseURL
	}
	// R1 runs even with no origin anywhere: it refuses a router URI without one.
	if req.OAuthRedirectURI != "" {
		if err := config.CheckOAuthRedirect(origin, config.GetYAMLString(content, "hosting.instance_id"), req.OAuthRedirectURI); err != nil {
			return http.StatusBadRequest, "BAD_BODY", err.Error()
		}
	}
	if origin == "" {
		return http.StatusConflict, "NO_PUBLIC_ORIGIN", "a login provider needs a public origin; send public_base_url"
	}
	return 0, "", ""
}

// managedSetupReady answers the operation's preconditions itself (method,
// managed hosting, wiring) and returns the model lister.
func (s *Server) managedSetupReady(w http.ResponseWriter, r *http.Request) (setupModelLister, bool) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "use POST")
		return nil, false
	}
	if s.config == nil || !s.config.Hosting.Managed() {
		respondError(w, http.StatusConflict, "NOT_MANAGED", "managed setup is an operation of managed hosting")
		return nil, false
	}
	lister, ok := s.setupValidator.(setupModelLister)
	if s.setupConfigPath == "" || s.setupSecretsDir == "" || s.issuerProbe == nil || !ok || s.setupValidator == nil {
		respondError(w, http.StatusServiceUnavailable, "SETUP_NOT_CONFIGURED", "managed setup is not wired")
		return nil, false
	}
	return lister, true
}

// parseManagedSetup decodes and checks the body; code is "" when it is good.
func (s *Server) parseManagedSetup(r *http.Request) (managedSetupRequest, string, string) {
	var req managedSetupRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return req, "BAD_BODY", "the body must be {identity, models}: " + err.Error()
	}
	if msg := validateManagedSetup(&req); msg != "" {
		return req, "BAD_BODY", msg
	}
	if !gatewayVouched(req.Models.GatewayURL, s.config.Hosting.ModelHosts) {
		return req, "GATEWAY_NOT_VOUCHED", "gateway_url must be https, with its host in hosting.model_hosts"
	}
	return req, "", ""
}

// probeManagedSetup checks the issuer's discovery and that the gateway lists
// every catalogue model with the virtual key; code is "" when both hold.
func (s *Server) probeManagedSetup(ctx context.Context, lister setupModelLister, req *managedSetupRequest) (int, string, string) {
	if err := s.issuerProbe(ctx, req.Identity.Issuer); err != nil {
		return http.StatusBadGateway, "ISSUER_UNAVAILABLE", "the issuer's discovery document did not answer: " + err.Error()
	}
	listed, err := lister.ListModels(ctx, req.Models.GatewayURL, req.Models.VirtualKey)
	if err != nil {
		return http.StatusBadGateway, "GATEWAY_UNAVAILABLE", "the gateway did not list its models with the virtual key"
	}
	have := map[string]bool{}
	for _, m := range listed {
		have[m.ID] = true
	}
	for _, m := range req.Models.Catalogue {
		if !have[m.ID] {
			return http.StatusBadRequest, "MODEL_NOT_LISTED", fmt.Sprintf("the gateway does not list catalogue model %q", m.ID)
		}
	}
	return 0, "", ""
}

// validateManagedSetup checks the body's shape; "" when it is well formed.
func validateManagedSetup(req *managedSetupRequest) string {
	id, m := req.Identity, req.Models
	if u, err := url.Parse(id.Issuer); id.Issuer == "" || err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "identity.issuer must be an https URL with no userinfo, query or fragment"
	}
	if id.ClientID == "" || len(id.ClientID) > 256 {
		return "identity.client_id is required (at most 256 characters)"
	}
	if id.OwnerSubject == "" || len(id.OwnerSubject) > 255 || strings.ContainsAny(id.OwnerSubject, " \t\r\n") {
		return "identity.owner_subject is required: the issuer's sub claim, at most 255 characters, no whitespace"
	}
	if len(id.OwnerDisplay) > 256 {
		return "identity.owner_display is at most 256 characters"
	}
	if m.VirtualKey == "" || len(m.VirtualKey) > 1024 || strings.ContainsAny(m.VirtualKey, "\r\n") {
		return "models.virtual_key is required: one line, at most 1024 characters"
	}
	if len(m.Catalogue) == 0 || len(m.Catalogue) > managedCatalogueMax {
		return fmt.Sprintf("models.catalogue needs 1 to %d models", managedCatalogueMax)
	}
	seen := map[string]bool{}
	for _, c := range m.Catalogue {
		if c.ID == "" || len(c.ID) > 128 || len(c.GoodFor) > 200 || seen[c.ID] {
			return "every models.catalogue entry needs a unique id (at most 128 characters) and good_for of at most 200"
		}
		seen[c.ID] = true
	}
	if !seen[m.DefaultModel] {
		return "models.default_model must be one of the catalogue's ids"
	}
	if _, msg := managedSeedsFor(req.Templates); msg != "" {
		return msg
	}
	req.PublicBaseURL = strings.TrimRight(req.PublicBaseURL, "/")
	if o := req.PublicBaseURL; o != "" {
		if u, err := url.Parse(o); err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
			return "public_base_url must be an https origin: a host and optional port, no userinfo, path, query or fragment"
		}
	}
	return ""
}

// gatewayVouched: an https URL whose host is one of hosting.model_hosts.
func gatewayVouched(raw string, hosts []string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil {
		return false
	}
	return slices.Contains(hosts, u.Hostname())
}

// managedConfigCurrent reports whether content already holds every value
// managed setup would write.
func managedConfigCurrent(content []byte, req *managedSetupRequest, name, owner string, admins []string, dispatcher string) bool {
	get := func(k string) string { return config.GetYAMLString(content, k) }
	if dispatcher != "" && get("telegram.dispatcher_project_id") != dispatcher {
		return false
	}
	if req.PublicBaseURL != "" && (get("server.public_base_url") != req.PublicBaseURL || get("auth.external_base_url") != req.PublicBaseURL) {
		return false
	}
	if req.OAuthRedirectURI != "" && get("hosting.oauth_redirect_uri") != req.OAuthRedirectURI {
		return false
	}
	if get("auth.providers.oidc.issuer") != req.Identity.Issuer ||
		get("auth.providers.oidc.client_id") != req.Identity.ClientID ||
		get("auth.providers.oidc.name") != name ||
		!slices.Contains(admins, owner) ||
		get("chat.enabled") != "true" || get("chat.provider") != "http" ||
		get("chat.endpoint") != req.Models.GatewayURL || get("chat.model") != req.Models.DefaultModel ||
		get("chat.api_key") != "${VORNIK_CHAT_API_KEY}" {
		return false
	}
	var cur struct {
		AgentAdmin struct {
			Models []config.AgentModelConfig `yaml:"models"`
		} `yaml:"agent_admin"`
	}
	if err := yaml.Unmarshal(content, &cur); err != nil || len(cur.AgentAdmin.Models) != len(req.Models.Catalogue) {
		return false
	}
	for i, m := range req.Models.Catalogue {
		if cur.AgentAdmin.Models[i].ID != m.ID || cur.AgentAdmin.Models[i].GoodFor != m.GoodFor {
			return false
		}
	}
	return true
}

// managedSetupContent writes managed setup's values into content.
func managedSetupContent(content []byte, req *managedSetupRequest, name, owner string, admins []string, dispatcher string) ([]byte, error) {
	if !slices.Contains(admins, owner) {
		admins = append(slices.Clone(admins), owner)
	}
	out, err := setYAML(content,
		yamlSet{"auth.providers.oidc.issuer", req.Identity.Issuer},
		yamlSet{"auth.providers.oidc.client_id", req.Identity.ClientID},
		yamlSet{"auth.providers.oidc.name", name},
		yamlSet{"auth.bootstrap_admins", admins},
		yamlSet{"chat.enabled", true},
		yamlSet{"chat.provider", "http"},
		yamlSet{"chat.endpoint", req.Models.GatewayURL},
		yamlSet{"chat.model", req.Models.DefaultModel},
		yamlSet{"chat.api_key", "${VORNIK_CHAT_API_KEY}"},
	)
	if err != nil {
		return nil, err
	}
	if req.PublicBaseURL != "" {
		// Both keys name one fact, as provision writes them (GitHub #591).
		if out, err = setYAML(out, yamlSet{"server.public_base_url", req.PublicBaseURL}, yamlSet{"auth.external_base_url", req.PublicBaseURL}); err != nil {
			return nil, err
		}
	}
	if req.OAuthRedirectURI != "" {
		if out, _, err = config.SetYAMLKey(out, "hosting.oauth_redirect_uri", req.OAuthRedirectURI); err != nil {
			return nil, err
		}
	}
	if dispatcher != "" {
		// The assistant template answers the web chat (GitHub #587).
		if out, _, err = config.SetYAMLKey(out, "telegram.dispatcher_project_id", dispatcher); err != nil {
			return nil, err
		}
	}
	if out, _, err = config.DeleteYAMLKey(out, "agent_admin.models"); err != nil {
		return nil, err
	}
	for _, m := range req.Models.Catalogue {
		out, err = config.AppendYAMLListItem(out, "agent_admin.models", []config.YAMLListField{{Key: "id", Value: m.ID}, {Key: "good_for", Value: m.GoodFor}})
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// readEnvSecret returns NAME's value in an env file, "" when absent.
func readEnvSecret(path, name string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), name+"="); ok {
			return v
		}
	}
	return ""
}
