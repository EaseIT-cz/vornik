package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"vornik.io/vornik/internal/auth"
	"vornik.io/vornik/internal/memory"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/taskkeys"
)

// MemoryStats handles GET /api/v1/memory/stats. Unscoped (returns all
// projects) so operators get one shot at "what's the state of RAG
// across everything"; a --project filter is applied client-side in the
// vornikctl CLI because the underlying repo query already returns all
// rows cheaply.
func (s *Server) MemoryStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "only GET is supported")
		return
	}
	if s.memoryStats == nil {
		respondError(w, http.StatusServiceUnavailable, "MEMORY_DISABLED", "memory stats is not enabled")
		return
	}
	rows, err := s.memoryStats.Stats(r.Context())
	if err != nil {
		s.logger.Warn().Err(err).Msg("memory stats failed")
		respondError(w, http.StatusInternalServerError, "STATS_ERROR", "memory stats failed")
		return
	}
	if rows == nil {
		rows = []MemoryProjectStats{}
	}
	if allowed, scoped := requestScopedProjectSet(r); scoped {
		filtered := rows[:0]
		for _, row := range rows {
			if allowed[row.ProjectID] {
				filtered = append(filtered, row)
			}
		}
		rows = filtered
	}
	out := map[string]any{
		"projects": rows,
		"total":    len(rows),
	}
	// The RESOLVED embedder, when the deployment can report one. Absent rather
	// than empty-valued when it cannot, so a caller can tell "no embedder wired"
	// from "an embedder whose identity we failed to read".
	if s.memoryEmbedder != nil {
		if provider, model, dims := s.memoryEmbedder.Embedder(); model != "" {
			emb := map[string]any{"provider": provider, "model": model}
			if dims > 0 {
				emb["dimensions"] = dims
			}
			out["embedder"] = emb
		}
	}
	respondJSON(w, http.StatusOK, out)
}

// MemoryCacheStats handles GET /api/v1/memory/cache-stats. Returns
// both embedding cache (Phase D) and response cache (Phase E)
// summaries so the CLI can render them side-by-side. Each block's
// Enabled flag distinguishes "feature off" from "no rows yet" —
// the CLI uses Enabled to pick "disabled" vs "0 rows" wording.
func (s *Server) MemoryCacheStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "only GET is supported")
		return
	}
	if !s.requireAdminGate(w, r) {
		return
	}
	if s.memoryCacheStats == nil {
		respondError(w, http.StatusServiceUnavailable, "CACHE_STATS_DISABLED",
			"memory cache stats not wired on this daemon")
		return
	}
	embed, embedErr := s.memoryCacheStats.EmbeddingCacheStats(r.Context())
	if embedErr != nil {
		s.logger.Warn().Err(embedErr).Msg("embedding cache stats failed")
	}
	resp, respErr := s.memoryCacheStats.ResponseCacheStats(r.Context())
	if respErr != nil {
		s.logger.Warn().Err(respErr).Msg("response cache stats failed")
	}
	// Both errors → 500. One error → return the half that worked +
	// a warning header. Operators see the partial result rather than
	// "everything's broken".
	if embedErr != nil && respErr != nil {
		respondError(w, http.StatusInternalServerError, "CACHE_STATS_ERROR",
			"both cache stats queries failed")
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{
		"embedding_cache": embed,
		"response_cache":  resp,
	})
}

