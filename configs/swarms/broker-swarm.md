---
swarmId: broker-swarm
displayName: Broker swarm (read-only mail reader)
description: >-
  Minimal swarm for a broker project: one role that reads mail through
  read-only tools and writes a bounded digest. See the privileged-work broker
  design and configs/examples/broker-mail.yaml.
leadRole: mail-reader
roles:
    - name: "mail-reader"
      description: "Reads recent mail with read-only tools and writes a bounded digest"
      count: 1
      runtimePolicy: "ephemeral"
      # Open-weight by default: the broker exists to keep sensitive work on
      # infrastructure you control. Point this at a local model if you have one.
      model: "zai.glm-5"
      maxTokens: 4096
      runtime:
        image: "ghcr.io/grinco/vornik-agent:latest"
        cpu: "1"
        memory: "2Gi"
      permissions:
        # Every tool must be broker-safe: workspace/clock built-ins, or an
        # MCP tool from a server the broker project declares broker_read_only
        # and lists in that server's allowed_tools. CheckBrokerRunnable
        # refuses the workflow at delegate time otherwise.
        allowedTools:
          - "current_time"
          - "file_write"
          - "mcp__google-workspace__time_getCurrentDate"
          - "mcp__google-workspace__gmail_search"
          - "mcp__google-workspace__gmail_get"
---

# Broker swarm

One role, `mail-reader`, for the `mail-digest` broker workflow.
