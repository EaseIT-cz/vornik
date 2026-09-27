package config

import (
	"encoding/json"
	"errors"
	"fmt"

	"gopkg.in/yaml.v3"
)

// ErrStdioMCPChange refuses a write that would add or alter a stdio MCP
// server. Process-spawn law, reading 3
// (https://docs.vornik.io): a stdio
// server's program comes from the operator's own edits to the config files,
// never from a daemon surface (a web form, a control-plane proposal), because
// the daemon launches that program on the next reload.
var ErrStdioMCPChange = errors.New("stdio MCP servers are registered by editing the config files, not through the daemon")

// StdioMCPChange compares the stdio MCP servers (mcp.servers, in the daemon
// config or a project file) of old and updated file content, and refuses when
// updated adds one or changes what it would execute: command, args, env and
// auth (auth env_from injects environment) all count, since LD_PRELOAD or PATH
// change what runs, and so does any field this guard does not know. Fields
// that do not affect execution (allowed_tools, timeouts, …) may change freely,
// and an entry that names no program at all (a name-only subscription that
// inherits the daemon catalogue's own entry) chooses nothing. Removing a server
// is allowed. Content that does not parse as YAML carries no servers: the
// config loader uses the same parser and refuses it, so nothing launches.
func StdioMCPChange(old, updated []byte) error {
	before := stdioMCPServers(old)
	for name, fp := range stdioMCPServers(updated) {
		if before[name] != fp {
			return fmt.Errorf("%w (server %q)", ErrStdioMCPChange, name)
		}
	}
	return nil
}

// httpMCPTransports never launch a program. Every other transport value,
// including a case variant or an unknown one, is treated as a possible program:
// default-deny, so this guard never depends on how leniently the loader reads
// the value. An empty transport (a name-only subscription inheriting the daemon
// catalogue's entry) is judged by its fields below like any other.
var httpMCPTransports = map[string]bool{"sse": true, "streamable-http": true}

// executionNeutralMCPFields do not change what a stdio server executes.
var executionNeutralMCPFields = map[string]bool{
	"name": true, "transport": true, "url": true, "allowed_tools": true,
	"require_declared_tools": true, "timeout_seconds": true,
}

// stdioMCPServers maps each stdio server that defines a program to a
// fingerprint of its execution-relevant fields. The decode is generic, so a
// field's type cannot make an entry slip past this guard while the typed
// loader accepts it.
func stdioMCPServers(content []byte) map[string]string {
	out := map[string]string{}
	var doc map[string]any
	if len(content) == 0 || yaml.Unmarshal(content, &doc) != nil {
		return out
	}
	mcpSection, _ := doc["mcp"].(map[string]any)
	servers, _ := mcpSection["servers"].([]any)
	seen := map[string]int{}
	for i, raw := range servers {
		entry, ok := raw.(map[string]any)
		if !ok || httpMCPTransports[fmt.Sprint(entry["transport"])] {
			continue
		}
		name := fmt.Sprint(entry["name"])
		if entry["name"] == nil {
			name = fmt.Sprintf("#%d", i)
		}
		relevant := map[string]any{}
		for k, v := range entry {
			if !executionNeutralMCPFields[k] {
				relevant[k] = v
			}
		}
		if len(relevant) == 0 {
			continue // names no program: inherits the daemon catalogue's entry
		}
		fp, _ := json.Marshal(relevant)
		// Keyed by name AND occurrence: with the name alone, a second entry of
		// the same name overwrote the first, so a new program could hide in
		// front of an unchanged copy.
		out[fmt.Sprintf("%s#%d", name, seen[name])] = string(fp)
		seen[name]++
	}
	return out
}
