package agentadmin

// WorkspacePatterns are the ways an agent can lay out its projects, served
// by describe_installation as "patterns" so the agent reads them before it
// builds anything (agent-administered design part 24 §24.6 S4, GitHub #920).
// The admin guidance carries the same five, and the tool descriptions of
// create_project, update_project and define_workflow's hand_over point here.
func WorkspacePatterns() []string {
	return []string{
		"One project per area of work, memory sealed. The default: a project's jobs remember for that project only.",
		"Shared memory for jobs on one topic. Set shared_memory on projects whose jobs build on each other's findings, such as a scout and an enrichment job for the same venues; they read and write one memory. The user approves joining it on their phone.",
		"Never share a project that reads private data with one that reaches the web. A mail, calendar, files or banking job stays sealed; what it found reaches you only through its approved result.",
		"Chain jobs with hand_over in define_workflow: one job's finished files start the next job. The user approves each hand-over on their phone. Rule 3 applies to what is handed over.",
		"Your own memory is separate. What you remember lives in your own memory, not in your projects; jobs never read it, and you never read a job's memory, only its approved result.",
	}
}