// MemoryBackfillTitles handles POST /api/v1/memory/backfill-titles.
// One call processes one batch (default 10, capped at 100 to keep the
// HTTP round-trip bounded — the CLI loops). Optional ?count=true is
// a cheap probe that returns only the Remaining field, used by
// --dry-run.
func (s *Server) MemoryBackfillTitles(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "only POST is supported")
		return
	}
	if !s.requireAdminGate(w, r) {
		return
	}
	if s.memoryTitleBackfiller == nil {
		respondError(w, http.StatusServiceUnavailable, "MEMORY_TITLER_DISABLED", "memory title backfill is not enabled — set memory.titler.enabled=true and restart")
		return
	}

	if r.URL.Query().Get("count") == "true" {
		remaining, err := s.memoryTitleBackfiller.CountRemaining(r.Context())
		if err != nil {
			s.logger.Warn().Err(err).Msg("memory backfill: count failed")
			respondError(w, http.StatusInternalServerError, "BACKFILL_ERROR", "count failed")
			return
		}
		respondJSON(w, http.StatusOK, MemoryTitleBackfillResult{Remaining: remaining})
		return
	}

	batchSize := 10
	if s := r.URL.Query().Get("batch_size"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			batchSize = n
		}
	}
	if batchSize > 100 {
		batchSize = 100
	}

	res, err := s.memoryTitleBackfiller.BackfillBatch(r.Context(), batchSize)
	if err != nil {
		s.logger.Warn().Err(err).Msg("memory backfill: batch failed")
		respondError(w, http.StatusInternalServerError, "BACKFILL_ERROR", err.Error())
		return
	}
	respondJSON(w, http.StatusOK, res)
}

// MemoryReclassifyLLM handles POST /api/v1/memory/reclassify-llm.
// One call processes one batch (default 10, capped at 50 to keep
// the HTTP round-trip bounded — the CLI loops). Required query
// parameter: project. Optional ?count=true is a cheap probe that
// returns only the Remaining field, used by --dry-run.
func (s *Server) MemoryReclassifyLLM(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "only POST is supported")
		return
	}
	if s.memoryClassifyBackfiller == nil {
		respondError(w, http.StatusServiceUnavailable, "MEMORY_CLASSIFIER_DISABLED", "memory LLM classifier is not enabled — set memory.classifier.enabled=true and restart")
		return
	}
	projectID := r.URL.Query().Get("project")
	if projectID == "" {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "project query parameter is required")
		return
	}
	if !requestAllowsProject(r, projectID) {
		respondError(w, http.StatusForbidden, "FORBIDDEN", "project not allowed")
		return
	}

	if r.URL.Query().Get("count") == "true" {
		remaining, err := s.memoryClassifyBackfiller.CountRemaining(r.Context(), projectID)
		if err != nil {
			s.logger.Warn().Err(err).Msg("memory reclassify-llm: count failed")
			respondError(w, http.StatusInternalServerError, "RECLASSIFY_ERROR", "count failed")
			return
		}
		respondJSON(w, http.StatusOK, MemoryClassifyBackfillResult{Remaining: remaining})
		return
	}

	batchSize := 10
	if bs := r.URL.Query().Get("batch_size"); bs != "" {
		if n, err := strconv.Atoi(bs); err == nil && n > 0 {
			batchSize = n
		}
	}
	if batchSize > 50 {
		batchSize = 50
	}

	res, err := s.memoryClassifyBackfiller.BackfillBatch(r.Context(), projectID, batchSize)
	if err != nil {
		s.logger.Warn().Err(err).Msg("memory reclassify-llm: batch failed")
		respondError(w, http.StatusInternalServerError, "RECLASSIFY_ERROR", "reclassify failed")
		return
	}
	respondJSON(w, http.StatusOK, res)
}

// MemoryRegraph handles POST /api/v1/memory/regraph?project=X[&dry_run=true].
// Re-flags every project chunk that produced zero published edges so
// the KG worker reprocesses them with current pipeline logic. Returns
// the count of chunks affected (or the count that WOULD be affected
// when dry_run=true).
//
// Use case: after a KG-pipeline fix (e.g. evidence-substring
// normalisation, 2026-05-25) the operator wants the existing isolated
// entities to actually benefit, not just future ingest. Without this
// surface the fix only helps new chunks until the operator manually
// flips flags.
//
// Project-scoped (not global) so a fix only re-runs against the
// project the operator opts in. Same admin-gate matrix as
// /reclassify-llm: requestAllowsProject is the canonical scope check.
func (s *Server) MemoryRegraph(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "only POST is supported")
		return
	}
	if s.memoryGraphReflagger == nil {
		respondError(w, http.StatusServiceUnavailable, "MEMORY_REGRAPH_DISABLED",
			"KG re-flag endpoint not wired on this deployment")
		return
	}
	projectID := r.URL.Query().Get("project")
	if projectID == "" {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "project query parameter is required")
		return
	}
	if !requestAllowsProject(r, projectID) {
		respondError(w, http.StatusForbidden, "FORBIDDEN", "project not allowed")
		return
	}
	dryRun := r.URL.Query().Get("dry_run") == "true"

	n, err := s.memoryGraphReflagger.ReflagChunksMissingEdges(r.Context(), projectID, dryRun)
	if err != nil {
		s.logger.Warn().Err(err).Str("project", projectID).Msg("memory regraph: reflag failed")
		respondError(w, http.StatusInternalServerError, "REGRAPH_ERROR", "regraph failed")
		return
	}
	respondJSON(w, http.StatusOK, MemoryGraphReflagResult{
		Project:   projectID,
		DryRun:    dryRun,
		ReFlagged: n,
	})
}

