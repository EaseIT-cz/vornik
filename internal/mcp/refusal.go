package mcp

// Refusal is one MCP server the daemon refused to register, and why (GitHub
// #625 item 5; agent-administered design, "Follow-up 2026-11-12 (#625 item
// 5)"). The service records one per refusal in its daemon and project passes;
// the control-plane hub lists them and the doctor reads the project ones.
type Refusal struct {
	// Scope is "" for the daemon catalogue (config.yaml mcp.servers), else
	// the id of the project whose entry was refused.
	Scope string
	// Server is the server's configured name.
	Server string
	// Reason is the refusal in words. It names configuration fields and
	// secret names, never a secret's value.
	Reason string
}
