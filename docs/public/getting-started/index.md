---
sources:
    - path: README.md
      sha256: d396d9a5b9ce1c6f9aed484597e3df60ad187e11bf9b09f02afd846569660b47
    - path: deployments/macos/install.sh
      sha256: 60ac9e590a844f509806d50ff2427d4aec748cbd35b015f9b4f65aad9b29a844
---
# Getting Started

This page takes you from a fresh machine to a running vornik daemon with a
first project, then runs one task end to end. It assumes no vornik
knowledge; [Concepts](../concepts/index.md) explains each term.

## What vornik does

vornik is an asynchronous orchestration daemon. You define **projects**.
Each runs on a **swarm** of agents and one or more **workflows**. You submit
**tasks**: vornik queues them, starts the agents in isolated local
containers, runs them through the workflow, and keeps the results. Work
runs continuously in the background: you submit and walk away, then come
back for the output.

You drive it from the CLI, the HTTP API, the Web UI, a connected
conversation channel (Telegram, Slack, e-mail or web chat), or another AI
assistant connected as a companion. Recent releases add operability and
resilience features that later pages cover: a one-command redacted support
bundle, expanded `vornikctl doctor` checks, centralised log forwarding,
structured recovery for failed steps, a bounded graceful shutdown, approvals
on a paired phone, and the controls in the
[security model](../security-model.md). You need none of them to start: the
defaults are safe and local-first.

## Prerequisites

- **Go 1.26 or later**, to build the daemon and CLI from source.
- **Podman**, rootless recommended. vornik runs every agent in its own
  Podman container. Docker is not used.
