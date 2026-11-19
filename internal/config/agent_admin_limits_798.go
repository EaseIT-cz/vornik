package config

// The admin verbs' caps (agent-administered design part 06 §6.9, GitHub
// #798, #805).

// EffectiveMaxConcurrentTasks returns the configured or default cap.
func (a AgentAdminConfig) EffectiveMaxConcurrentTasks() int {
	if a.MaxConcurrentTasks > 0 {
		return a.MaxConcurrentTasks
	}
	return 2
}

// EffectiveMaxRoleTokens returns the configured or default cap.
func (a AgentAdminConfig) EffectiveMaxRoleTokens() int {
	if a.MaxRoleTokens > 0 {
		return a.MaxRoleTokens
	}
	return 32768
}

// EffectiveMaxRoleToolIterations returns the configured or default cap.
func (a AgentAdminConfig) EffectiveMaxRoleToolIterations() int {
	if a.MaxRoleToolIterations > 0 {
		return a.MaxRoleToolIterations
	}
	return 120
}
