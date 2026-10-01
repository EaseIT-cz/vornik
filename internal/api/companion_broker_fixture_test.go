package api

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/registry"
)

// seedBrokerRegistry builds the two-project shape of the broker design
// (https://docs.vornik.io
// §8): broker-acme holds the mail server and runs mail-digest; memory-acme
// holds nothing. wf-plain is an ordinary companion workflow.
func seedBrokerRegistry(t *testing.T) *registry.Registry {
	t.Helper()
	root := t.TempDir()
	for _, d := range []string{"projects", "swarms", "workflows"} {
		require.NoError(t, os.MkdirAll(filepath.Join(root, d), 0o755))
	}
	write := func(rel, body string) {
		require.NoError(t, os.WriteFile(filepath.Join(root, rel), []byte(body), 0o644))
	}
	write("swarms/swarm.md", `---
swarmId: swarm-broker
roles:
  - name: reader
    runtime:
      image: test-image
    permissions:
      allowedTools: [file_write, mcp__gmail__search_messages]
  - name: worker
    runtime:
      image: test-image
---
`)
	write("workflows/mail-digest.md", `---
workflowId: mail-digest
description: Digest of recent mail, no bodies.
entrypoint: read
broker:
  input_schema:
    type: object
    additionalProperties: false
    required: [since]
    properties:
      since:      { type: string, format: date-time }
      importance: { enum: [all, high] }
      topic:      { type: string, maxLength: 120, x-untrusted: true }
  egress:
    output: digest.json
    max_bytes: 4096
    schema:
      type: object
      required: [items]
      properties:
        items:
          type: array
          maxItems: 30
          items:
            type: object
            properties:
              one_line: { type: string, maxLength: 200 }
steps:
  read:
    type: agent
    prompt: "Digest the mail described by the broker inputs."
    role: reader
    on_success: done
terminals:
  done:
    status: COMPLETED
---
`)
	// bad-broker is a valid broker block run by a role with no allowedTools
	// (unrestricted), so CheckBrokerRunnable must refuse it at delegate.
	write("workflows/bad-broker.md", `---
workflowId: bad-broker
entrypoint: run
broker:
  input_schema: { type: object, additionalProperties: false, properties: {} }
  egress: { output: out.json, schema: { type: object } }
steps:
  run:
    type: agent
    prompt: "x"
    role: worker
    on_success: done
terminals:
  done:
    status: COMPLETED
---
`)
	write("workflows/mail-reply.md", `---
workflowId: mail-reply
entrypoint: read
broker:
  input_schema:
    type: object
    additionalProperties: false
    properties:
      intent: { enum: [accept, decline] }
  egress: { output: summary.json, schema: { type: object } }
  proposes:
    - action: gmail_reply
      tool: mcp__gmail-write__gmail_send
      output: proposal.json
      args_schema:
        type: object
        additionalProperties: false
        required: [to, body]
        properties:
          to:   { type: string, format: email, maxLength: 254 }
          body: { type: string, maxLength: 2000, x-untrusted: true }
steps:
  read:
    type: agent
    prompt: "Draft the reply."
    role: reader
    on_success: done
terminals:
  done:
    status: COMPLETED
---
`)
	write("workflows/wf-plain.md", `---
workflowId: wf-plain
entrypoint: run
steps:
  run:
    type: agent
    prompt: "plain work"
    role: worker
    on_success: done
terminals:
  done:
    status: COMPLETED
---
`)
	write("projects/broker-acme.yaml", `
projectId: broker-acme
displayName: Broker
swarmId: swarm-broker
defaultWorkflowId: mail-digest
defaultPriority: 50
broker: true
mcp:
  servers:
    - name: gmail
      transport: sse
      url: http://gmail.invalid/sse
      broker_read_only: true
      allowed_tools: [search_messages]
    - name: gmail-write
      transport: sse
      url: http://gmail.invalid/sse
      broker_write: true
      allowed_tools: [gmail_send]
`)
	write("projects/memory-acme.yaml", `
projectId: memory-acme
displayName: Memory
swarmId: swarm-broker
defaultWorkflowId: wf-plain
defaultPriority: 50
`)
	reg := registry.New()
	require.NoError(t, reg.Load(root))
	return reg
}
