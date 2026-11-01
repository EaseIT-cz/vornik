package persistence

import "sort"

// The table census of the owner export (GitHub #581 D5,
// enterprise-packaging-design.md amendment 2026-10-29). Every table the
// migrations create is classified: OwnerData is exported as it is, the
// customer's own data; SecretMaterial is emptied in the export's dump
// (pg_dump --exclude-table-data), schema kept. The census test enumerates a
// fully migrated database and fails on a table missing here, so a new table
// cannot reach an export unexamined, and on a foreign key that references a
// SecretMaterial table, which would break a restore of the export. A table
// that keeps config text (pre-images, diffs, snapshots, transcripts) is
// SecretMaterial too: a managed config holds keys literally (commit security
// review of bab44b9b2), and a lane test requires it by column name.

// TableClass is what the owner export does with a table's rows.
type TableClass int

const (
	// OwnerData rows are exported.
	OwnerData TableClass = iota + 1
	// SecretMaterial rows are left out of the export; the table is kept.
	SecretMaterial
)

// TableCensusEntry classifies one table; Why says what secret it holds.
type TableCensusEntry struct {
	Class TableClass
	Why   string
}

// TableCensus classifies every table the migrations create.
var TableCensus = map[string]TableCensusEntry{
	"a2a_push_configs":                 {SecretMaterial, "the push endpoints' bearer tokens"},
	"admin_audit":                      {SecretMaterial, "config text that may hold a literal secret (pre-images, diffs, snapshots, transcripts)"},
	"agent_account_transitions":        {OwnerData, ""},
	"agent_admin_refusals":             {OwnerData, ""},
	"agent_approval_requests":          {OwnerData, ""},
	"agent_connector_installs":         {OwnerData, ""},
	"agent_integration_approvals":      {OwnerData, ""},
	"agent_model_provider_approvals":   {OwnerData, ""},
	"agent_namespace_budgets":          {OwnerData, ""},
	"agent_secrets":                    {SecretMaterial, "sealed secret-store values"},
	"agent_workflow_approvals":         {OwnerData, ""},
	"api_keys":                         {SecretMaterial, "API key hashes (credential verifiers)"},
	"approver_devices":                 {SecretMaterial, "paired devices' token hashes and rotation nonces"},
	"approver_pairings":                {SecretMaterial, "pairing claim and code hashes"},
	"artifacts":                        {OwnerData, ""},
	"autonomy_evaluations":             {OwnerData, ""},
	"blackbox_trace_cache":             {OwnerData, ""},
	"blackbox_trace_cache_generation":  {OwnerData, ""},
	"broker_actions":                   {OwnerData, ""},
	"broker_standing_grants":           {SecretMaterial, "standing write approvals, an authorization a restore must not carry"},
	"budget_reservations":              {OwnerData, ""},
	"channel_disclosure_log":           {OwnerData, ""},
	"ai_output_marks":                  {OwnerData, ""},
	"ai_mark_keys":                     {OwnerData, ""},
	"channel_sessions":                 {OwnerData, ""},
	"chat_audit_log":                   {OwnerData, ""},
	"chat_memory_write_audit":          {OwnerData, ""},
	"chat_memory_write_confirmations":  {OwnerData, ""},
	"chat_system_prompts":              {OwnerData, ""},
	"class_e_slot_reservations":        {OwnerData, ""},
	"cluster_nodes":                    {OwnerData, ""},
	"config_apply_journal":             {SecretMaterial, "config text that may hold a literal secret (pre-images, diffs, snapshots, transcripts)"},
	"control_plane_proposals":          {SecretMaterial, "config text that may hold a literal secret (pre-images, diffs, snapshots, transcripts)"},
	"corpus_embedding_versions_active": {OwnerData, ""},
	"corpus_epochs":                    {OwnerData, ""},
	"corpus_epochs_active":             {OwnerData, ""},
	"corpus_rollbacks":                 {OwnerData, ""},
	"cost_tuning_canaries":             {OwnerData, ""},
	"cross_project_calls":              {OwnerData, ""},
	"daemon_leader_locks":              {OwnerData, ""},
	"data_subject_identifiers":         {OwnerData, ""},
	"data_subject_links":               {OwnerData, ""},
	"data_subject_requests":            {OwnerData, ""},
	"data_subject_restrictions":        {OwnerData, ""},
	"data_subjects":                    {OwnerData, ""},
	"dispatcher_reminders":             {OwnerData, ""},
	"embedding_cache":                  {OwnerData, ""},
	"entity_mentions":                  {OwnerData, ""},
	"execution_hints":                  {OwnerData, ""},
	"execution_injected_skills":        {OwnerData, ""},
	"execution_live_events":            {OwnerData, ""},
	"execution_narration":              {OwnerData, ""},
	"execution_quality_scores":         {OwnerData, ""},
	"execution_ratings":                {OwnerData, ""},
	"execution_step_outcomes":          {OwnerData, ""},
	"execution_tool_grants":            {OwnerData, ""},
	"executions":                       {OwnerData, ""},
	"extracted_documents":              {OwnerData, ""},
	"fixit_sessions":                   {SecretMaterial, "config text that may hold a literal secret (pre-images, diffs, snapshots, transcripts)"},
	"forge_ci_outcomes":                {OwnerData, ""},
	"forge_pr_review_state":            {OwnerData, ""},
	"group_members":                    {OwnerData, ""},
	"group_projects":                   {OwnerData, ""},
	"groups":                           {OwnerData, ""},
	"installation_onboarding_sessions": {SecretMaterial, "config text that may hold a literal secret (pre-images, diffs, snapshots, transcripts)"},
	"instance_exports":                 {SecretMaterial, "the export registry: ids that serve the owner's downloads"},
	"data_subject_escalations":         {OwnerData, "which erasure rows were escalated to the operator, by request id; no content"},
	"instance_milestones":              {OwnerData, "the first recall hit's and first answer's times; no secret"},
	"instinct_action_history":          {OwnerData, ""},
	"instinct_applications":            {OwnerData, ""},
	"instinct_evidence":                {OwnerData, ""},
	"instinct_lift":                    {OwnerData, ""},
	"instincts":                        {OwnerData, ""},
	"intent_verdicts":                  {OwnerData, ""},
	"key_claim_attempts":               {SecretMaterial, "key-guessing bookkeeping"},
	"knowledge_edges":                  {OwnerData, ""},
	"knowledge_entities":               {OwnerData, ""},
	"link_codes":                       {SecretMaterial, "identity link code hashes"},
	"llm_exchanges":                    {OwnerData, ""},
	"llm_response_cache":               {OwnerData, ""},
	"mcp_oauth_tokens":                 {SecretMaterial, "sealed connector access and refresh tokens"},
	"memory_chunk_provenance":          {OwnerData, ""},
	"memory_embed_dlq":                 {OwnerData, ""},
	"memory_embed_queue":               {OwnerData, ""},
	"memory_eviction_audit":            {OwnerData, ""},
	"memory_eviction_runs":             {OwnerData, ""},
	"memory_ingest_audit":              {OwnerData, ""},
	"memory_policy_evaluations":        {OwnerData, ""},
	"memory_retrieval_audit":           {OwnerData, ""},
	"memory_search_stages":             {OwnerData, ""},
	"memory_verification_backup":       {OwnerData, ""},
	"memory_verification_events":       {OwnerData, ""},
	"migrations":                       {OwnerData, ""},
	"operator_identity_link":           {OwnerData, ""},
	"operator_profile":                 {OwnerData, ""},
	"package_contributions":            {OwnerData, ""},
	"profile_use_audit":                {OwnerData, ""},
	"project_first_seen":               {OwnerData, ""},
	"project_gists":                    {OwnerData, ""},
	"project_ingest_queue":             {OwnerData, ""},
	"project_memory_chunks":            {OwnerData, ""},
	"project_memory_quarantine":        {OwnerData, ""},
	"project_skill_versions":           {OwnerData, ""},
	"project_skills":                   {OwnerData, ""},
	"project_spawns":                   {OwnerData, ""},
	"project_wizard_sessions":          {SecretMaterial, "config text that may hold a literal secret (pre-images, diffs, snapshots, transcripts)"},
	"ratelimit_counters":               {OwnerData, ""},
	"recovery_events":                  {OwnerData, ""},
	"schema_registry":                  {OwnerData, ""},
	"secret_redaction_audit":           {OwnerData, ""},
	"security_incidents":               {OwnerData, ""},
	"step_prompts":                     {OwnerData, ""},
	"task_credentials":                 {SecretMaterial, "credential values handed to tasks"},
	"task_judge_verdicts":              {OwnerData, ""},
	"task_llm_usage":                   {OwnerData, ""},
	"task_messages":                    {OwnerData, ""},
	"task_post_mortems":                {OwnerData, ""},
	"task_scratchpad":                  {OwnerData, ""},
	"task_watchers":                    {OwnerData, ""},
	"tasks":                            {OwnerData, ""},
	"tasks_lease_audit":                {OwnerData, ""},
	"telegram_poller_state":            {OwnerData, ""},
	"telegram_task_threads":            {OwnerData, ""},
	"tool_audit_log":                   {OwnerData, ""},
	"trading_fills":                    {OwnerData, ""},
	"trading_fills_shadow":             {OwnerData, ""},
	"trading_orders":                   {OwnerData, ""},
	"trading_funnel_candidates":        {OwnerData, ""},
	"trading_benchmark_closes":         {OwnerData, ""},
	"trading_shadow_decisions":         {OwnerData, ""},
	"trading_positions_snapshots":      {OwnerData, ""},
	"trading_safety_events":            {OwnerData, ""},
	"ui_sessions":                      {SecretMaterial, "console session token hashes"},
	"user_identities":                  {OwnerData, ""},
	"users":                            {OwnerData, ""},
	"web_write_actions":                {SecretMaterial, "one-time approval token hashes"},
	"webhook_events":                   {OwnerData, ""},
	"workflow_healing_candidates":      {OwnerData, ""},
	"workflow_healing_overrides":       {OwnerData, ""},
	"workflow_healing_trials":          {OwnerData, ""},
	"workflow_healing_triggers":        {OwnerData, ""},
	"workflow_proposals":               {OwnerData, ""},
}

// SecretMaterialTables returns the SecretMaterial tables, sorted.
func SecretMaterialTables() []string {
	var out []string
	for t, e := range TableCensus {
		if e.Class == SecretMaterial {
			out = append(out, t)
		}
	}
	sort.Strings(out)
	return out
}

// ReviewedOwnerTextColumns are the owner-data columns whose names look like
// config text, each reviewed and kept in the export with its reason (commit
// security review of 41e07408b). The lane test requires every column named
// like config text to be in a SecretMaterial table or here; the export's dump
// is scrubbed by value in any case (cli.scrubDumpFile).
var ReviewedOwnerTextColumns = map[string]string{
	"cross_project_calls.result_envelope":       "a call's result: the owner's output",
	"executions.state_snapshot":                 "an execution's state: the owner's work",
	"executions.workflow_snapshot":              "the workflow the run used: secrets in workflows are secret:// references",
	"tasks.result_envelope":                     "a task's result: the owner's output",
	"workflow_healing_candidates.proposal_diff": "a proposed workflow change: references only, as above",
	"workflow_proposals.pre_apply_yaml":         "a workflow before a change: references only, as above",
	"workflow_proposals.proposal_yaml":          "a proposed workflow: references only, as above",
}
