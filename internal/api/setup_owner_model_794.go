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

func writeSetupStatus(w http.ResponseWriter, status onboarding.Status, reasons []string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(setupStatusResponse{Status: status, RestartPending: len(reasons) > 0, RestartReasons: reasons})
}
