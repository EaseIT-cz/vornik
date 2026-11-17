---
hide:
  - navigation
  - toc
---
# Vornik

[![Vornik architecture. Who asks: chat (Telegram, Slack, email), the web console and CLI, a paired phone that approves sensitive actions, AI assistants (Claude Code, Codex, Hermes) and automation (API, webhooks, schedules). Your host holds the trusted, deterministic Vornik control plane: dispatch and queue, workflows, policy and budgets, approvals, memory, the secrets vault, the egress guard, and audit and cost. Below it, untrusted, non-deterministic agent containers, one per task step: no network unless the role opts in, a per-task key, capabilities dropped, and tools and models reached through the daemon over a unix socket. Beside them, the PostgreSQL database for state, memory and audit. Vornik calls models (local or cloud), tools and APIs, and GitHub; the daemon or the gateway adds the keys, and agents get only the secrets you grant. Runs on your hardware; models propose, code decides; secrets sealed and redacted; every step audited.](assets/vornik-architecture.svg)](assets/vornik-architecture.svg)

Vornik is an asynchronous, **local-first** orchestration daemon for teams of AI
agents. You define projects, each backed by a swarm of agents and one or more
workflows; you submit tasks (or let a project create its own); Vornik queues
them, runs each agent in its own isolated container, and persists every result.
Work runs continuously in the background — you submit and walk away, then come
back for the output.

The daemon runs on your own infrastructure. Agents run with **no direct network
egress** by default — the daemon makes the outbound calls — so Vornik suits
data-sensitive and air-gapped environments, including with self-hosted,
open-weight models.

![Models propose, the daemon decides. The models, in a non-deterministic layer, plan, write and choose among their allowed tools. The deterministic layer around them decides tool grants, budgets, output schemas, approvals, network, credentials, redaction and audit.](assets/vornik-control-plane.svg)

A model's output is a proposal. The daemon is ordinary deterministic code,
and it decides whether the proposal takes effect. May the role call that
tool? May the task spend more? Does the output match its schema? Must a
person approve it first? How each check works:
[Security model](security-model.md).

## Start here

- **[Getting Started](getting-started/index.md)** — install the daemon, create
  your first project, and run a task end to end.
- **[Concepts](concepts/index.md)** — projects, swarms, roles, workflows, tasks,
  delegation, and memory — and the [architecture](concepts/architecture.md)
  behind them.

## What it does

![The life of a task in six steps. Ask: from chat, the console, an assistant, a schedule, a webhook or a standing goal. Route: you or the chat dispatcher pick the project and workflow, and the task waits in a durable queue. Run: each step runs a role in its own container; models propose and the daemon checks. Approve, when needed: sensitive actions wait for you on your paired phone. Deliver: results are saved as artifacts and sent back where you asked. Remember: the audit trail, the cost and, if enabled, memory carry into the next task, which is yours or one a project starts itself. A restart loses no work: unfinished tasks are recovered.](assets/vornik-task-cycle.svg)

- **Runs agent teams asynchronously** — durable queues, isolated per-agent
  containers, and structured [recovery](guides/recovery.md) when a step fails.
- **Runs itself when you want it to** — [autonomy](guides/autonomy.md) lets a
  project pursue a standing goal on a schedule, with budgets, rate limits, and
  [approval gates](guides/approvals.md) keeping it in bounds.
- **Keeps work auditable** — the [Autonomy Black Box](features/blackbox.md) gives
  every task a single replayable trace, and the
  [self-healing genome](features/self-healing.md) proposes and trials workflow
  repairs for operator-gated promotion.
- **Learns from its history** — the [instinct layer](features/instinct.md) mines
  past runs into advisory, confidence-scored patterns.
- **Connects to where people work** — Telegram, Slack, email, and
  [GitHub automation](features/forge.md) via the
  [conversation channels](guides/conversation-channels.md), plus a
  [Claude Code companion](features/companion.md).
- **Governs cost and identity** — hard per-project spend caps and dynamic tool
  budgets ([cost and caching](guides/cost-and-caching.md)), a policy-aware
  [memory firewall](features/memory-rag.md), and
  [authentication with role-based access](features/auth.md).

## Find your way around

- **[Features](features/blackbox.md)** — the capabilities above, and how to
  enable them.
- **[Guides](guides/index.md)** — task-focused how-tos for running Vornik day to
  day.
- **[Reference](reference/index.md)** — every [configuration key](reference/configuration.md)
  and [`vornikctl` command](reference/vornikctl.md).
- **[Troubleshooting](troubleshooting/index.md)** — when something won't start or
  a task gets stuck.