// MemoryRegraphQuarantined handles POST
// /api/v1/memory/regraph/quarantined?[project=P]&[error_contains=S]&[dry_run=true].
// Gives QUARANTINED chunks (the KG worker's failure bound reached) back to the
// worker with their attempt counter reset — never a chunk that is not
// quarantined — and reports, per project, the quarantined chunks examined,
// the ones whose error contains S, and the ones re-flagged. Knowledge-graph
// LLD §3.3b (2026-10-16): the recovery for the chunks #284 quarantined, which
// the edge-less /regraph could not target.
//
// Authorisation is regraph's: a named project passes requestAllowsProject.
// Without one the call spans every project, so a project-scoped key is
// refused, as window-mode support reports are (resolveWindowScope).
func (s *Server) MemoryRegraphQuarantined(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "only POST is supported")
		return
	}
	if s.memoryGraphReflagger == nil {
		respondError(w, http.StatusServiceUnavailable, "MEMORY_REGRAPH_DISABLED",
			"KG re-flag endpoint not wired on this deployment")
		return
	}
	q := r.URL.Query()
	projectID := q.Get("project")
	errorContains := q.Get("error_contains")
	dryRun := q.Get("dry_run") == "true"
	if projectID == "" {
		if _, scoped := requestScopedProjectSet(r); scoped {
			respondError(w, http.StatusForbidden, "GLOBAL_KEY_REQUIRED",
				"re-flagging quarantined chunks of every project needs a key that is not project-scoped; pass project")
			return
		}
	} else if !requestAllowsProject(r, projectID) {
		respondError(w, http.StatusForbidden, "FORBIDDEN", "project not allowed")
		return
	}

	rows, err := s.memoryGraphReflagger.ReflagQuarantinedChunks(r.Context(), projectID, errorContains, dryRun)
	if err != nil {
		s.logger.Warn().Err(err).Str("project", projectID).Msg("memory regraph quarantined: reflag failed")
		respondError(w, http.StatusInternalServerError, "REGRAPH_ERROR", "regraph failed")
		return
	}
	out := MemoryGraphQuarantineReflagResult{
		Project:       projectID,
		ErrorContains: errorContains,
		DryRun:        dryRun,
		Projects:      make([]MemoryGraphQuarantineReflagRow, 0, len(rows)),
	}
	for _, c := range rows {
		out.Projects = append(out.Projects, MemoryGraphQuarantineReflagRow{
			Project: c.ProjectID, Quarantined: c.Quarantined, Matched: c.Matched, ReFlagged: c.ReFlagged,
		})
		out.Totals.Quarantined += c.Quarantined
		out.Totals.Matched += c.Matched
		out.Totals.ReFlagged += c.ReFlagged
	}
	if !dryRun && out.Totals.ReFlagged > 0 {
		s.logger.Info().Str("project", projectID).Str("error_contains", errorContains).
			Int("reflagged", out.Totals.ReFlagged).Int("matched", out.Totals.Matched).
			Int("quarantined", out.Totals.Quarantined).
			Msg("memory regraph quarantined: chunks re-flagged with attempts reset")
	}
	respondJSON(w, http.StatusOK, out)
}