- **PostgreSQL with [pgvector](https://github.com/pgvector/pgvector).**
  vornik keeps all durable state (queues, tasks, executions, artifacts,
  memory) in PostgreSQL, so work survives restarts. pgvector backs the
  memory (RAG) semantic search; the `pgvector/pgvector` image ships it, and
  vornik enables the extension on first boot. SQLite runs the orchestration
  core but has no vector search, so memory falls back to keyword search only
  (see
  [Memory & RAG](../features/memory-rag.md)).
- **Make**, for the build targets.

!!! note
    vornik is **local-first**: the daemon and every agent container run on
    the same host. There is no cloud service to sign up for.

## A 60-second playground

!!! tip "Preferred: let your coding agent install it"
    If you use Claude Code, Codex, or a Hermes agent with shell access, paste
    this into it, and it does everything on this page:

    > Set up Vornik for me. Follow the runbook at https://agents.vornik.io

    [AGENTS.md](https://github.com/EaseIT-cz/vornik/blob/main/AGENTS.md) is a
    runbook written for agents. It installs (with the same `get.vornik.io`
    script as below), sets up your LLM key, runs a first task, and gives the
    agent its own companion project, with persistent RAG memory and a
    knowledge-skill store for procedures it captures and reuses across
    sessions and projects. Every step has a check, and the agent asks you
    before anything privileged.

    Other agents (Gemini CLI, opencode and others) can use Vornik's memory
    and delegation once it runs: see
    [Use Vornik from any agent](../guides/any-agent.md).

To run it yourself, one command sets up a complete playground on a Linux
host. On macOS it first provisions a small Linux VM
([below](#macos-inside-a-linux-vm)). On Linux it:

- installs any missing prerequisites;
- builds the daemon and CLI in a throwaway container (no Go toolchain
  needed);
- starts PostgreSQL with pgvector in a container;
- runs the daemon as a rootless `systemctl --user` service that starts
  agent containers through your rootless Podman.

```bash
curl -fsSL https://get.vornik.io | bash
```

The script pins its own release tag, not a moving branch; set
`VORNIK_REF=main` for the latest code. To check the script against its
published checksum before running it (this catches tampering in transit or
through a redirect; it is not a signature), fetch both first:

```bash
REF=<release>  # a tag from github.com/EaseIT-cz/vornik/releases that ships quickstart.sh.sha256
base="https://raw.githubusercontent.com/EaseIT-cz/vornik/$REF/deployments/podman"
curl -fsSLO "$base/quickstart.sh" && curl -fsSLO "$base/quickstart.sh.sha256"
sha256sum -c quickstart.sh.sha256 && VORNIK_REF="$REF" bash quickstart.sh
```

The daemon creates and migrates its own schema on first boot; the script
provisions the empty database. When it finishes, open
<http://localhost:8080/ui> and log in with the API key the installer
generated (`VORNIK_API_KEY` in `~/.config/vornik/secrets/api.env`; every
request needs it). A first-run **setup guide** walks you through connecting
an LLM endpoint and key (with a live connection test), optional memory
(RAG), and your first project.
From the terminal instead: set `VORNIK_CHAT_API_KEY` in
`~/.config/vornik/vornik.env` and run `systemctl --user restart vornik`. The
[Podman deployment notes](https://github.com/EaseIT-cz/vornik/tree/main/deployments/podman)
cover the tunables and the runtime model.

### macOS (inside a Linux VM)

macOS has no native container runtime, and vornik's zero-egress agent
isolation needs Linux: a container with no network reaches the daemon over
a bind-mounted unix socket. To keep that isolation exactly as on Linux, the
installer runs the same stack in a lightweight [Lima](https://lima-vm.io)
VM. The one-liner above detects macOS and does it for you; there is no macOS
variant to remember:

- it installs Lima with Homebrew;
- creates a `vornik` VM (Apple Silicon first; Intel is best effort);
- runs the standard install inside it;
- forwards the UI and API to <http://localhost:8080/ui> on your Mac.

The installed `vornikctl` shim runs against the VM:

```bash
vornikctl status          # daemon health inside the VM
vornikctl logs            # daemon logs
vornikctl backup out.tar  # snapshot (DB via pg_dump + config) to a Mac path
vornikctl delete --force  # destroy the VM; run backup first, this erases all data
```

Your data, PostgreSQL included, lives **inside the VM**, for durability; carry it across a
VM rebuild with `vornikctl backup` and `restore`. Only `~/.config/vornik` is
shared from the Mac: edit it there, then run `vornikctl reload`. Any local
Mac process can reach the daemon's API through the port forward, which is
fine for a single operator.

That is the fastest way to a working instance. The rest of this page builds
the same thing by hand, so you see each piece.

## Install

Build the two binaries from source:

```bash
# The daemon
go build -o bin/vornik ./cmd/vornik

# The control CLI
go build -o bin/vornikctl ./cmd/vornikctl
```

`make build` produces the same binaries. Put `bin/` on your `PATH` (or call
the binaries by path); the rest of this page assumes `vornik` and
`vornikctl` are on it. Confirm the build:

```bash
vornik --version
vornikctl version
```

`vornik --version` reports the build you are running. A build from the
latest `main` can report a newer version than the latest tagged release. The [release notes](../release-notes/index.md) say what each
release adds.

## Point vornik at a database

vornik reads its database connection from its configuration or from
environment variables. The quickest way is environment variables:

```bash
export VORNIK_DATABASE_HOST=localhost
export VORNIK_DATABASE_PORT=5432
export VORNIK_DATABASE_NAME=vornik
export VORNIK_DATABASE_USER=vornik
export VORNIK_DATABASE_PASSWORD=vornik
```

vornik creates and migrates its schema on first start: give it an empty
database, and do not load a schema dump. Use a PostgreSQL image with
**pgvector** (for example `pgvector/pgvector`) so memory search works; vornik
enables the extension during that first migration. Every setting, and its
YAML form, is in the [configuration reference](../reference/configuration.md).

## Start the daemon

```bash
vornik
```

With no flags, vornik reads its configuration from the first of: the
`--config` flag, the `VORNIK_CONFIG` variable, `./vornik.yaml` (or
`./config.yaml`), `~/.config/vornik/`, and `/etc/vornik/vornik.yaml`. To name
a file:

```bash
vornik --config /path/to/vornik.yaml
```

The daemon serves the Web UI and the HTTP API, on `:8080` by default. Check
that it is ready before you submit work:

```bash
curl http://localhost:8080/readyz
# {"status":"ready","checks":[{"name":"database","status":"ok"}]}
```

A `200` means every dependency is reachable. A `503` names the failing
check; [Troubleshooting](../troubleshooting/index.md) covers each.

## Create your first swarm

A project needs a swarm. The fastest way is a built-in preset. The
`research` preset (a lead, a researcher and a writer) fits digest, scan and
summary work:

```bash
vornikctl init swarm my-swarm --template research
```

`vornikctl init swarm --list` lists every preset, one line each. The command
writes a `SWARM.md` into `configs/swarms/` and checks it against the
registry before saving.

## Create your first project

Create a project on that swarm and the built-in `adaptive` workflow:

```bash
vornikctl init project my-project \
  --display-name "My First Project" \
  --swarm my-swarm \
  --workflow adaptive
```

Pass `--dry-run` first to preview the generated YAML without writing it. The
command validates the project against the registry, and refuses one that
names a swarm or workflow that does not exist.

!!! note
    You can also create projects in the Web UI: `/ui/projects/new` has a
    gallery of templates, and `/ui/projects/new/wizard` is a conversational
    wizard. The wizard combines a base template with typed add-ons (MCP
    servers, a schedule, secrets, tools, role instructions) and builds the
    project, swarm and workflow for you.

    A project runs **one** autonomy style. Describe a need with two
    cadences, say "ingest my mail every 5 minutes **and** produce a daily
    digest", and the wizard builds one scheduled loop that does both, rather
    than two conflicting schedules: it ingests on every tick and produces
    the digest once the day has passed. Set
    `chat.wizard_model` to a capable model (a strong Bedrock chat model
    works well) for better drafts; the wizard reaches a valid project with
    any model.

Confirm the daemon loaded it:

```bash
vornikctl project list
vornikctl project show my-project
```

Every project, however you made it, has a readiness checklist at
`/ui/projects/{id}/setup`: config, secrets, MCP reachability, chat model,
schedule, and an optional smoke test. The project list flags a project with
unmet required checks.

## Submit your first task

`--prompt` is the simplest way to describe the work:

```bash
vornikctl task submit \
  --project my-project \
  --prompt "Summarise the key themes in the attached notes."
```

It returns a task ID: vornik has queued the task. The scheduler picks it up,
starts the agents and runs them through the project's workflow. Useful options:

- `--priority <0-100>`: lower runs first; `0` uses the project's default.
- `--workflow <id>`: run on a workflow other than the project's default.
- `--task-type <label>`: a free-form label shown in the task list.
- `--attach <file>`: attach an input file. It is snapshotted and extracted
  into project memory, so the agents can read it.

```bash
vornikctl task submit -p my-project \
  --prompt "Review this report and list open risks." \
  --attach report.pdf \
  --priority 10
```

## Watch it run

List a project's tasks, optionally by status:

```bash
vornikctl task list --project my-project
vornikctl task list --project my-project --status RUNNING
```

Show one task: its status, workflow and timestamps:

```bash
vornikctl task get <taskId> --project my-project
```

Follow its logs while it runs, or read the result excerpt once it has
finished:

```bash
vornikctl task tail <taskId> --project my-project --follow
```

A task moves through `PENDING`, `QUEUED` and `RUNNING`, and ends `COMPLETED`,
`FAILED` or `CANCELLED`. Files the agents wrote as outputs are kept as
**artifacts**, shown in the API and the Web UI.

## Where to go next

- **[Concepts](../concepts/index.md)**: projects, swarms, workflows, tasks,
  delegation and memory, before you customise.
- **[Guides](../guides/index.md)**: how-tos, including
  [workflows and LLM controls](../guides/workflows-and-llm-controls.md) and
  [conversation channels](../guides/conversation-channels.md).
- **[Companion](../features/companion.md)** and
  **[Knowledge skills](../features/knowledge-skills.md)**: connect your own
  coding agent, and teach it reusable procedures that reach swarm roles (and
  every project, when marked global).
- **[Configuration reference](../reference/configuration.md)** and the
  **[vornikctl reference](../reference/vornikctl.md)**: every setting and
  command.
- **[Troubleshooting](../troubleshooting/index.md)**: when something does
  not start or a task gets stuck.
