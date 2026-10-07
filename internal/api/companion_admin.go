package api

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"vornik.io/vornik/internal/agentadmin"
	"vornik.io/vornik/internal/agentns"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// The agent admin verbs on the companion endpoint (agent-administered
// Vornik design §6). They are listed and callable only for an agent admin
// key, and only while agent_admin.enabled is on; to any other key an admin
// verb is the same "unknown tool" a stranger gets.

// AgentAdminVerbs is the service behind the verbs (internal/service).
type AgentAdminVerbs interface {
	Do(ctx context.Context, key *persistence.APIKey, verb string, input json.RawMessage) (agentadmin.Result, error)
	ListSetupJSON(ctx context.Context, key *persistence.APIKey) (any, error)
	DescribeJSON(ctx context.Context, key *persistence.APIKey) (any, error)
	// ListRecipesJSON answers list_recipes (design §19.2).
	ListRecipesJSON(ctx context.Context, key *persistence.APIKey) (any, error)
	// ApprovedWorkflows is every approved workflow of the key's namespace,
	// sorted: what catalog lists for an agent admin key (design §18.3).
	ApprovedWorkflows(ctx context.Context, key *persistence.APIKey) ([]string, error)
	// EnsureHome creates the namespace's home project if it does not exist
	// and returns its ID (the grant binds the agent admin key to it).
	EnsureHome(ctx context.Context, namespace, clientKind string) (string, error)
}

// WithAgentAdmin wires the verbs; enabled is agent_admin.enabled.
func WithAgentAdmin(v AgentAdminVerbs, enabled func() bool) ServerOption {
	return func(srv *Server) { srv.agentAdmin, srv.agentAdminEnabled = v, enabled }
}

// The verb names.
const (
	toolListMySetup          = "list_my_setup"
	toolDescribeInstallation = "describe_installation"
)

var agentAdminMutating = map[string]bool{
	agentadmin.VerbCreateProject: true, agentadmin.VerbDefineSwarm: true, agentadmin.VerbDefineWorkflow: true,
	agentadmin.VerbAddMCPServer: true, agentadmin.VerbAddAPI: true, agentadmin.VerbRequestCredential: true,
	agentadmin.VerbSetBudget: true, agentadmin.VerbRemove: true, agentadmin.VerbInstallRecipe: true,
	agentadmin.VerbUpdateProject: true,
}

// keyCoversProject reports whether a companion key may see a task in
// project: its own project, or, for an agent admin key, any project of its
// namespace (design §5: the key delegates into every broker project of its
// namespace, so it must read those tasks' status and results).
func keyCoversProject(key *persistence.APIKey, project string) bool {
	if key == nil {
		return false
	}
	if project == key.ProjectID {
		return true
	}
	if !key.AgentAdmin {
		return false
	}
	ns, ok := agentns.FromID(project)
	return ok && ns == key.AgentNamespace
}

// isAgentAdminTool reports whether name is an admin verb.
func isAgentAdminTool(name string) bool {
	return agentAdminMutating[name] || name == toolListMySetup || name == toolDescribeInstallation || name == agentadmin.VerbListRecipes
}

// agentAdminOffered reports whether key may see and call the admin verbs.
func (s *Server) agentAdminOffered(key *persistence.APIKey) bool {
	return key != nil && key.AgentAdmin && s.agentAdmin != nil && s.agentAdminEnabled != nil && s.agentAdminEnabled()
}

// companionToolsFor is tools/list for one key.
func (s *Server) companionToolsFor(key *persistence.APIKey) []mcpToolDef {
	// Offer exactly what the key may call: the same gate the call path
	// applies (DoD lane bring-up, 2026-10-02: an agent admin key was offered
	// memory and skill tools its broker home project always refuses).
	var defs []mcpToolDef
	for _, d := range companionToolDefs() {
		if key == nil || s.gateCompanionTool(key, d.Name) == nil {
			defs = append(defs, d)
		}
	}
	// Broker-project keys run workflows in their own broker project; agent-admin
	// keys delegate into broker workflows across their namespace. Both surfaces
	// refuse prompt/inputArtifacts and take typed inputs instead.
	if s.isBrokerProjectKey(key) || s.agentAdminOffered(key) {
		for i := range defs {
			if defs[i].Name == "delegate" {
				defs[i] = brokerOnlyDelegate(defs[i])
			}
		}
	} else {
		for i := range defs {
			if defs[i].Name == "delegate" {
				defs[i] = promptOnlyDelegate(defs[i])
			}
		}
	}
	if s.agentAdminOffered(key) {
		defs = append(defs, companionAdminToolDefs()...)
	}
	return defs
}

