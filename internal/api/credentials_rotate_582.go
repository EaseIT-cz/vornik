package api

// Rotation of the operations credentials (GitHub #582 P4,
// operations-api-contract-design.md §6.2 P4 and §6.3 F3, F5). Each credential
// has two slots, current and previous; the new value is generated here, written
// to config.yaml, and answered sealed to the caller's X-Wing key, never in
// clear. It takes effect at the restart the control plane performs. Only the
// current instance_admin key operates the credentials, so a stolen previous key
// cannot rotate, retire or lock out the current one.

import (
	"crypto/hpke"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"

	"vornik.io/vornik/internal/apikey"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/configlock"
	"vornik.io/vornik/internal/featuredoctor"
	"vornik.io/vornik/internal/secretgen"
)

// credentialRotateInfoPrefix domain-separates a rotation's sealed answer from
// provision's ("vornik-provision/1/"): the HPKE info is this prefix, the
// instance id, "/", and the credential.
const credentialRotateInfoPrefix = "vornik-rotate/1/"

const (
	credInstanceAdmin = "instance_admin"
	credScrapeToken   = "scrape_token"
	modeRotate        = "rotate"
	modeRetire        = "retire"
	modeReplace       = "replace"
)

type rotateRequest struct {
	Credential string `json:"credential"`
	Mode       string `json:"mode"`
	Recipient  string `json:"recipient"`
}

// RotateOperationsCredential serves POST /api/v1/operations/credentials/rotate.
func (s *Server) RotateOperationsCredential(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "use POST")
		return
	}
	if s.config == nil || !s.config.Hosting.Managed() {
		respondError(w, http.StatusConflict, "NOT_MANAGED", "credential rotation is an operation of managed hosting")
		return
	}
	if s.setupConfigPath == "" {
		respondError(w, http.StatusServiceUnavailable, "CONFIG_PATH_UNSET", "the config file path is not wired")
		return
	}
	// Managed mode requires auth on (config validation), so the predicate
	// below never passes for want of a key; the presented key is still read
	// and compared, because the slot it fills decides what it may do.
	presented := extractAPIKey(r)
	if presented == "" || !s.isAdminClassRequest(r) || !s.isAdminClassKey(presented) {
		respondError(w, http.StatusForbidden, "ADMIN_SCOPE_REQUIRED", "rotation needs the current instance_admin key")
		return
	}
	limitJSONBody(w, r)
	req, recipient, code, msg := parseRotateRequest(r)
	if code != "" {
		respondError(w, http.StatusBadRequest, code, msg)
		return
	}

	// The process's one config-write lock (§6.3 F4; GitHub #584, §6.6).
	unlock, lerr := configlock.LockAsWriter(r.Context()) // refused on a non-writer daemon (#681)
	if lerr != nil {
		respondNotConfigWriter(w, lerr)
		return
	}
	defer unlock()
	writer := &featuredoctor.FileConfigWriter{Path: s.setupConfigPath}
	content, err := writer.Read()
	if err != nil {
		respondError(w, http.StatusInternalServerError, "CONFIG_READ", "the config file could not be read")
		return
	}
	// The slots are read from the file, not the boot-time config: after a
	// rotation, a retire or a replace, and before the restart, the boot-time
	// key set still accepts keys the file no longer holds as current.
	if !isCurrentAdminKeyInFile(content, presented) {
		respondError(w, http.StatusForbidden, "NOT_CURRENT_KEY", "only the current instance_admin key, listed in this instance's config file and not the previous one, may operate the credentials")
		return
	}
	prevAdmin := config.GetYAMLString(content, "hosting.previous_admin_api_key")

	var newValue string
	if req.Mode != modeRetire {
		if req.Credential == credInstanceAdmin {
			newValue, err = apikey.Generate("instance_admin")
		} else {
			newValue, err = secretgen.Password(32)
		}
		if err != nil {
			respondError(w, http.StatusInternalServerError, "GENERATE", "the new value could not be generated")
			return
		}
	}

	var out []byte
	if req.Credential == credInstanceAdmin {
		out, code, msg = rotateAdminKey(content, req.Mode, presented, prevAdmin, newValue)
	} else {
		out, code, msg = rotateScrapeToken(content, req.Mode, newValue)
	}
	if code != "" {
		respondError(w, http.StatusConflict, code, msg)
		return
	}
	backup, ok := commitConfig(w, writer, out)
	if !ok {
		return
	}

	resp := map[string]any{"credential": req.Credential, "mode": req.Mode, "restart_required": true}
	if newValue != "" {
		ct, err := hpke.Seal(recipient, hpke.HKDFSHA256(), hpke.AES256GCM(),
			[]byte(credentialRotateInfoPrefix+s.config.Hosting.InstanceID+"/"+req.Credential), []byte(newValue))
		if err != nil {
			// The new value is written and cannot be delivered: restore, so
			// the caller is not left with a credential it cannot read.
			_ = writer.Restore(backup)
			respondError(w, http.StatusInternalServerError, "SEAL", "the new value could not be sealed; the config was restored and nothing changed")
			return
		}
		resp["sealed"] = base64.StdEncoding.EncodeToString(ct)
	}
	s.logger.Info().Str("credential", req.Credential).Str("mode", req.Mode).Msg("operations credential changed; takes effect at the restart")
	respondJSON(w, http.StatusOK, resp)
}

