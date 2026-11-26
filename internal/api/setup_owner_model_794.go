package api

import (
	"encoding/json"
	"net/http"
	"sort"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/onboarding"
)

// setup_owner_model_794.go — a managed instance's owner connects their own
// model provider (GitHub #794; security design 18, amendment 2026-11-16) and
// the control plane learns that a restart is due (operations-API design
// §6.12).

// OwnerModelAPIKeyName is the operator credential the owner's provider key is
// sealed under. A distinct name, so a VORNIK_CHAT_API_KEY that provided models
// left in chat.env cannot shadow it: a ${NAME} placeholder resolves from the
// environment before the credential store (#626).
const OwnerModelAPIKeyName = "OWNER_MODEL_API_KEY"

// OwnerEmbeddingAPIKeyName is the credential-store name of an embedding key
// the owner typed on the model-provider page, beside OwnerModelAPIKeyName
// (GitHub #807, security design 18 amendment 2026-11-18).
const OwnerEmbeddingAPIKeyName = "OWNER_EMBEDDING_API_KEY"

// RestartReasonModelProvider is the restart-pending code the owner's save
// marks; a restart clears it (operations-API design §6.12 R1).
const RestartReasonModelProvider = "model_provider"

// OwnerModelProviderContent returns content with the owner's provider written
// as the chat block setup's commit writes (writeChatConfig), the key behind
// ${OWNER_MODEL_API_KEY}, and agent_admin.models reduced to the chosen model,
// so agent roles are not offered a provided gateway's models the instance can
// no longer reach (design 18, amendment 2026-11-16, P5 step 2).
func OwnerModelProviderContent(content []byte, endpoint, model string) ([]byte, error) {
	out, err := setYAML(content,
		yamlSet{"chat.enabled", true},
		yamlSet{"chat.provider", "http"},
		yamlSet{"chat.endpoint", endpoint},
		yamlSet{"chat.model", model},
		yamlSet{"chat.api_key", "${" + OwnerModelAPIKeyName + "}"},
	)
	if err != nil {
		return nil, err
	}
	if out, _, err = config.DeleteYAMLKey(out, "agent_admin.models"); err != nil {
		return nil, err
	}
	return config.AppendYAMLListItem(out, "agent_admin.models", []config.YAMLListField{
		{Key: "id", Value: model},
		{Key: "good_for", Value: "the default model, from your own provider"},
	})
}

// OwnerMemoryContent returns content with memory enabled on the owner's
// embedding provider: the OpenAI-compatible endpoint, the model, the
// dimension the probe returned, and the key behind keyName (a credential
// store name, never the key). GitHub #807.
func OwnerMemoryContent(content []byte, endpoint, model string, dimension int, keyName string) ([]byte, error) {
	return setYAML(content,
		yamlSet{"memory.enabled", true},
		yamlSet{"memory.embedding_provider", ""},
		yamlSet{"memory.embedding_endpoint", endpoint},
		yamlSet{"memory.embedding_model", model},
		yamlSet{"memory.embedding_dimension", dimension},
		yamlSet{"memory.embedding_api_key", "${" + keyName + "}"},
	)
}

// WithRestartPendingSource wires the one restart-pending registry's reader
// (operations-API design §6.12 R1): the registry's reason codes plus
// config:<path> for each restart-only field that differs. Names only.
func WithRestartPendingSource(f func() []string) ServerOption {
	return func(s *Server) { s.restartPendingSource = f }
}

// setupStatusResponse is the detector's status plus the restart signal
// (§6.12 R2). The embedded struct's fields stay at the top level.
type setupStatusResponse struct {
	onboarding.Status
	RestartPending bool     `json:"restart_pending"`
	RestartReasons []string `json:"restart_reasons"`
	// The restart generation (§6.13 G2, G3; GitHub #799): left out when the
	// counter could not be read, at start or now.
	PendingGeneration *int64 `json:"pending_generation,omitempty"`
	LoadedGeneration  *int64 `json:"loaded_generation,omitempty"`
}

// RestartGeneration is what setup/status reads for §6.13: the counter now,
// and the value the daemon read at start (ok false: it did not parse).
type RestartGeneration interface {
	Read() (int64, bool)
}

// WithRestartGeneration wires the counter and the value read at start.
func WithRestartGeneration(g RestartGeneration, loaded int64, loadedOK bool) ServerOption {
	return func(s *Server) { s.restartGen, s.restartGenLoaded, s.restartGenLoadedOK = g, loaded, loadedOK }
}

// restartGenerations returns both fields, or neither when either is unknown.
func (s *Server) restartGenerations() (pending, loaded *int64) {
	if s.restartGen == nil || !s.restartGenLoadedOK {
		return nil, nil
	}
	p, ok := s.restartGen.Read()
	if !ok {
		s.logger.Error().Msg("restart generation file does not parse; setup/status leaves the generations out")
		return nil, nil
	}
	l := s.restartGenLoaded
	return &p, &l
}

// restartReasons returns the wired source's reasons, sorted and unique, and
// an empty (not nil) list when nothing is pending or nothing is wired.
func (s *Server) restartReasons() []string {
	out := []string{}
	if s.restartPendingSource == nil {
		return out
	}
	seen := map[string]bool{}
	for _, r := range s.restartPendingSource() {
		if r != "" && !seen[r] {
			seen[r] = true
			out = append(out, r)
		}
	}
	sort.Strings(out)
	return out
}

func writeSetupStatus(w http.ResponseWriter, status onboarding.Status, reasons []string, pending, loaded *int64) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(setupStatusResponse{Status: status, RestartPending: len(reasons) > 0, RestartReasons: reasons,
		PendingGeneration: pending, LoadedGeneration: loaded})
}
