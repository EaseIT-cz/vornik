package agentadmin

import (
	"encoding/json"
	"strings"

	"vornik.io/vornik/internal/agentns"
)

// Refusal rules (design §23.2, GitHub #172): the closed vocabulary a refused
// agent-admin call is recorded under. The row and the metric carry the rule,
// never the reason text, which quotes the agent's input.
const (
	// Renderer rules: refuse() classifies its own reason (refusalRule).
	RuleInvalidInput   = "invalid_input"
	RuleOutOfNamespace = "out_of_namespace"
	RuleURLNotHTTPS    = "url_not_https"
	RuleNotFound       = "not_found"
	RuleAlreadyExists  = "already_exists"
	RuleLimitExceeded  = "limit_exceeded"
	RuleEgressSchema   = "egress_schema"
	RuleNotApproved    = "not_approved"
	RulePendingChange  = "pending_change"
	RuleOther          = "other"
	// Service rules: set at the line in doVerb that decides.
	RuleVerbNotCallable   = "verb_not_callable"
	RuleTransportMismatch = "transport_mismatch"
	RuleSpendingBlocked   = "spending_blocked"
	RuleTooManyFiles      = "too_many_files"
	RuleApplyRefused      = "apply_refused"
	// RuleRecordFailed marks a change refused because the daemon could not
	// record it after it applied (design §23.4: it is rolled back). It is a
	// daemon-side failure, not the agent's refusal: it is NOT in
	// RefusalRules, the ledger writes no row for it and the refusal metric
	// and the agent_refusal_burst check never count it (#334 audit
	// 2026-10-14). The daemon logs it instead.
	RuleRecordFailed = "record_failed"
	// API rules: set on the companion tools/call path.
	RuleToolNotOffered       = "tool_not_offered"
	RuleInvalidArguments     = "invalid_arguments"
	RuleDelegateNotDelegable = "delegate_not_delegable"
	RuleDelegateRefused      = "delegate_refused"
)

// RefusalRules is every rule, in the order the operator page lists them.
var RefusalRules = []string{
	RuleInvalidInput, RuleOutOfNamespace, RuleURLNotHTTPS, RuleNotFound, RuleAlreadyExists,
	RuleLimitExceeded, RuleEgressSchema, RuleNotApproved, RulePendingChange, RuleOther,
	RuleVerbNotCallable, RuleTransportMismatch, RuleSpendingBlocked, RuleTooManyFiles, RuleApplyRefused,
	RuleToolNotOffered, RuleInvalidArguments, RuleDelegateNotDelegable, RuleDelegateRefused,
}

// refusalPatterns classifies a renderer refusal by its reason text, first
// match wins. A classification, not a guarantee: the reason quotes the
// agent's input, whose words can match an earlier pattern. A reason no
// pattern matches is RuleOther, which is published like every rule, so the
// total over all rules is always every refusal (design §23.2).
var refusalPatterns = []struct{ text, rule string }{
	{"waiting for approval", RulePendingChange},
	{"is not a project of", RuleOutOfNamespace},
	{"is not https", RuleURLNotHTTPS},
	{"is not a plain URL", RuleURLNotHTTPS},
	{"egress", RuleEgressSchema},
	{"is not an approved", RuleNotApproved},
	{"not a proposable write", RuleNotApproved},
	{"is not waiting for its tools", RuleNotApproved},
	{"already exists", RuleAlreadyExists},
	{"already has", RuleAlreadyExists},
	{"there is no ", RuleNotFound},
	{" has no ", RuleNotFound},
	{"does not exist", RuleNotFound},
	{"is not loaded yet", RuleNotFound},
	{"does not offer", RuleNotFound},
	{"too long", RuleLimitExceeded},
	{" is over ", RuleLimitExceeded},
	{"at most ", RuleLimitExceeded},
	{"above $", RuleLimitExceeded},
	{"the limit is", RuleLimitExceeded},
	{"must be", RuleInvalidInput},
	{"is required", RuleInvalidInput},
	{"does not match the verb's fields", RuleInvalidInput},
	{"trailing data", RuleInvalidInput},
	{"control character", RuleInvalidInput},
	{"may not contain", RuleInvalidInput},
	{"is not a tool name", RuleInvalidInput},
	{"is not a scope", RuleInvalidInput},
	{"supply ", RuleInvalidInput},
	{"does not compile", RuleInvalidInput},
	{"does not parse", RuleInvalidInput},
	{"unusable name", RuleInvalidInput},
	{"listed no tools", RuleInvalidInput},
}

// refusalRule classifies a renderer refusal's reason.
func refusalRule(reason string) string {
	for _, p := range refusalPatterns {
		if strings.Contains(reason, p.text) {
			return p.rule
		}
	}
	return RuleOther
}

// namesForeignNamespace reports whether a verb's typed input names another
// namespace's ID where a project is expected: "project", "slug", or "id" of
// a project removal. The renderer answers such an ID as "there is no
// project" on purpose (§5); the recorded rule says what happened. Never "id"
// of a workflow removal: its "<project>--<workflow>" form parses as one.
func namesForeignNamespace(ns string, input json.RawMessage) bool {
	var in map[string]json.RawMessage
	if json.Unmarshal(input, &in) != nil {
		return false
	}
	str := func(k string) string {
		var s string
		_ = json.Unmarshal(in[k], &s)
		return s
	}
	fields := []string{str("project"), str("slug")}
	if str("kind") == "project" {
		fields = append(fields, str("id"))
	}
	for _, v := range fields {
		if other, ok := agentns.FromID(v); ok && other != ns {
			return true
		}
	}
	return false
}