// isCurrentAdminKeyInFile reports whether key is the instance's current
// instance_admin key by the config file as written: listed literally in both
// api.api_keys and admin.allowed_keys, and not hosting.previous_admin_api_key
// (commit security review 2026-10-29, findings 1 to 3). A key held only as an
// ${ENV} reference does not qualify: a literal edit could not remove it, so
// a retirement would not happen.
func isCurrentAdminKeyInFile(content []byte, key string) bool {
	if key == "" {
		return false
	}
	// Every key is compared, with no early return, so the time does not say
	// which listed key matched.
	found := 0
	for _, k := range config.CurrentAdminKeysInFile(content) {
		if apikey.SecretEqual(key, k) { // length-safe (GitHub #445)
			found = 1
		}
	}
	return found == 1
}

// parseRotateRequest decodes and checks the body; code is "" when it is good.
// The recipient is parsed before anything is generated (§6.3 F5).
func parseRotateRequest(r *http.Request) (rotateRequest, hpke.PublicKey, string, string) {
	var req rotateRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return req, nil, "BAD_BODY", "the body must be {credential, mode, recipient}"
	}
	if req.Credential != credInstanceAdmin && req.Credential != credScrapeToken {
		return req, nil, "BAD_CREDENTIAL", "credential is instance_admin or scrape_token"
	}
	if req.Mode != modeRotate && req.Mode != modeRetire && req.Mode != modeReplace {
		return req, nil, "BAD_MODE", "mode is rotate, retire or replace"
	}
	if req.Mode == modeRetire {
		return req, nil, "", ""
	}
	raw, err := base64.StdEncoding.DecodeString(req.Recipient)
	var recipient hpke.PublicKey
	if err == nil {
		recipient, err = hpke.MLKEM768X25519().NewPublicKey(raw)
	}
	if err != nil || req.Recipient == "" {
		return req, nil, "BAD_RECIPIENT", "recipient is the caller's base64 X-Wing (ML-KEM-768 + X25519) public key"
	}
	return req, recipient, "", ""
}

