package cli

import (
	"fmt"
	"net/url"
	"strings"

	"vornik.io/vornik/internal/agentadmin"
	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/datasubject"
	"vornik.io/vornik/internal/retention"
)

// The Art 15(1)(c) recipients and (d) retention windows of the access report,
// read from the configuration (GitHub #470, GDPR design §4.22). Only hosts,
// regions, key names and day counts leave this file: never an API key, and
// never a full URL (a Vertex path names the GCP project).

// routerKinds are the router sub-providers, in the order the report lists
// them, with the config key each is enabled under.
var routerKinds = []struct{ kind, key string }{
	{"http", "chat.router.http"},
	{"vertex", "chat.router.vertex"},
	{"openrouter", "chat.router.openrouter"},
	{"ollama_cloud", "chat.router.ollama_cloud"},
	{"bedrock", "chat.router.bedrock"},
	{"claude-subscription", "chat.router.claude_subscription"},
	{"codex-subscription", "chat.router.codex_subscription"},
	{"claude-cli", "chat.router.claude_cli"},
	{"codex-cli", "chat.router.codex_cli"},
}

func routerKindEnabled(r config.ChatRouterConfig, kind string) bool {
	switch kind {
	case "http":
		return r.HTTP.Enabled
	case "vertex":
		return r.Vertex.Enabled
	case "openrouter":
		return r.OpenRouter.Enabled
	case "ollama_cloud":
		return r.OllamaCloud.Enabled
	case "bedrock":
		return r.Bedrock.Enabled
	case "claude-subscription":
		return r.ClaudeSubscription.Enabled
	case "codex-subscription":
		return r.CodexSubscription.Enabled
	case "claude-cli":
		return r.ClaudeCLI.Enabled
	case "codex-cli":
		return r.CodexCLI.Enabled
	}
	return false
}

// subjectRecipients lists the model providers the configuration enables: the
// chat providers, the agents' endpoint when it is another host, and the memory
// embedding endpoint.
func subjectRecipients(cfg *config.Config) []datasubject.Recipient {
	var out []datasubject.Recipient
	if cfg.Chat.Enabled {
		provider := strings.ToLower(strings.TrimSpace(cfg.Chat.Provider))
		switch provider {
		case "router":
			for _, k := range routerKinds {
				if !routerKindEnabled(cfg.Chat.Router, k.kind) {
					continue
				}
				endpoint, cli, ok := chat.RouterSubEndpoint(cfg.Chat, k.kind)
				out = append(out, modelRecipient(k.kind, k.key, endpoint, cli, ok, routerRegion(cfg.Chat.Router, k.kind)))
			}
		case "claude-cli", "codex-cli":
			out = append(out, modelRecipient(provider, "chat.provider", "", true, true, ""))
		default:
			out = append(out, modelRecipient("http", "chat.endpoint", cfg.Chat.Endpoint, false, true, ""))
		}
	}
	if ep := cfg.Runtime.AgentLLM.Endpoint; ep != "" && !hasDestination(out, ep) {
		out = append(out, modelRecipient("agents", "runtime.agent_llm.endpoint", ep, false, true, ""))
	}
	if cfg.Memory.Enabled {
		out = append(out, embeddingRecipient(cfg))
	}
	return out
}

func embeddingRecipient(cfg *config.Config) datasubject.Recipient {
	if strings.EqualFold(strings.TrimSpace(cfg.Memory.EmbeddingProvider), "bedrock") {
		region, key := cfg.Memory.Bedrock.Region, "memory.bedrock.region"
		if region == "" {
			region, key = cfg.Chat.Router.Bedrock.Region, "chat.router.bedrock.region"
		}
		endpoint := ""
		if region != "" {
			endpoint = chat.BedrockRuntimeEndpoint(region)
		}
		r := modelRecipient("embeddings (bedrock)", key, endpoint, false, true, region)
		r.Receives = "memory text, to compute its embedding"
		return r
	}
	endpoint, key := cfg.Memory.EmbeddingEndpoint, "memory.embedding_endpoint"
	if endpoint == "" {
		endpoint, key = cfg.ResolvedAgentLLM().Endpoint, "memory.embedding_endpoint (unset: the agent LLM endpoint)"
	}
	r := modelRecipient("embeddings", key, endpoint, false, true, "")
	r.Receives = "memory text, to compute its embedding"
	return r
}

