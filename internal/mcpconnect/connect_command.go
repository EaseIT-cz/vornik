package mcpconnect

import "vornik.io/vornik/internal/agentns"

// ConnectCommand is the one remedy for an oauth server with no stored grant,
// shared by every surface that reports it: the daemon's warning, the
// withheld item, the registry error, the doctor and `vornikctl mcp
// oauth-status` (auth design §14 item 5, GitHub #91). projectID "" is the
// daemon scope. An agent-namespace project signs in on the approver device,
// not with vornikctl (review 5fe0 F1).
func ConnectCommand(projectID, server string) string {
	if _, agent := agentns.FromID(projectID); agent {
		return "sign in on the approver device (the assistant's request_credential, kind oauth)"
	}
	cmd := "vornikctl mcp connect " + server
	if projectID != "" {
		cmd += " -p " + projectID
	}
	return cmd
}

// NoGrantState is how every surface names the state (auth design §14).
const NoGrantState = "not connected, no grant"

// NoGrantRemedy is the state with its remedy, as every surface prints it:
// "not connected, no grant: run vornikctl mcp connect x[ -p p]", or the
// approver-device wording for an agent project.
func NoGrantRemedy(projectID, server string) string {
	cmd := ConnectCommand(projectID, server)
	if _, agent := agentns.FromID(projectID); agent {
		return NoGrantState + ": " + cmd
	}
	return NoGrantState + ": run " + cmd
}

// NoGrant is one oauth server withheld for want of a grant (auth design
// §14): ProjectID is the project that declares or subscribes to it ("" for
// the daemon catalog), CredentialScope the scope its grant would be stored
// under.
type NoGrant struct {
	ProjectID       string
	CredentialScope string
	Server          string
}