// commitConfig backs the config up, writes out and validates it, restoring
// the backup on any failure (§6.3 F8); it answers the failure itself. It
// returns the backup so a later step can still restore.
func commitConfig(w http.ResponseWriter, writer *featuredoctor.FileConfigWriter, out []byte) (string, bool) {
	backup, err := writer.Backup()
	if err != nil {
		respondError(w, http.StatusInternalServerError, "CONFIG_BACKUP", "the config backup failed; nothing changed")
		return "", false
	}
	if err := writer.Write(out); err != nil {
		_ = writer.Restore(backup)
		respondError(w, http.StatusInternalServerError, "CONFIG_WRITE", "the config write failed and was restored; nothing changed")
		return "", false
	}
	if err := writer.Validate(); err != nil {
		_ = writer.Restore(backup)
		respondError(w, http.StatusInternalServerError, "CONFIG_INVALID", "the written config did not validate and was restored; nothing changed")
		return "", false
	}
	return backup, true
}

// rotateAdminKey computes the config for an instance_admin operation. The
// current key is the one the request authenticated with.
func rotateAdminKey(content []byte, mode, current, prev, newKey string) ([]byte, string, string) {
	apiKeys := config.GetYAMLStringList(content, "api.api_keys")
	allowed := config.GetYAMLStringList(content, "admin.allowed_keys")
	without := func(list []string, k string) []string {
		return slices.DeleteFunc(slices.Clone(list), func(v string) bool { return v == k })
	}
	switch mode {
	case modeRotate:
		if prev != "" {
			return nil, "RETIRE_FIRST", "a previous instance_admin key is still listed; retire it before rotating again"
		}
		apiKeys, allowed = append(apiKeys, newKey), append(allowed, newKey)
		prev = current
	case modeReplace:
		if prev != "" {
			return nil, "RETIRE_FIRST", "a previous instance_admin key is still listed; retire it before replacing"
		}
		apiKeys, allowed = append(without(apiKeys, current), newKey), append(without(allowed, current), newKey)
	case modeRetire:
		if prev == "" {
			return nil, "NOTHING_TO_RETIRE", "no previous instance_admin key is listed"
		}
		if !slices.Contains(apiKeys, prev) || !slices.Contains(allowed, prev) {
			return nil, "PREVIOUS_NOT_LITERAL", "the previous key is not listed literally in both key lists, so it cannot be removed by this operation"
		}
		apiKeys, allowed = without(apiKeys, prev), without(allowed, prev)
		prev = ""
	}
	out, err := setYAML(content,
		yamlSet{"api.api_keys", apiKeys},
		yamlSet{"admin.allowed_keys", allowed},
		yamlSet{"hosting.previous_admin_api_key", prev})
	if err != nil {
		return nil, "CONFIG_PATCH", err.Error()
	}
	return out, "", ""
}

// rotateScrapeToken computes the config for a scrape token operation.
func rotateScrapeToken(content []byte, mode, newTok string) ([]byte, string, string) {
	current := config.GetYAMLString(content, "metrics.scrape_token")
	prev := config.GetYAMLString(content, "metrics.previous_scrape_token")
	switch mode {
	case modeRotate:
		if prev != "" {
			return nil, "RETIRE_FIRST", "a previous scrape token is still accepted; retire it before rotating again"
		}
		prev, current = current, newTok
	case modeReplace:
		if prev != "" {
			return nil, "RETIRE_FIRST", "a previous scrape token is still accepted; retire it before replacing"
		}
		current = newTok
	case modeRetire:
		if prev == "" {
			return nil, "NOTHING_TO_RETIRE", "no previous scrape token is accepted"
		}
		prev = ""
	}
	out, err := setYAML(content,
		yamlSet{"metrics.scrape_token", current},
		yamlSet{"metrics.previous_scrape_token", prev})
	if err != nil {
		return nil, "CONFIG_PATCH", err.Error()
	}
	return out, "", ""
}

type yamlSet struct {
	key string
	val any
}

func setYAML(content []byte, sets ...yamlSet) ([]byte, error) {
	for _, s := range sets {
		var err error
		content, _, err = config.SetYAMLKey(content, s.key, s.val)
		if err != nil {
			return nil, fmt.Errorf("set %s: %w", s.key, err)
		}
	}
	return content, nil
}
