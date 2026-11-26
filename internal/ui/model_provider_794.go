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
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/chat"
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
	// ListModels lists a base's models without the invocation check; the
	// page's "did you mean" asks it of two nearby bases (GitHub #807).
	ListModels(ctx context.Context, endpoint, apiKey string) ([]chat.ModelInfo, error)
}

// WithOwnerMemoryProbe wires the page's embeddings probe (GitHub #807):
// onboarding.NewManagedMemoryValidator, the managed dial guard.
func WithOwnerMemoryProbe(p onboarding.MemoryValidatorInterface) ServerOption {
	return func(s *Server) { s.ownerMemoryProbe = p }
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
	// The optional embeddings section (GitHub #807): blank model, no memory.
	FormEmbeddingEndpoint  string
	FormEmbeddingModel     string
	FormEmbeddingDimension string
	MemoryAvailable        bool
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
	}, MemoryAvailable: s.ownerMemoryProbe != nil}
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
		if !res.ModelsListed {
			data.Error += s.didYouMean(r.Context(), endpoint, key)
		}
		return http.StatusBadRequest
	}
	mem, status := s.ownerMemoryFromForm(r, data, endpoint, key)
	if status != 0 {
		return status
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
	if mem != nil && mem.ownKey != "" {
		if err := s.operatorCredentials.Put(r.Context(), api.OwnerEmbeddingAPIKeyName, []byte(mem.ownKey), who); err != nil {
			s.logger.Warn().Err(err).Msg("model provider: the embedding key could not be stored")
			data.Error = "the chat key was stored, but the embedding key could not be; the configuration was not changed"
			return http.StatusInternalServerError
		}
	}
	writer := &featuredoctor.FileConfigWriter{Path: s.cpConfigPath}
	if msg := writeOwnerModelConfig(writer, endpoint, model, mem); msg != "" {
		s.logger.Warn().Str("step", msg).Msg("model provider: the configuration was not written")
		data.Error = "the key was stored, but the configuration could not be written (" + msg + "); nothing else changed"
		if mem != nil && mem.ownKey != "" {
			data.Error = "the chat and embedding keys were stored, but the configuration could not be written (" + msg + "); nothing else changed"
		}
		return http.StatusInternalServerError
	}
	audit := s.auditModelProvider(r, host, model, mem)
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
func writeOwnerModelConfig(writer *featuredoctor.FileConfigWriter, endpoint, model string, mem *ownerMemory) string {
	content, err := writer.Read()
	if err != nil {
		return "read"
	}
	out, err := api.OwnerModelProviderContent(content, endpoint, model)
	if err != nil {
		return "compose"
	}
	if mem != nil {
		if out, err = api.OwnerMemoryContent(out, mem.endpoint, mem.model, mem.dimension, mem.keyName); err != nil {
			return "compose"
		}
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
func (s *Server) auditModelProvider(r *http.Request, host, model string, mem *ownerMemory) error {
	detail := map[string]any{"model": model, "restart_pending": true}
	if mem != nil {
		detail["memory"] = "on"
		detail["embedding_host"] = mem.host
		detail["embedding_model"] = mem.model
	}
	return s.auditKeyAction(r, "model_provider.set", host, detail)
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
	s.bumpRestartGeneration()
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

// ownerMemory is the embeddings section of a save (GitHub #807, security
// design 18 amendment 2026-11-18 item 1).
type ownerMemory struct {
	endpoint, host, model string
	dimension             int
	keyName               string // credential-store name memory.embedding_api_key refers to
	ownKey                string // a key the owner typed for embeddings, or "" (the chat key is used)
}

// ownerMemoryFromForm reads and probes the embeddings section. A blank model
// means no memory change. It returns a status to send, or 0. Nothing is
// written here: the save is one unit (design item 1).
func (s *Server) ownerMemoryFromForm(r *http.Request, data *ModelProviderData, chatEndpoint, chatKey string) (*ownerMemory, int) {
	model := strings.TrimSpace(r.PostFormValue("embedding_model"))
	endpoint := strings.TrimSpace(r.PostFormValue("embedding_endpoint"))
	dim := strings.TrimSpace(r.PostFormValue("embedding_dimension"))
	ownKey := r.PostFormValue("embedding_api_key")
	data.FormEmbeddingEndpoint, data.FormEmbeddingModel, data.FormEmbeddingDimension = endpoint, model, dim
	if model == "" {
		return nil, 0
	}
	if endpoint == "" {
		endpoint = chatEndpoint
	}
	key := ownKey
	if key == "" {
		key = chatKey
	}
	host, msg := checkProviderInput(endpoint, model, key)
	if msg != "" {
		data.Error = "Embeddings: " + msg
		return nil, http.StatusBadRequest
	}
	typed := 0
	if dim != "" {
		n, err := strconv.Atoi(dim)
		if err != nil || n <= 0 || n > 65536 {
			data.Error = "Embeddings: the dimension must be a whole number, or blank to take the provider's."
			return nil, http.StatusBadRequest
		}
		typed = n
	}
	if ownKey != "" {
		if _, set := s.envLookup()(api.OwnerEmbeddingAPIKeyName); set {
			data.Error = "This instance's environment sets " + api.OwnerEmbeddingAPIKeyName + ", which would be used instead of your key. Ask your operator to remove it."
			return nil, http.StatusConflict
		}
	}
	if s.ownerMemoryProbe == nil {
		data.Error = "connecting embeddings is not available on this daemon; nothing was changed"
		return nil, http.StatusServiceUnavailable
	}
	res := s.ownerMemoryProbe.Validate(r.Context(), onboarding.MemoryConfigProposal{
		Enabled: true, EmbeddingEndpoint: endpoint, EmbeddingAPIKey: key, EmbeddingModel: model, EmbeddingDimension: typed})
	if !res.EmbeddingOK {
		data.Error = "Embeddings: the provider check failed. Check the base URL, the key and the embedding model; nothing was changed."
		return nil, http.StatusBadRequest
	}
	if typed != 0 && typed != res.ReturnedDimension {
		data.Error = fmt.Sprintf("Embeddings: the provider returns %d dimensions, not %d. Leave the dimension blank to take the provider's; nothing was changed.", res.ReturnedDimension, typed)
		return nil, http.StatusBadRequest
	}
	m := &ownerMemory{endpoint: endpoint, host: host, model: model, dimension: res.ReturnedDimension, keyName: api.OwnerModelAPIKeyName}
	if ownKey != "" {
		m.keyName, m.ownKey = api.OwnerEmbeddingAPIKeyName, ownKey
	}
	return m, 0
}

// didYouMean asks two nearby bases, <base>/v1 and <origin>/api/v1, for their
// models with the same key and through the same guard, when the typed base
// lists none. It returns the error's suffix naming the first that answers,
// or "". Nothing is rewritten: the owner retypes (GitHub #807, item 4).
func (s *Server) didYouMean(ctx context.Context, endpoint, key string) string {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		return ""
	}
	base := strings.TrimRight(endpoint, "/")
	var tries []string
	if !strings.HasSuffix(strings.ToLower(base), "/v1") {
		tries = append(tries, base+"/v1")
	}
	if alt := u.Scheme + "://" + u.Host + "/api/v1"; alt != base && (len(tries) == 0 || tries[0] != alt) {
		tries = append(tries, alt)
	}
	for _, try := range tries {
		if models, err := s.ownerModelProbe.ListModels(ctx, try, key); err == nil && len(models) > 0 {
			return " Did you mean " + try + "?"
		}
	}
	return ""
}