// modelRecipient classifies one provider with agentadmin.DestinationOf, the
// rule the agent model catalogue applies, so the report and the catalogue
// call the same endpoint local or remote.
func modelRecipient(name, key, endpoint string, cli, ok bool, region string) datasubject.Recipient {
	r := datasubject.Recipient{Name: name, Category: datasubject.CategoryModelProvider,
		Receives: "prompt content", ConfigKey: key, Region: region}
	d, why := agentadmin.DestinationOf(agentadmin.ModelRoute{SubProvider: name, Endpoint: endpoint, CLI: cli}, ok)
	switch {
	case cli:
		r.Destination = datasubject.Unknown + ": a command-line provider; Vornik cannot see where it sends a call"
		r.Location = datasubject.Unknown
	case why != "":
		r.Destination = datasubject.Unknown + ": " + why
		r.Location = datasubject.Unknown
	case d.Host == "":
		// Never call an endpoint local or remote without a host, whatever the
		// classifier said (review 6a1e).
		r.Destination = datasubject.Unknown + ": the endpoint has no host"
		r.Location = datasubject.Unknown
	case d.Local:
		r.Destination, r.Location = d.Host, datasubject.LocationLocal
	default:
		r.Destination, r.Location = d.Host, datasubject.LocationRemote
	}
	return r
}

// routerRegion is the region the configuration names for a sub-provider, or
// empty: the report states a region only when the config does.
func routerRegion(r config.ChatRouterConfig, kind string) string {
	switch kind {
	case "bedrock":
		return r.Bedrock.Region
	case "vertex":
		if r.Vertex.Endpoint == "" {
			if r.Vertex.Location == "" {
				return "global"
			}
			return r.Vertex.Location
		}
	}
	return ""
}

func hasDestination(rs []datasubject.Recipient, endpoint string) bool {
	u, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil {
		return false
	}
	for _, r := range rs {
		if strings.EqualFold(r.Destination, u.Hostname()) {
			return true
		}
	}
	return false
}

