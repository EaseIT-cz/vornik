package service

// memoryProjectFor is the project whose memory projectID's tasks read and
// write: <ns>--home for an agent project that opted in with
// memory.shared_with_namespace, the project itself otherwise
// (agent-administered design part 24 §24.6, GitHub #920). It reads the live
// registry, so a reload that changes the flag applies at the next ingest or
// search; an unknown project keeps its own memory.
func (c *Container) memoryProjectFor(projectID string) string {
	if c == nil || c.Registry == nil {
		return projectID
	}
	p := c.Registry.GetProject(projectID)
	if p == nil {
		return projectID
	}
	return p.MemoryProject()
}
