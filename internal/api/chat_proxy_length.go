package api

// lengthCutoffOther is the label for a role or model the calling project's
// swarm does not declare.
const lengthCutoffOther = "other"

// lengthCutoffLabels bounds the length cut-off counter's labels to the
// calling project's swarm (GitHub #100; security review 2026-10-08): the
// role only when the swarm declares it, the model only when it is the
// daemon's default model or a role's model or modelFallback in that swarm.
// Anything else, including a request with no or an unknown project, is
// "other". Per project the series count is at most
// (declared roles + 1) x (declared models + default + 1).
// see LLD § https://docs.vornik.io §8.4
func (s *Server) lengthCutoffLabels(projectID, role, model string) (string, string) {
	roleLabel, modelLabel := lengthCutoffOther, lengthCutoffOther
	if s.chatProvider != nil && model != "" && model == s.chatProvider.Model() {
		modelLabel = model
	}
	if projectID == "" || s.projectRegistry == nil {
		return roleLabel, modelLabel
	}
	_, swarm, err := s.projectRegistry.GetProjectWithSwarm(projectID)
	if err != nil || swarm == nil {
		return roleLabel, modelLabel
	}
	for _, sr := range swarm.Roles {
		if role != "" && sr.Name == role {
			roleLabel = role
		}
		if model != "" && (sr.Model == model || sr.ModelFallback == model) {
			modelLabel = model
		}
	}
	return roleLabel, modelLabel
}