// memorySearchResponse is the JSON response for GET /api/v1/projects/{id}/memory/search.
// The routing fields (Verdict/Guidance/Basis) are emitted only when the
// searcher supports confidence-based retrieval routing (P3); with routing off
// they are omitted and the response is byte-identical to before the feature.
type memorySearchResponse struct {
	Results []MemorySearchResult `json:"results"`
	// RetrievalTrustVerdict + Guidance + Basis are populated only under
	// routing. omitempty keeps the non-routing response shape unchanged.
	RetrievalTrustVerdict string                   `json:"retrieval_trust_verdict,omitempty"`
	Guidance              string                   `json:"guidance,omitempty"`
	VerdictBasis          *RoutingVerdictBasisWire `json:"verdict_basis,omitempty"`
	WidenRounds           int                      `json:"widen_rounds,omitempty"`
	// RetrievalPolicy is the denominator of a filtered search (#550), absent
	// when nothing filtered it.
	RetrievalPolicy *retrievalPolicyWire `json:"retrieval_policy,omitempty"`
}

// retrievalPolicyWire says what filtered a search and what it left out:
// UnfilteredReturned is the same query's hit count with no scope or
// authority filter, measured only when the filtered search returned fewer
// than its limit. 0 of 5 says the filter excluded everything relevant; 0 of
// 0 says nothing relevant exists.
type retrievalPolicyWire struct {
	RepoScope          string   `json:"repo_scope,omitempty"`
	StrictScope        bool     `json:"strict_scope"`
	Authority          string   `json:"authority"`
	SourcePrefixes     []string `json:"source_prefixes,omitempty"`
	MaxSensitivity     string   `json:"max_sensitivity,omitempty"`
	Returned           int      `json:"returned"`
	UnfilteredReturned *int     `json:"unfiltered_returned,omitempty"`
}

// memorySearchRequestFrom reads the search's limit and filters from the
// query string. ok is false for an unknown authority.
func memorySearchRequestFrom(r *http.Request, limit int) (MemorySearchRequest, bool) {
	qs := r.URL.Query()
	req := MemorySearchRequest{Limit: limit, RepoScope: strings.TrimSpace(qs.Get("repo_scope"))}
	req.StrictScope, _ = strconv.ParseBool(qs.Get("strict_scope"))
	switch a := strings.TrimSpace(qs.Get("authority")); a {
	case "", registry.MemorySearchAuthorityAny:
	case registry.MemorySearchAuthorityProvenance:
		req.Authority = a
	default:
		return req, false
	}
	return req, true
}

// applyMemorySearchRolePolicy narrows req to the role's policy: the policy's
// scope replaces the caller's, strict scope and authority can only be turned
// on, and the limit defaults to and is capped by the policy's.
func applyMemorySearchRolePolicy(req MemorySearchRequest, pol *registry.MemorySearchPolicy, limitGiven bool) MemorySearchRequest {
	if pol == nil {
		return req
	}
	if pol.RepoScope != "" {
		req.RepoScope = pol.RepoScope
	}
	req.StrictScope = req.StrictScope || pol.StrictScope
	if pol.Authority == registry.MemorySearchAuthorityProvenance {
		req.Authority = registry.MemorySearchAuthorityProvenance
	}
	// Source prefixes come only from the role (#614 C3): the query string has
	// no parameter for them, so the role's list is the whole list.
	req.SourcePrefixes = pol.SourcePrefixes
	// The sensitivity cap, too, comes only from the role (#618).
	req.MaxSensitivity = pol.MaxSensitivity
	if pol.Limit > 0 && (!limitGiven || req.Limit > pol.Limit) {
		req.Limit = pol.Limit
	}
	return req
}