// subjectRetention states the deployment-wide windows, resolved by
// internal/retention exactly as the daemon's sweeper resolves them.
func subjectRetention(cfg *config.Config) datasubject.RetentionStatement {
	rc := cfg.Retention
	// ResolveToolAuditFloor returns a warning, not an error: the floor it
	// returns is the one the sweeper applies either way. The warning (an
	// ignored floor, a refused or an accepted override) is stated on the tool
	// audit window rather than dropped (review 6a1e). GlobalFromConfig's
	// second value is the upgrade-dump policy, which holds no personal data
	// window.
	floor, floorWarning := retention.ResolveToolAuditFloor(rc.ToolAuditFloorDays, rc.ToolAuditFloorOverride, rc.ToolAuditFloorOverrideBasis)
	p := retention.Resolve("", retention.Policy{}, retention.DefaultsFromConfig(rc, floor, ""))
	g, _ := retention.GlobalFromConfig(rc)

	st := datasubject.RetentionStatement{
		Known:          true,
		SweeperEnabled: rc.Enabled,
		Scope: "These are the deployment-wide windows. A project's own retention settings may set " +
			"a different window for that project's data; this report does not read them.",
	}
	add := func(data string, tables []string, key string, set, effective int, optIn bool) {
		w := datasubject.RetentionWindow{Data: data, Tables: tables, ConfigKey: "retention." + key, Days: effective}
		switch {
		case set != 0:
			w.Source = datasubject.WindowConfigured
		case optIn && effective <= 0:
			w.Source, w.Days = datasubject.WindowNotSet, 0
		default:
			w.Source = datasubject.WindowDefault
		}
		switch {
		case w.Days <= 0:
			w.Period = "kept until deleted another way; no period is configured"
		case !rc.Enabled:
			w.Period = fmt.Sprintf("%d days is configured, but not applied: the retention sweeper is off "+
				"(retention.enabled: false), so the data is kept until deleted another way", w.Days)
		default:
			w.Period = fmt.Sprintf("deleted %d days after it was stored", w.Days)
		}
		st.Windows = append(st.Windows, w)
	}
	add("tasks", []string{"tasks"}, "tasks_days", rc.TasksDays, p.TasksDays, false)
	add("task executions", []string{"executions"}, "executions_days", rc.ExecutionsDays, p.ExecutionsDays, false)
	add("uploaded and produced files", []string{"artifacts"}, "artifacts_days", rc.ArtifactsDays, p.ArtifactsDays, false)
	add("task messages, beyond removal with their task", []string{"task_messages"}, "task_messages_days", rc.TaskMessagesDays, p.TaskMessagesDays, true)
	add("chat messages", []string{"chat_audit_log"}, "chat_audit_days", rc.ChatAuditDays, p.ChatAuditDays, false)
	add("tool calls, memory searches and autonomy proposals", []string{"tool_audit_log", "memory_retrieval_audit", "autonomy_evaluations"},
		"tool_audit_days", rc.ToolAuditDays, p.ToolAuditDays, false)
	if floorWarning != "" {
		for i := range st.Windows {
			if st.Windows[i].ConfigKey == "retention.tool_audit_days" {
				st.Windows[i].Period += ". Note on the six-month minimum: " + floorWarning
			}
		}
	}
	add("project memory, beyond each memory class's own period", []string{"project_memory_chunks"}, "memory_chunks_days", rc.MemoryChunksDays, p.MemoryChunksDays, true)
	add("knowledge-graph records set aside when their source expired", []string{"knowledge_entities"}, "graph_quarantine_days",
		rc.GraphQuarantineDays, p.EffectiveGraphQuarantineDays(), false)
	add("memory deposit records", []string{"memory_ingest_audit"}, "memory_ingest_audit_days", rc.MemoryIngestAuditDays, p.MemoryIngestAuditDays, false)
	add("erasure records", []string{"memory_eviction_audit", "memory_eviction_runs"}, "memory_eviction_audit_days", rc.MemoryEvictionAuditDays, p.MemoryEvictionAuditDays, false)
	add("cached embeddings", []string{"embedding_cache"}, "embedding_cache_days", rc.EmbeddingCacheDays, g.EmbeddingCacheDays, true)
	add("cached model responses", []string{"llm_response_cache"}, "response_cache_days", rc.ResponseCacheDays, g.ResponseCacheDays, true)
	add("assembled task traces", []string{"blackbox_trace_cache"}, "blackbox_trace_cache_days", rc.BlackboxTraceCacheDays,
		g.EffectiveBlackboxTraceCacheDays(), false)
	add("project summaries", []string{"project_gists"}, "project_gists_days", rc.ProjectGistsDays,
		g.EffectiveProjectGistsDays(), false)
	add("ratings of task output", []string{"execution_ratings"}, "execution_ratings_days", rc.ExecutionRatingsDays,
		g.EffectiveExecutionRatingsDays(), false)

	// ropa.md §7: data with no window.
	for _, w := range []datasubject.RetentionWindow{
		{Data: "text and keyframes extracted from uploaded documents and media", Tables: []string{"extracted_documents"},
			Period: "no retention window exists; kept until the source file is erased"},
		{Data: "records of AI disclosure", Tables: []string{"channel_disclosure_log"},
			Period: "exempt from deletion by age: kept as evidence under EU AI Act Art 99"},
		{Data: "records of data-subject requests", Tables: []string{"data_subjects", "data_subject_links", "data_subject_requests", "data_subject_restrictions"},
			Period: "no retention window exists; kept as the record that a right was exercised"},
	} {
		w.Source = datasubject.WindowNone
		st.Windows = append(st.Windows, w)
	}
	return st
}
