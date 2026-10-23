package api

import (
	"context"

	"vornik.io/vornik/internal/actor"
	"vornik.io/vornik/internal/auth"
	"vornik.io/vornik/internal/config"
)

// TaskPrincipal is the principal a request-created task records in
// created_by_actor (GitHub #482, #499; design 22, "Amendment 2026-10-21
// (#482)", part 8a). The memory verification predicate trusts a USER task
// only with an attributable operator, so the stamp is decided here, once, for
// REST, the UI task form and the UI chat:
//
//  1. A DB-backed key decides first, whatever else the request carries
//     (round-3 review A1): zero, because the task creator derives
//     `api_key:<id>` from the key id, and the predicate reads that key's row.
//  2. A static key next: `system:operator_key:<id>` when it is admin-class
//     AND listed in admin.operator_key_ids (isOperatorKey), else
//     `system:static_key:<id>`, attributable and never trusted. The broker's
//     and the A2A adapter's static keys land here.
//  3. A session user only when no key was presented: `user:<id>`.
//
// The id of a static key is the first 16 hex digits of its SHA-256; the key
// itself never appears. Zero when nothing is known.
func TaskPrincipal(ctx context.Context, isOperatorKey func(key string) bool) actor.Actor {
	if APIKeyIDFromContext(ctx) != "" {
		return actor.Actor{}
	}
	if key := APIKeyFromContext(ctx); key != "" {
		id := config.KeyHashID(key) // the one id function; admin.operator_key_ids lists these
		if isOperatorKey != nil && isOperatorKey(key) {
			return actor.System("operator_key:" + id)
		}
		return actor.System("static_key:" + id)
	}
	if id := IdentityFromContext(ctx); id != nil && id.Extra != nil {
		if uid, _ := id.Extra[auth.ExtraSessionUserID].(string); uid != "" {
			return actor.User(uid)
		}
	}
	return actor.Actor{}
}

// isOperatorStaticKey reports whether a presented static key is the
// operator's: admin-class, and its hash id listed in admin.operator_key_ids.
// It reads the one admin key list REST and the UI share (adminClassKeys;
// the UI's operatorCapability is built from the same config value, pinned
// by TestOperatorKeyPredicateIsOneValue), so both stamp the same keys
// (review 7262).
func (s *Server) isOperatorStaticKey(key string) bool {
	return s.adminClassKeys.IsOperatorKey(key)
}

// taskCreatorActor is TaskPrincipal with this server's operator keys.
func (s *Server) taskCreatorActor(ctx context.Context) actor.Actor {
	return TaskPrincipal(ctx, s.isOperatorStaticKey)
}