// agentKeyPrincipal reports whether the request authenticated with a key the
// executor mints for an agent container, and which agent it belongs to, read
// from the key row alone (no header takes part). Recognition is the broad,
// name-based isAgentKeyRow, the one AuthMiddleware confines agent keys with,
// so a warm-pool key is covered as well as a per-task key (security review
// 2026-10-25, finding 1: a warm role's key was not recognised and its search
// ran unfiltered). A warm-pool key's role is read only from the name suffix
// after its own project, so a key cannot claim another project's role. Unlike
// runningAgentTaskKey it does not require the task to be running: a key
// whose policy cannot be resolved is refused, never served unfiltered.
func agentKeyPrincipal(r *http.Request) (AgentKeyPrincipal, bool) {
	id := IdentityFromContext(r.Context())
	if id == nil {
		return AgentKeyPrincipal{}, false
	}
	row, ok := id.Extra[auth.ExtraDBKeyRow].(*persistence.APIKey)
	if !ok || !isAgentKeyRow(row) {
		return AgentKeyPrincipal{}, false
	}
	if taskID, isTaskKey := persistence.TaskIDFromKeyName(row.Name); isTaskKey {
		return AgentKeyPrincipal{TaskID: taskID, ProjectID: row.ProjectID}, true
	}
	who := AgentKeyPrincipal{ProjectID: row.ProjectID}
	if own := taskkeys.WarmAgentKeyNamePrefix + row.ProjectID + ":"; row.ProjectID != "" && strings.HasPrefix(row.Name, own) {
		who.Role = strings.TrimPrefix(row.Name, own)
	}
	return who, true
}

// retrievalPolicyBlock builds the denominator of a filtered search. The
// unfiltered count costs one more search, only when the filtered one came
// back short; its hits are counted and never returned.
func (s *Server) retrievalPolicyBlock(ctx context.Context, rs MemoryRoutingSearcher, projectID, q string, req MemorySearchRequest, returned int) *retrievalPolicyWire {
	block := &retrievalPolicyWire{RepoScope: req.RepoScope, StrictScope: req.StrictScope, Authority: req.Authority,
		SourcePrefixes: req.SourcePrefixes, MaxSensitivity: req.MaxSensitivity, Returned: returned}
	if block.Authority == "" {
		block.Authority = registry.MemorySearchAuthorityAny
	}
	if rs == nil || returned >= req.Limit {
		return block
	}
	all, _, err := rs.SearchRouting(ctx, projectID, q, MemorySearchRequest{Limit: req.Limit})
	if err != nil {
		s.logger.Debug().Err(err).Str("project_id", projectID).Msg("memory search: unfiltered count failed; denominator omitted")
		return block
	}
	n := len(all)
	block.UnfilteredReturned = &n
	return block
}

