package api

import (
	"fmt"
	"sort"
	"strings"
	"vornik.io/vornik/internal/retention"
)

// checkRetentionEnabled warns when the retention sweeper is off.
//
// WHY THIS IS A CHECK AND NOT A DEFAULT CHANGE. The sweeper ships disabled on
// purpose: flipping it on during an upgrade would delete data an operator never
// agreed to lose, and silent data loss is worse than a warned-about gap. The
// consequence is that a stock deployment keeps personal data INDEFINITELY, which
// does not satisfy GDPR Art 5(1)(e) — so the default is safe for the data and
// wrong for the compliance posture, and only the operator can resolve that.
//
// The warning is therefore deliberately loud and not dismissible. It names the
// article, says what the consequence is, and points at the profile to copy. A
// dismissible warning would be discovered during a supervisory-authority
// enquiry rather than before one.
//
// It also reports a HALF-CONFIGURED sweeper — enabled with every window unset —
// because that state looks configured in a config diff and prunes nothing.
//
// see LLD § https://docs.vornik.io §4.9
func (h *DoctorHandlers) checkRetentionEnabled() DoctorCheck {
	name := "retention_enabled"

	if !h.retentionKnown {
		return DoctorCheck{Name: name, Status: "SKIPPED",
			Message: "retention configuration not wired into the doctor"}
	}

	if !h.retentionEnabled {
		return DoctorCheck{
			Name:   name,
			Status: "WARNING",
			Message: "retention.enabled=false — this deployment keeps personal data INDEFINITELY, " +
				"which does not satisfy GDPR Art 5(1)(e) (storage limitation). The default is off " +
				"deliberately so an upgrade never deletes data you did not agree to lose, so only you " +
				"can close this. Start from the profile shipped alongside your config " +
				"(<config-dir>/configs/retention-recommended.yaml): copy the block into config.yaml, " +
				"set the windows to YOUR purposes, and set enabled: true.",
		}
	}

	// Enabled with no window set: the sweeper runs on the compiled defaults
	// (retention.Resolve), which were not chosen for this deployment's
	// purposes, and two classes have no default at all. GitHub #896: this
	// said "prunes nothing", which was false.
	if len(h.retentionWindows) == 0 {
		d := retention.Resolve("", retention.Policy{}, retention.Policy{})
		return DoctorCheck{
			Name:   name,
			Status: "WARNING",
			Message: fmt.Sprintf("retention.enabled=true with no window set, so the sweeper prunes on the compiled "+
				"defaults: tasks %dd, executions %dd, artifacts %dd, LLM usage %dd, tool audit %dd, chat audit %dd. "+
				"Memory chunks and task messages have no default: memory chunks are kept until their class TTL "+
				"and task messages until their task is pruned. Under Art 5(1)(e) the windows must match your "+
				"purposes; set them per data class, starting from <config-dir>/configs/retention-recommended.yaml.",
				d.TasksDays, d.ExecutionsDays, d.ArtifactsDays, d.TaskLLMUsageDays, d.ToolAuditDays, d.ChatAuditDays),
		}
	}

	set := make([]string, 0, len(h.retentionWindows))
	for k, days := range h.retentionWindows {
		set = append(set, fmt.Sprintf("%s=%dd", k, days))
	}
	sort.Strings(set)

	msg := fmt.Sprintf("sweeper on, %d window(s) configured (%s)",
		len(set), strings.Join(set, ", "))

	// Memory chunks are the row most operators under-think: an embedding of a
	// sentence about a person is personal data about that person, and this store
	// is designed to outlive its sources. An otherwise-configured sweeper that
	// omits it leaves the longest-lived personal data unbounded.
	if _, ok := h.retentionWindows["memory_chunks_days"]; !ok {
		return DoctorCheck{Name: name, Status: "WARNING",
			Message: msg + " — but memory_chunks_days is UNSET, so RAG memory derived from mail, " +
				"documents and chat is kept indefinitely. That is the longest-lived personal data in " +
				"the system and the one most often missed."}
	}

	// The embedding cache holds vectors of recall QUERIES under the hash of
	// their own text — often a person's name — and no erasure path can find
	// them, so a time window is the only bound on that data (Art 17 redact
	// design §4.4a; GDPR design §4.20). Only where the cache is written:
	// memory.embedding_cache_enabled on, and not SQLite (no embedding_cache).
	if _, ok := h.retentionWindows["embedding_cache_days"]; !ok && h.embeddingCacheApplies {
		return DoctorCheck{Name: name, Status: "WARNING",
			Message: msg + " — but embedding_cache_days is UNSET, so cached embeddings of recall " +
				"queries (which often name a person) are kept indefinitely. No erasure can reach " +
				"them, so a window is the only storage limit on them; see " +
				"<config-dir>/configs/retention-recommended.yaml."}
	}

	return DoctorCheck{Name: name, Status: "OK", Message: msg}
}
