package api

// GitHub #622 follow-up (found at: production, 2026-11-02, the live check of
// fd586c008): `vornikctl account grant-key` with an A2A caller key was
// correctly refused by authz.AssignKey (ErrKeyNotALogin), but the operator saw
// "500 ACCOUNT_ERROR: account operation failed", because
// respondAccountErrorStatus had no case for the new sentinel. Regression test
// at: unit (the mapping function both operator account doors share). Lower
// because: the 500 comes from this one switch, which every account handler
// calls; nothing between it and the client changes the status.
// Design: oidc-identity-permissions-design.md, Amendment 2026-11-02 (#622).

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"vornik.io/vornik/internal/authz"
)

func TestRespondAccountError622_KeyNotALoginIsAConflictWithItsReason(t *testing.T) {
	rec := httptest.NewRecorder()
	respondAccountErrorStatus(rec, fmt.Errorf("assign: %w", authz.ErrKeyNotALogin))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", rec.Code, rec.Body.String())
	}
	var env struct {
		Error struct{ Code, Message string } `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	if env.Error.Code != "KEY_NOT_A_LOGIN" {
		t.Errorf("code = %q, want KEY_NOT_A_LOGIN", env.Error.Code)
	}
	if env.Error.Message == "" || env.Error.Message == "account operation failed" {
		t.Errorf("message %q does not say why", env.Error.Message)
	}
}

// Every sentinel the key-mapping functions return is a curated answer, never
// the unrecognised-error 500. A new sentinel in internal/authz/claimkey.go goes
// here and into respondAccountErrorStatus together.
func TestRespondAccountError622_EveryKeyMappingSentinelIsCurated(t *testing.T) {
	for _, err := range []error{
		authz.ErrKeyClaimRefused, authz.ErrKeyClaimRateLimited,
		authz.ErrKeysUnavailable, authz.ErrKeyNotALogin,
	} {
		rec := httptest.NewRecorder()
		respondAccountErrorStatus(rec, err)
		if rec.Code == http.StatusInternalServerError {
			t.Errorf("%v maps to 500: %s", err, rec.Body.String())
		}
	}
}
