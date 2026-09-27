package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"vornik.io/vornik/internal/configdrift"
)

// ackRequest is the body of POST /api/v1/doctor/ack.
type ackRequest struct {
	Check string `json:"check"`
	File  string `json:"file"`
}

type ackRecordJSON struct {
	Rel    string `json:"rel,omitempty"`
	Class  string `json:"class"`
	Key    string `json:"key"`
	Regime string `json:"regime"`
}

type ackResponse struct {
	File     string          `json:"file"`
	Recorded []ackRecordJSON `json:"recorded"`
	Pruned   []ackRecordJSON `json:"pruned"`
	Date     string          `json:"date"`
	// Actor is who the acknowledgement records (drift design slice F).
	Actor string `json:"actor"`
}

// AckDoctorFinding handles POST /api/v1/doctor/ack: record that an operator
// declines what config_template_drift reports for one deployed file (drift
// design, slice C).
//
// The DAEMON writes, so the acknowledgement lands in the tree the daemon
// resolved — the one the check reads — not wherever the CLI would resolve
// it. Admin-gated and fail-CLOSED, like `doctor features enable`: an ack
// silences a WARNING, which is a configuration decision. Only
// config_template_drift is acknowledgeable. The ack covers every CURRENT
// finding of the file (acknowledged or not, so a re-ack restores a store a
// restore emptied); a file with none is refused, as is an ack against a
// baseline that is unstamped or not this binary's.
func (h *DoctorHandlers) AckDoctorFinding(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "use POST")
		return
	}
	if h.server == nil {
		respondError(w, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "server not wired; cannot authenticate")
		return
	}
	if !h.server.requireAdminGate(w, r) {
		return
	}
	var req ackRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "BAD_REQUEST", "body must be {\"check\": ..., \"file\": ...}")
		return
	}
	if req.Check != "config_template_drift" {
		respondError(w, http.StatusBadRequest, "NOT_ACKNOWLEDGEABLE", "only config_template_drift findings can be acknowledged")
		return
	}
	rel := filepath.ToSlash(filepath.Clean(req.File))
	if req.File == "" || !filepath.IsLocal(rel) || configdrift.IsBaselineArtifact(rel) {
		respondError(w, http.StatusBadRequest, "BAD_FILE", "file must be a path relative to the configs directory, e.g. workflows/dev-pipeline.md")
		return
	}
	if h.configDir == "" {
		respondError(w, http.StatusServiceUnavailable, "NO_CONFIG_DIR", "no config directory is configured")
		return
	}
	// The daemon's currency test (stamp vs this binary) runs INSIDE
	// Acknowledge's lock, on the comparison that drives the write.
	actor := ackActor(r.Context())
	res, err := configdrift.Acknowledge(h.configDir, rel, time.Now(), h.baselineCurrency, actor)
	switch {
	case errors.Is(err, configdrift.ErrNothingToAck):
		respondError(w, http.StatusConflict, "NOTHING_TO_ACKNOWLEDGE", "config_template_drift has no finding for "+rel)
		return
	case errors.Is(err, configdrift.ErrNoBaseline):
		respondError(w, http.StatusConflict, "NO_BASELINE", "no template baseline under "+h.configDir+
			"/.templates, so there is nothing to acknowledge against — run the manifest-driven installer (make install-config-assets) first")
		return
	case errors.Is(err, configdrift.ErrBaselineNotCurrent):
		respondError(w, http.StatusConflict, "BASELINE_NOT_CURRENT", strings.TrimPrefix(err.Error(), "configdrift: ")+
			" — re-run the installer (make install-config-assets) before acknowledging")
		return
	case err != nil:
		respondError(w, http.StatusInternalServerError, "ACK_FAILED", err.Error())
		return
	}
	out := ackResponse{File: rel, Date: res.Date, Actor: configdrift.SanitizeActor(actor), Recorded: []ackRecordJSON{}, Pruned: []ackRecordJSON{}}
	for _, rec := range res.Recorded {
		out.Recorded = append(out.Recorded, ackRecordJSON{Class: string(rec.Class), Key: rec.Key, Regime: rec.Regime})
	}
	for _, rec := range res.Pruned {
		out.Pruned = append(out.Pruned, ackRecordJSON{Rel: rec.Rel, Class: string(rec.Class), Key: rec.Key, Regime: rec.Regime})
	}
	respondJSON(w, http.StatusOK, out)
}

// ackActor is who an acknowledgement records (drift design slice F). Every
// branch returns a non-empty value: "" is reserved for a record written before
// slice F. Never a credential. Order:
//  1. auth disabled: "auth-disabled" (nobody was authenticated, and the record
//     says so rather than guess a name);
//  2. a key was PRESENTED: the admin audit's principal form (api_key_id:<id>
//     or api_key_sha256:<16 hex>), or "api-key-unresolved" should that helper
//     return nothing; never the session branch, because for a static key
//     auth.Identity.Subject IS the key;
//  3. no key and a browser-session admin: "session:<sub>", or
//     "session-without-subject";
//  4. otherwise "unidentified-admin" (the gate fails closed under auth, so this
//     is unreachable; it exists so weakening the gate cannot write "").
//
// Real actors carry a "kind:" prefix; sentinels carry no colon.
func ackActor(ctx context.Context) string {
	if !IsAuthEnabledFromContext(ctx) {
		return "auth-disabled"
	}
	keyID, _ := ctx.Value(apiKeyIDKey).(string)
	if keyID != "" || APIKeyFromContext(ctx) != "" {
		if p := apiKeyPrincipalFromContext(ctx); p != "" {
			return p
		}
		return "api-key-unresolved"
	}
	if SessionRoleFromContext(ctx) == "admin" {
		if id := IdentityFromContext(ctx); id != nil && id.Subject != "" {
			return "session:" + id.Subject
		}
		return "session-without-subject"
	}
	return "unidentified-admin"
}
