package ui

// model_provider_794.go — a managed instance's owner connects their own model
// provider (GitHub #794, #779; 18-security-and-tenancy.md amendment
// 2026-11-16). The page probes the provider under managed mode's dial rule,
// seals the key, writes the chat block under the config lock and writer
// fence, records the change, and marks a restart the control plane performs
// (operations-API design §6.12).

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
	"unicode"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/configlock"
	"vornik.io/vornik/internal/featuredoctor"
	"vornik.io/vornik/internal/onboarding"
)

const (
	maxProviderEndpointBytes = 2048
	maxProviderModelBytes    = 200
	maxProviderKeyBytes      = 4096
)

// OwnerModelProbe checks a proposed provider before anything is written. The
// service wires onboarding.NewManagedChatValidator(hosting.model_hosts), whose
// dial rule is managed mode's run-time rule for models (design P4/F1).
type OwnerModelProbe interface {
	Validate(ctx context.Context, p onboarding.ChatConfigProposal) onboarding.ChatValidationResult
}

// WithOwnerModelProbe wires the model-provider page's probe.
func WithOwnerModelProbe(p OwnerModelProbe) ServerOption {
	return func(s *Server) { s.ownerModelProbe = p }
}

// ModelProviderData backs model_provider.html.
type ModelProviderData struct {
	adminCommonData
	Endpoint       string // scheme and host of the configured provider, never a key
	Model          string
	RestartPending bool
	Saved          bool
	Error          string
	// Notice is a warning about a save that succeeded: its audit row or its
	// live reload failed (code review review-20261116-9905 findings 3, 4).
	Notice       string
	FormEndpoint string
	FormModel    string
}

