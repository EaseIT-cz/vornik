package api

import (
	"net/http"

	"vornik.io/vornik/internal/auth"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/untrusted"
)

// memorySearchCallerIsModel reports whether the memory-search response goes
// to a model (the agent container's memory_search tool) rather than to an
// operator (UI, vornikctl). Two triggers (untrusted-content wrapper design
// §2.5): a task-scoped key, which every agent container holds, whatever the
// query says (an agent image older than the daemon sends no flag); or
// wrap=untrusted, which handle_memory_search sends (a deployment without
// authentication has no key to identify the caller).
func memorySearchCallerIsModel(r *http.Request) bool {
	// Any wrap=untrusted value counts, not only the first: a second wrap=
	// smuggled in through the model-supplied limit cannot switch it off.
	for _, v := range r.URL.Query()["wrap"] {
		if v == "untrusted" {
			return true
		}
	}
	id := IdentityFromContext(r.Context())
	if id == nil {
		return false
	}
	row, ok := id.Extra[auth.ExtraDBKeyRow].(*persistence.APIKey)
	if !ok || row == nil {
		return false
	}
	_, isTaskKey := persistence.TaskIDFromKeyName(row.Name)
	return isTaskKey
}

// wrapMemoryResultsForModel wraps, in place, every chunk field a writer
// controls (GitHub #454, #456): content, source_name (tags included),
// content_class and repo_scope. A companion remember call sets all four
// free-form. Daemon-generated fields (ids, score, trust and timestamps)
// stay raw so they remain machine-readable.
func wrapMemoryResultsForModel(results []MemorySearchResult) {
	for i := range results {
		r := &results[i]
		r.Content = untrusted.WrapLabeled("memory_hit", r.Content)
		r.SourceName = untrusted.WrapLabeled("memory_source_name", r.SourceName)
		if r.ContentClass != "" {
			r.ContentClass = untrusted.WrapLabeled("memory_content_class", r.ContentClass)
		}
		if r.RepoScope != "" {
			r.RepoScope = untrusted.WrapLabeled("memory_repo_scope", r.RepoScope)
		}
	}
}