// brokerOnlyDelegate is delegate as a broker-project or agent-admin key may
// call it: every workflow it can run is a broker workflow, which refuses a
// prompt and inputArtifacts, so the schema omits both instead of inviting a
// refused call (broker design 2026-09-29 §4.3; agent-admin design §18.2,
// review 3e94 F3). The definition is copied, never mutated.
func brokerOnlyDelegate(d mcpToolDef) mcpToolDef {
	return delegateSchemaWithout(d, []string{"prompt", "inputArtifacts", "skip_auto_extract", "acknowledge_workflow_cannot_fetch"}, []string{"workflow", "inputs"})
}

// promptOnlyDelegate is delegate as an ordinary companion key may call it:
// ordinary workflows take a prompt, not broker typed inputs. The definition is
// copied, never mutated.
func promptOnlyDelegate(d mcpToolDef) mcpToolDef {
	return delegateSchemaWithout(d, []string{"inputs"}, []string{"workflow", "prompt"})
}

func delegateSchemaWithout(d mcpToolDef, omit []string, required []string) mcpToolDef {
	omitted := map[string]bool{}
	for _, k := range omit {
		omitted[k] = true
	}
	schema := map[string]any{}
	for k, v := range d.InputSchema {
		schema[k] = v
	}
	props := map[string]any{}
	if in, ok := d.InputSchema["properties"].(map[string]any); ok {
		for k, v := range in {
			if omitted[k] {
				continue
			}
			props[k] = v
		}
	}
	schema["properties"] = props
	schema["required"] = required
	d.InputSchema = schema
	return d
}

// inputsDescription is define_workflow's inputs description, built from the
// validator's published rules so the two cannot drift (design §18.2).
func inputsDescription() string {
	var b strings.Builder
	b.WriteString("JSON Schema (an object) of the typed inputs you will pass to delegate. Rules, enforced when the workflow is defined: ")
	for i, r := range registry.BrokerInputRules() {
		if i > 0 {
			b.WriteString("; ")
		}
		b.WriteString(r.Text)
	}
	b.WriteString(".")
	return b.String()
}