// ModelProvider serves /ui/model-provider: GET shows the configured provider,
// POST connects a new one.
func (s *Server) ModelProvider(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	if !s.managedHosting {
		http.NotFound(w, r)
		return
	}
	switch api.OperatorSurfaceAccessFor(r, s.isAdminKeyForSurface, api.SurfaceModelProvider) {
	case api.OperatorSurfaceAllowed:
	case api.OperatorSurfaceHidden:
		http.NotFound(w, r)
		return
	default:
		http.Error(w, "admin scope required", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	data := ModelProviderData{adminCommonData: adminCommonData{
		Title:       "Model provider",
		CurrentPage: "model-provider",
		IsAdmin:     api.IsAdminClassRequest(r, s.operatorCapability.IsAdminKey),
	}}
	if r.Method == http.MethodPost {
		if code := s.modelProviderPost(w, r, &data); code != 0 {
			w.WriteHeader(code)
		}
	}
	if cfg := s.onboardingDetector.Config; cfg != nil {
		data.Endpoint = schemeAndHost(cfg.Chat.Endpoint)
		data.Model = cfg.Chat.Model
	}
	data.RestartPending = s.modelProviderPending()
	s.render(w, "model_provider.html", data)
}

// modelProviderPost runs the checks, the probe and the writes in the
// design's order. It returns a status to send, or 0 for 200.
func (s *Server) modelProviderPost(w http.ResponseWriter, r *http.Request, data *ModelProviderData) int {
	r.Body = http.MaxBytesReader(w, r.Body, maxProviderEndpointBytes+maxProviderModelBytes+maxProviderKeyBytes+4096)
	if err := r.ParseForm(); err != nil {
		data.Error = "the form could not be read"
		return http.StatusBadRequest
	}
	endpoint := strings.TrimSpace(r.PostFormValue("endpoint"))
	model := strings.TrimSpace(r.PostFormValue("model"))
	key := r.PostFormValue("api_key")
	data.FormEndpoint, data.FormModel = endpoint, model
	host, msg := checkProviderInput(endpoint, model, key)
	if msg != "" {
		data.Error = msg
		return http.StatusBadRequest
	}
	if _, set := s.envLookup()(api.OwnerModelAPIKeyName); set {
		data.Error = "This instance's environment sets " + api.OwnerModelAPIKeyName + ", which would be used instead of your key. Ask your operator to remove it."
		return http.StatusConflict
	}
	if s.ownerModelProbe == nil || s.operatorCredentials == nil || s.cpConfigPath == "" {
		data.Error = "connecting a model provider is not available on this daemon"
		return http.StatusServiceUnavailable
	}
	res := s.ownerModelProbe.Validate(r.Context(), onboarding.ChatConfigProposal{Endpoint: endpoint, APIKey: key, Model: model})
	if !res.PingOK || !res.ModelKnown {
		data.Error = probeFailureText(res)
		return http.StatusBadRequest
	}

	unlock, err := configlock.LockAsWriter(r.Context())
	if err != nil {
		if errors.Is(err, configlock.ErrNotConfigWriter) {
			data.Error = "another daemon writes this instance's configuration; nothing was changed"
			return http.StatusConflict
		}
		data.Error = "the configuration is busy; try again"
		return http.StatusServiceUnavailable
	}
	defer unlock()

	who := credentialsWho(r, credentialsActor(r))
	if err := s.operatorCredentials.Put(r.Context(), api.OwnerModelAPIKeyName, []byte(key), who); err != nil {
		s.logger.Warn().Err(err).Msg("model provider: the key could not be stored")
		data.Error = "the key could not be stored; nothing was changed"
		return http.StatusInternalServerError
	}
	writer := &featuredoctor.FileConfigWriter{Path: s.cpConfigPath}
	if msg := writeOwnerModelConfig(writer, endpoint, model); msg != "" {
		s.logger.Warn().Str("step", msg).Msg("model provider: the configuration was not written")
		data.Error = "the key was stored, but the configuration could not be written (" + msg + "); nothing else changed"
		return http.StatusInternalServerError
	}
	audit := s.auditModelProvider(r, host, model)
	s.markModelProviderPending()
	reload := s.applyConfigEdit("model provider")
	data.Saved = true
	switch {
	case audit != nil:
		data.Notice = keysAuditNotRecorded
	case reload.Level == "error":
		data.Notice = reload.Message
	}
	return 0
}

// writeOwnerModelConfig writes the chat block through the config writer:
// backup, write, validate, and restore on any failure. It returns "" or the
// failed step.
func writeOwnerModelConfig(writer *featuredoctor.FileConfigWriter, endpoint, model string) string {
	content, err := writer.Read()
	if err != nil {
		return "read"
	}
	out, err := api.OwnerModelProviderContent(content, endpoint, model)
	if err != nil {
		return "compose"
	}
	backup, err := writer.Backup()
	if err != nil {
		return "backup"
	}
	if err := writer.Write(out); err != nil {
		_ = writer.Restore(backup)
		return "write"
	}
	if err := writer.Validate(); err != nil {
		_ = writer.Restore(backup)
		return "validate"
	}
	return ""
}

// auditModelProvider writes the model_provider.set row: the endpoint's host
// and the model, never the key (design P5 step 3). It is also the record of
// the egress decision #794 asks for.
func (s *Server) auditModelProvider(r *http.Request, host, model string) error {
	return s.auditKeyAction(r, "model_provider.set", host, map[string]any{"model": model, "restart_pending": true})
}

// checkProviderInput validates the form (design P4) and returns the
// endpoint's host, or a message for the owner.
func checkProviderInput(endpoint, model, key string) (string, string) {
	if endpoint == "" || len(endpoint) > maxProviderEndpointBytes {
		return "", "Enter the provider's base URL (at most 2048 characters)."
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.Hostname() == "" {
		return "", "The base URL must be an https address with a host, for example https://api.example.com/v1."
	}
	if u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(endpoint, "#") {
		return "", "The base URL must not contain a user name, password, query or fragment."
	}
	if model == "" || len(model) > maxProviderModelBytes || hasControl(model) {
		return "", "Enter the model's id (at most 200 characters)."
	}
	if key == "" || len(key) > maxProviderKeyBytes || hasControl(key) {
		return "", "Enter the API key (at most 4096 characters)."
	}
	return strings.ToLower(u.Hostname()), ""
}

func hasControl(s string) bool {
	return strings.IndexFunc(s, unicode.IsControl) >= 0
}

// probeFailureText is the owner's message for a failed probe: the failure
// class's remediation, which is Vornik's own text. The validator's Message can
// quote the provider's response, so it is never shown.
func probeFailureText(res onboarding.ChatValidationResult) string {
	for _, f := range res.Failures {
		if f.Severity == "blocking" && f.Remediation != "" {
			return "The provider check failed: " + f.Remediation
		}
	}
	if !res.ModelKnown {
		return "The provider does not list this model. Check the model id."
	}
	return "The provider check failed. Check the base URL, the key and the model."
}

// schemeAndHost shows a configured endpoint without its path, user info or
// query.
func schemeAndHost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// envLookup is the environment lookup, replaceable in tests.
func (s *Server) envLookup() func(string) (string, bool) {
	if s.lookupEnv != nil {
		return s.lookupEnv
	}
	return os.LookupEnv
}

// markModelProviderPending records the model_provider restart reason
// (operations-API design §6.12 R1). Only a restart clears it.
func (s *Server) markModelProviderPending() {
	s.modelProviderSince.CompareAndSwap(0, time.Now().UnixNano())
}

func (s *Server) modelProviderPending() bool {
	return s.modelProviderSince.Load() != 0
}

// RestartPendingCodes is the one restart-pending registry's reader (§6.12
// R1): "reload" while the banner's reload mark stands (with its self-clear
// applied, as the banner applies it) and "model_provider" after the owner's
// save. The service wires it into the API's setup/status.
func (s *Server) RestartPendingCodes() []string {
	var out []string
	if s.restartBanner().Pending {
		out = append(out, "reload")
	}
	if s.modelProviderPending() {
		out = append(out, api.RestartReasonModelProvider)
	}
	return out
}