// MemorySearch handles GET /api/v1/projects/{projectId}/memory/search?q=<query>&limit=<n>.
func (s *Server) MemorySearch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "only GET is supported")
		return
	}

	projectID := extractProjectID(r)
	if projectID == "" {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "projectId is required")
		return
	}
	// Defense in depth: the global ProjectAuthMiddleware already guards
	// project-scoped endpoints, but this branch is separately exposed
	// and it's cheap to re-check. Forbidden API keys return the same
	// shape as other project-scoped endpoints.
	if !requestAllowsProject(r, projectID) {
		respondError(w, http.StatusForbidden, "FORBIDDEN", "project not allowed")
		return
	}

	q := r.URL.Query().Get("q")
	if q == "" {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "query parameter 'q' is required")
		return
	}

	limit := 10
	if limitStr := r.URL.Query().Get("limit"); limitStr != "" {
		if n, err := strconv.Atoi(limitStr); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > 50 {
		limit = 50
	}

	if s.memorySearcher == nil {
		respondError(w, http.StatusServiceUnavailable, "MEMORY_DISABLED", "memory search is not enabled")
		return
	}

	// #550: the search's filters, then the role policy of an executor task
	// key, which a wider parameter cannot loosen (design
	// 2026-10-25-architect-expert-design.md §2.1 C1).
	searchReq, ok := memorySearchRequestFrom(r, limit)
	if !ok {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "authority must be 'any' or 'provenance'")
		return
	}
	who, isAgent := agentKeyPrincipal(r)
	if isAgent && s.memorySearchRolePolicy != nil {
		pol, perr := s.memorySearchRolePolicy(r.Context(), who)
		if perr != nil {
			// Serving the search unfiltered would hand a narrowed role what
			// its policy excludes; refuse instead.
			s.logger.Warn().Err(perr).Str("project_id", projectID).Str("task_id", who.TaskID).Str("role", who.Role).
				Msg("memory search: role policy unresolved; search refused")
			respondError(w, http.StatusServiceUnavailable, "POLICY_UNAVAILABLE", "the role's memory search policy could not be resolved")
			return
		}
		searchReq = applyMemorySearchRolePolicy(searchReq, pol, r.URL.Query().Get("limit") != "")
	}

	// B-15: stamp retrieval context so the audit row carries who
	// did the search. Without this every REST-API recall produced an
	// audit row with NULL actor_kind / actor_id, muddying the
	// dashboards that split agent vs companion vs operator-direct
	// retrievals. ActorID empty for static-keys-only is fine — the
	// audit row gets actor_kind="rest_api" + NULL actor_id, which
	// still distinguishes it from agent/companion paths.
	//
	// #617 slice 2: an agent's search also names its task (task key) or
	// role (warm-pool key), so the audit row ties the chunks to the task
	// that retrieved them. The task key is deleted when the task ends, so
	// this cannot be recovered later. ActorKind and ActorID stay as they
	// were: the dashboards and the memory firewall read only those.
	rc := &memory.RetrievalContext{
		ActorKind: "rest_api",
		ActorID:   APIKeyIDFromContext(r.Context()),
	}
	if isAgent {
		rc.TaskID, rc.Role = who.TaskID, who.Role
	}
	ctx := memory.WithRetrievalContext(r.Context(), rc)

	// Prefer the confidence-based retrieval routing path (P3) when the
	// searcher supports it: REST is a first opt-in caller (§6). The response
	// then carries the retrieval_trust_verdict + guidance + per-result trust
	// fields. Searchers without the capability use the legacy Search path,
	// whose response is byte-identical to before the feature.
	var (
		results []MemorySearchResult
		verdict *RoutingVerdictWire
		err     error
	)
	rs, routing := s.memorySearcher.(MemoryRoutingSearcher)
	switch {
	case routing:
		results, verdict, err = rs.SearchRouting(ctx, projectID, q, searchReq)
	case searchReq.narrows():
		respondError(w, http.StatusServiceUnavailable, "FILTER_UNSUPPORTED", "this memory searcher cannot apply repo_scope, strict_scope or authority")
		return
	default:
		results, err = s.memorySearcher.Search(ctx, projectID, q, searchReq.Limit)
	}
	if err != nil {
		s.logger.Warn().
			Err(err).
			Str("project_id", projectID).
			Str("query", q).
			Msg("memory search failed")
		respondError(w, http.StatusInternalServerError, "SEARCH_ERROR", "memory search failed")
		return
	}

	if results == nil {
		results = []MemorySearchResult{}
	}

	// #454 / #456: a model-facing caller gets every caller-controlled
	// chunk field wrapped. See wrapMemoryResultsForModel.
	toModel := memorySearchCallerIsModel(r)
	if toModel {
		wrapMemoryResultsForModel(results)
	}

	resp := memorySearchResponse{Results: results}
	if searchReq.narrows() {
		// An agent gets the policy and its own count only: a second,
		// unfiltered search would tell the model how much its policy
		// excluded, and record a retrieval of chunks nobody was shown.
		countWith := rs
		if isAgent {
			countWith = nil
		}
		resp.RetrievalPolicy = s.retrievalPolicyBlock(ctx, countWith, projectID, q, searchReq, len(results))
	}
	if verdict != nil {
		resp.RetrievalTrustVerdict = verdict.Verdict
		resp.Guidance = verdict.Guidance
		resp.WidenRounds = verdict.WidenRounds
		basis := verdict.Basis
		resp.VerdictBasis = &basis
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	enc := json.NewEncoder(w)
	// A model-facing body goes to the model as printed (handle_memory_search
	// does not decode it). Without HTML escaping the wrapper's markers reach
	// it as the literal tags the Prelude names rather than < escapes;
	// the content was already escaped by untrusted.WrapLabeled, which also
	// folds <-style spellings (security review 2026-10-21). Operator
	// callers keep the default encoding, byte for byte.
	enc.SetEscapeHTML(!toModel)
	_ = enc.Encode(resp)
}
