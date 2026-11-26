package agentns

// MemoryProject is the project whose memory projectID's tasks read and write
// (agent-administered design part 24 §24.6, GitHub #920): an agent
// namespace's project that opted in (shared) uses the namespace's home,
// <ns>--home; every other project, sealed or not an agent project, keeps
// its own.
func MemoryProject(projectID string, shared bool) string {
	ns, ok := FromID(projectID)
	if !ok || !shared {
		return projectID
	}
	return ID(ns, "home")
}
