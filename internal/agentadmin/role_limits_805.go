package agentadmin

// Role limits through define_swarm (part 06 §6.9, GitHub #805).

import "fmt"

// DefaultToolSteps is the agent's tool-iteration default: the entrypoint's
// ${VORNIK_MAX_TOOL_ITERATIONS:-30}, which the executor mirrors as
// defaultAgentToolIterations (a test in internal/executor pins the two).
const DefaultToolSteps = 30

// minRoleTokens is the lowest max_tokens a role may set: below it a reply
// cannot hold one tool call.
const minRoleTokens = 256

// roleLimitsWhy refuses a limit outside its bounds; "" accepts.
func roleLimitsWhy(st *State, ri RoleInput) string {
	if ri.MaxTokens != 0 && (ri.MaxTokens < minRoleTokens || ri.MaxTokens > st.MaxRoleTokensCap) {
		return fmt.Sprintf("max_tokens must be from %d to %d (agent_admin.max_role_tokens; the operator can raise it), or left out for the default",
			minRoleTokens, st.MaxRoleTokensCap)
	}
	if ri.MaxToolIterations < 0 || ri.MaxToolIterations > st.MaxRoleToolIterationsCap {
		return fmt.Sprintf("max_tool_iterations must be from 1 to %d (agent_admin.max_role_tool_iterations; the operator can raise it), or left out for the default %d",
			st.MaxRoleToolIterationsCap, DefaultToolSteps)
	}
	return ""
}

// roleTokenDefault is what the executor gives a role that sets no
// max_tokens: the model's model_limits entry, else the agent LLM's
// max_tokens, else 0 (the provider's own default).
func roleTokenDefault(st *State, model string) int {
	if n := st.ModelMaxTokens[model]; n > 0 {
		return n
	}
	return st.AgentMaxTokens
}

// roleLimitClauses compares each role's limits with the values in force
// (its old explicit value, else the default; a new role compares with the
// default). raises need the user's approval. returns name a limit that was
// set and is now left out: the role goes back to the default, which is
// usually lower; when the default is higher, raises names that too.
func roleLimitClauses(st *State, old, next []RoleSpec) (raises, returns []string) {
	before := map[string]RoleSpec{}
	for _, r := range old {
		before[r.Name] = r
	}
	for _, r := range next {
		prev, had := before[r.Name]
		steps, nowSteps := orDefault(r.MaxToolIterations, DefaultToolSteps), DefaultToolSteps
		if had {
			nowSteps = orDefault(prev.MaxToolIterations, DefaultToolSteps)
		}
		if steps > nowSteps {
			raises = append(raises, fmt.Sprintf("The role %s may use up to %d tool steps (now %d).", r.Name, steps, nowSteps))
		}
		if had && r.MaxToolIterations == 0 && prev.MaxToolIterations > 0 {
			returns = append(returns, fmt.Sprintf("The role %s returns to the default %d tool steps (was %d).", r.Name, DefaultToolSteps, prev.MaxToolIterations))
		}
		def := roleTokenDefault(st, r.Model)
		tokens, nowTokens := orDefault(r.MaxTokens, def), def
		if had {
			nowTokens = orDefault(prev.MaxTokens, roleTokenDefault(st, prev.Model))
		}
		if tokens > nowTokens {
			now := "the provider's default"
			if nowTokens > 0 {
				now = fmt.Sprintf("%d", nowTokens)
			}
			raises = append(raises, fmt.Sprintf("The role %s may write up to %d tokens a reply (now %s).", r.Name, tokens, now))
		}
		if had && r.MaxTokens == 0 && prev.MaxTokens > 0 {
			returns = append(returns, fmt.Sprintf("The role %s returns to the default output length (was %d tokens).", r.Name, prev.MaxTokens))
		}
	}
	return raises, returns
}

func orDefault(v, def int) int {
	if v > 0 {
		return v
	}
	return def
}