// companionAdminTool runs one admin verb.
func (s *Server) companionAdminTool(ctx context.Context, key *persistence.APIKey, name string, args json.RawMessage) (string, error) {
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	var (
		out any
		err error
	)
	switch name {
	case toolListMySetup:
		out, err = s.agentAdmin.ListSetupJSON(ctx, key)
	case toolDescribeInstallation:
		out, err = s.agentAdmin.DescribeJSON(ctx, key)
	case agentadmin.VerbListRecipes:
		out, err = s.agentAdmin.ListRecipesJSON(ctx, key)
	default:
		out, err = s.agentAdmin.Do(ctx, key, name, args)
	}
	if err != nil {
		return "", fmt.Errorf("%s failed: %w", name, err)
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func obj(props map[string]any, required ...string) map[string]any {
	m := map[string]any{"type": "object", "additionalProperties": false, "properties": props}
	if len(required) > 0 {
		m["required"] = required
	}
	return m
}

func str(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }

// described adds a description to a schema.
func described(desc string, m map[string]any) map[string]any {
	m["description"] = desc
	return m
}

// companionAdminToolDefs are the admin verbs' definitions. Every mutating
// verb returns {change_id, effect: applied|awaiting_approval|refused,
// approval_url?, reason?, sentence?}.
func companionAdminToolDefs() []mcpToolDef {
	effect := " Returns {change_id, effect: applied | awaiting_approval | refused, approval_url, reason, sentence}. " +
		"awaiting_approval means the user must approve on their phone at approval_url; tell them the sentence."
	roleTools := "Each tool is a workspace/clock built-in (see describe_installation) or mcp__<server>__<tool> for a read tool of a server already approved for this project."
	return []mcpToolDef{
		{Name: toolDescribeInstallation, Description: "Call this first. How to work with Vornik, what you may and may not do, what needs the user's approval on their phone, the models a role may choose, and your current setup.", InputSchema: obj(map[string]any{})},
		{Name: toolListMySetup, Description: "Your projects, roles, workflows, servers, credentials (names only, never values), budgets, and requests awaiting approval, failed or expired.", InputSchema: obj(map[string]any{})},
		{Name: agentadmin.VerbCreateProject, Description: "Create a project (a private, broker-only space) with the default budget." + effect,
			InputSchema: obj(map[string]any{"slug": str(agentadmin.NewSlugRule), "purpose": str("One line saying what it is for."), "template": str("Optional; only \"default\".")}, "slug", "purpose")},
		{Name: agentadmin.VerbDefineSwarm, Description: "Set the roles of a project (its slug). A worker role always remains." + effect,
			InputSchema: obj(map[string]any{"slug": str(agentadmin.ProjectRefRule), "roles": map[string]any{"type": "array", "maxItems": 8, "items": obj(map[string]any{
				"name": str("Role name: a-z, 0-9, _ or -."), "instructions": str("What the role does. No lines starting with #, --- or ```."),
				"tools": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": roleTools},
				"model": str("Optional: a model id from describe_installation's models; leave it out for the installation's default. " +
					"A local model applies at once; a remote one sends this role's work to its provider and needs the user's approval on their phone, once per destination.")},
				"name", "instructions", "tools")}}, "slug", "roles")},
		{Name: agentadmin.VerbDefineWorkflow, Description: "Define a workflow you can delegate: steps run in order; what it returns is exactly the egress schema, which the user approves." + effect,
			InputSchema: obj(map[string]any{"project": str(agentadmin.ProjectRefRule), "slug": str("The workflow's slug."), "purpose": str("One line."),
				"steps":  map[string]any{"type": "array", "maxItems": 10, "description": "Run in order. " + agentadmin.HandoffRule, "items": obj(map[string]any{"name": str("Step name."), "role": str("A role of the project."), "instructions": str("What the step does.")}, "name", "role", "instructions")},
				"inputs": map[string]any{"type": "object", "description": inputsDescription()},
				"egress": map[string]any{"type": "object", "description": "JSON Schema of what you get back: objects with additionalProperties false, strings with maxLength <= 2000, arrays with maxItems <= 50, numbers, booleans, enums."},
				"proposes": map[string]any{"type": "array", "maxItems": 8, "description": "Optional writes the workflow may PROPOSE; the user approves each one on their phone before it is made. " +
					"Each: {action (a-z, 0-9, _), tool (mcp__<server>-write__<tool> of an approved write tool, or api:<name>:<METHOD>:<path> of an approved API write), " +
					"args_schema (JSON Schema object of the arguments; for an API write, the request body), " +
					"and optionally standing: {key: [argument names], max_days <= 7, max_uses <= 20}, which lets the user, when approving one write, also approve future writes with the same key values " +
					"(sent without being shown to them). The key must include every argument marked \"x-destination\": true (who the write goes to); a schema with an argument marked \"x-carries-content\": true cannot declare it.",
					"items": map[string]any{"type": "object"}},
				"schedule": described("Optional: run the workflow automatically. The user approves the schedule, its timezone and its inputs on their phone, and any change to them. At most hourly. "+
					"Each run's task id is in list_my_setup (recent_runs); read its output with result.", obj(map[string]any{
					"cron":     str("5-field cron: minute hour day-of-month month day-of-week, e.g. \"0 8 1 * *\" for 08:00 on the 1st of every month. At most hourly; no @shortcuts."),
					"timezone": str("IANA timezone, e.g. Europe/Prague; default UTC."),
					"inputs":   map[string]any{"type": "object", "description": "The inputs every scheduled run gets; they must match the workflow's inputs schema."},
				}, "cron", "inputs"))}, "project", "slug", "steps", "egress")},
		{Name: agentadmin.VerbAddMCPServer, Description: "Connect a project to a remote MCP server (https, or http on this machine). Always needs approval." + effect,
			InputSchema: obj(map[string]any{"project": str(agentadmin.ProjectRefRule), "name": str("Server name."), "url": str("Server URL."),
				"auth": obj(map[string]any{"mode": str("none, static, or oauth (the user signs in on their phone)."),
					"credential": str("For static: the credential NAME (A-Z, 0-9, _). The user enters its value on their phone; you never see it."),
					"scopes":     map[string]any{"type": "array", "maxItems": 20, "items": map[string]any{"type": "string"}, "description": "For oauth: the scopes to ask for."}}, "mode"),
				"write_tools": map[string]any{"type": "array", "maxItems": 16, "items": map[string]any{"type": "string"},
					"description": "Optional: the server's tools that change something (send, delete, pay). No role can call them; a workflow can only propose them, and the user approves each change on their phone."}}, "project", "name", "url", "auth")},
		{Name: agentadmin.VerbSetBudget, Description: "Set a project's monthly budget. Lowering applies at once; raising needs approval." + effect,
			InputSchema: obj(map[string]any{"project": str(agentadmin.ProjectRefRule), "monthly_usd": map[string]any{"type": "number", "exclusiveMinimum": 0}}, "project", "monthly_usd")},
		{Name: agentadmin.VerbUpdateProject, Description: "Update a project's purpose or display name. Applies at once; its slug stays unchanged." + effect,
			InputSchema: obj(map[string]any{"project": str(agentadmin.ProjectRefRule + " Immutable."), "purpose": str("Optional nonempty purpose, at most 300 characters on one line."), "display_name": str("Optional nonempty display label, at most 300 characters on one line. Supply at least one metadata field.")}, "project")},
		{Name: agentadmin.VerbRemove, Description: "Remove a project, a workflow or a server." + effect,
			InputSchema: obj(map[string]any{"kind": str("project, workflow or integration."), "id": str("A project: " + agentadmin.ProjectRefRule + " A workflow: <project>--<workflow>, without the namespace. An integration: the server name."), "project": str("For an integration: " + agentadmin.ProjectRefRule)}, "kind", "id")},
		{Name: agentadmin.VerbAddAPI, Description: "Connect a project to a REST API (https, or http on this machine). Always needs approval. " +
			"A role with the query_api tool can then read it (GET, HEAD); write methods can only be proposed by a workflow, and the user approves each change." + effect,
			InputSchema: obj(map[string]any{"project": str(agentadmin.ProjectRefRule), "name": str("API name: a-z, 0-9, _ or -."), "base_url": str("The API's base URL."),
				"auth": obj(map[string]any{"credential": str("The credential NAME (A-Z, 0-9, _); the user enters its value on their phone. Omit for no auth."),
					"header":      str("Header to carry it (default Authorization)."),
					"query_param": str("Query parameter to carry it, for legacy APIs such as Google Maps (for example, key). Mutually exclusive with header."),
					"prefix":      str("Prefix such as \"Bearer \", for header auth only.")}),
				"methods": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "GET, HEAD, POST, PUT, PATCH, DELETE."},
				"writes":  map[string]any{"type": "boolean", "description": "True exactly when methods include a write."}}, "project", "name", "base_url", "methods")},
		{Name: agentadmin.VerbListRecipes, Description: "Recipes: ready-made, tested workflows Vornik ships (an inbox digest, an agenda, a morning brief). " +
			"Each says what it reads, the variables you fill, what it returns, the sentence the user will approve, and where it is installed. Prefer a recipe when one fits.",
			InputSchema: obj(map[string]any{})},
		{Name: agentadmin.VerbInstallRecipe, Description: "Install a recipe into a project as one change: its server, its roles (named <recipe>-<role>), its workflow and schedule. " +
			"The user approves it once on their phone; then each credential it needs is requested on the phone. Installing the same version again changes nothing." + effect,
			InputSchema: obj(map[string]any{"recipe": str("The recipe's name, from list_recipes."), "project": str(agentadmin.ProjectRefRule),
				"variables": map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"},
					"description": "The recipe's variables by name, each a string (list_recipes gives their types and help). A credential is never a variable."},
				"schedule": map[string]any{"type": []any{"object", "null"}, "description": "Omit to use the recipe's default schedule (list_recipes shows it), " +
					"null for no schedule, or {cron, timezone, inputs} as in define_workflow."}}, "recipe", "project")},
		{Name: agentadmin.VerbRequestCredential, Description: "Ask the user to enter a credential that a server of a project already names (add the server first). " +
			"The user types the value on their phone; you never see it and cannot pass one. Always needs approval: entering the value is the approval." + effect,
			InputSchema: obj(map[string]any{"project": str(agentadmin.ProjectRefRule), "name": str("The credential NAME the server uses (A-Z, 0-9, _)."),
				"purpose": str("One line: what the credential is for, shown to the user."),
				"kind":    str("secret (the user types it), or oauth (the user signs in; name it OAUTH_<SERVER>, upper-case, dashes as underscores).")}, "project", "name", "purpose", "kind")},
	}
}
