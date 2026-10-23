package api

import (
	"context"
	"encoding/json"
	"errors"

	"vornik.io/vornik/internal/auditredact"
	"vornik.io/vornik/internal/auth"
	"vornik.io/vornik/internal/persistence"
)

// ClientSuppliedAuthorKey is the task_messages metadata key holding the
// author a CLIENT named (REST authorId, UI form author). It is a display
// label, never the author of record (task-lifecycle design §17, GitHub #432).
const ClientSuppliedAuthorKey = "clientSuppliedAuthor"

// ErrClientAuthorMetadata is returned by WithClientAuthorLabel when the
// metadata the label must join is not a JSON object.
var ErrClientAuthorMetadata = errors.New("metadata must be a JSON object when an author label is given")

// RequestPrincipal names who the server authenticated for this request,
// never with a bearer secret: "session:<users.id>" for a browser session,
// else "api_key_id:<id>" for a DB key, else "api_key_sha256:<16 hex>" for a
// static key, else "" (auth off and no credential presented). It never
// returns Identity.Subject, which for a static key IS the key.
// Design: task-lifecycle-conversational-design.md §17 (GitHub #432).
func RequestPrincipal(ctx context.Context) string {
	if id := IdentityFromContext(ctx); id != nil && id.Extra != nil {
		if uid, _ := id.Extra[auth.ExtraSessionUserID].(string); uid != "" {
			return "session:" + uid
		}
	}
	return apiKeyPrincipalFromContext(ctx)
}

// IsAgentKey reports whether the request authenticated with a credential the
// executor mints for an agent container: a DB key named with the per-task
// prefix ("agent:task_<id>") or the warm-pool prefix
// ("agent:warm_<project>:<role>"). Such a key is an agent, never the
// operator, so it may not write operator-authored task messages or operator
// status flips (task-lifecycle design §17.6, GitHub #463). Fail-closed on the
// name alone (any created_by, any client_kind): the stricter match
// runningAgentTaskKey uses is for GRANTING drain access, not for refusing.
func IsAgentKey(ctx context.Context) bool {
	id := IdentityFromContext(ctx)
	if id == nil || id.Extra == nil {
		return false
	}
	row, _ := id.Extra[auth.ExtraDBKeyRow].(*persistence.APIKey)
	return isAgentKeyRow(row)
}

// TaskMessageAuthor resolves a task-conversation write: the author of record
// is the request's principal; the client's value is only a bounded label
// (empty when the client named nobody).
func TaskMessageAuthor(ctx context.Context, clientValue string) (principal, label string) {
	return RequestPrincipal(ctx), auditredact.BoundIdentity(clientValue)
}

// WithClientAuthorLabel merges label into metadata under
// ClientSuppliedAuthorKey, overwriting any value the client planted there.
// An empty label returns metadata unchanged. Metadata that is present but not
// a JSON object cannot hold the label: ErrClientAuthorMetadata.
func WithClientAuthorLabel(metadata []byte, label string) ([]byte, error) {
	if label == "" {
		return metadata, nil
	}
	m := map[string]json.RawMessage{}
	if len(metadata) > 0 && string(metadata) != "null" {
		if err := json.Unmarshal(metadata, &m); err != nil || m == nil {
			return nil, ErrClientAuthorMetadata
		}
	}
	v, err := json.Marshal(label)
	if err != nil {
		return nil, err
	}
	m[ClientSuppliedAuthorKey] = v
	return json.Marshal(m)
}
