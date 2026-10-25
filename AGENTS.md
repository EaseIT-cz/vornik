# AGENTS.md — Vornik for AI agents

You are an AI agent (Claude Code, Codex, Gemini CLI, …). This file is
written for you, in two parts:

- **Part 1 — using Vornik** (Runbooks A, B, D): your user asked you to set up
  Vornik, connect it to an LLM, wire yourself in, or update an install. Every
  step is a command plus a check. Run the command, verify the check, and only
  then continue. If a check fails, stop and triage with the step's hints — do
  not push on past a failed check.
- **Part 2 — changing the code** (§1–§8): the operative rules for anyone,
  human or agent, changing this codebase. `CLAUDE.md` imports this file, so
  Claude Code loads the same rules.

## Trust & safety (read first)

- During install, Vornik doesn't send your data to us or any third party.
  Secrets are written to `~/.config/vornik/secrets/` and never transmitted.
- Setup talks to your local daemon at `http://localhost:8080`. The outbound
  calls are ones you trigger yourself: validating your chat/LLM key against
  the endpoint you entered, and — if you enable memory/RAG — validating your
  embedding key against that endpoint. Never any Vornik service.
- The installer is open source — review `deployments/podman/quickstart.sh` before running.

### Install (reviewable)

```bash
curl -fsSL https://get.vornik.io -o quickstart.sh
less quickstart.sh          # review — nothing leaves your machine
bash quickstart.sh
```

(One-liner `curl -fsSL https://get.vornik.io | bash` is the convenience option.)
Note: the two-step path depends on `get.vornik.io` serving the current script;
the in-repo `deployments/podman/quickstart.sh` is the reviewable source of truth.

### If a task fails after install

1. `vornikctl doctor` — checks agent-LLM topology, workspace uid, upstream key, and more.
2. `vornikctl report` — opens a pre-anonymized GitHub issue you review and submit.

### Upgrading from an install before this release

The installer seeds `~/.config/vornik/configs/` only on first install (when
it's empty) and preserves your existing configs on later runs — so an older
`~/.config/vornik/configs/projects/dev-project.yaml` keeps `autonomy.enabled: true`.
If you don't want unattended scheduling, set it to `false` (or toggle it off
in the UI). `vornikctl doctor` (autonomy_budget_guard) flags autonomy-enabled
projects.

**Ground rules**

1. **Ask before privileged commands.** Package installs (`sudo dnf/apt/…`)
   and `loginctl enable-linger` change the system — surface them to your
   user before running. Everything else below is user-scoped.
2. **Never echo or commit secrets.** LLM API keys and `sk-vornik-*` bearer
   tokens go only into the files named below, never into chat output, shell
   history you display, or git.
3. **Target platform is Linux with rootless Podman.** On macOS/Windows,
   stop and point your user at `docs/public/getting-started.md`.

---

## Runbook A — zero → hello world

### A1. Install the stack

One command builds the daemon + CLI (in an ephemeral container — no Go
toolchain needed), starts PostgreSQL + pgvector in a container, and runs the
daemon as a rootless `systemctl --user` service. It may `sudo`-install
missing prerequisites — warn your user first (rule 1):

```bash
# Recommended for agents — verify the installer's checksum before running it:
REF=<release>  # a tag from github.com/EaseIT-cz/vornik/releases that ships quickstart.sh.sha256
base="https://raw.githubusercontent.com/EaseIT-cz/vornik/$REF/deployments/podman"
curl -fsSLO "$base/quickstart.sh" && curl -fsSLO "$base/quickstart.sh.sha256"
sha256sum -c quickstart.sh.sha256 && VORNIK_REF="$REF" bash quickstart.sh

# Or the convenience one-liner (pins the release tag baked into the script;
# set VORNIK_REF=main for bleeding-edge):
curl -fsSL https://get.vornik.io | bash
# Already inside a Vornik checkout? Use it as the build context instead:
VORNIK_SKIP_FETCH=1 VORNIK_DIR="$PWD" bash deployments/podman/quickstart.sh
```

**Check:** the script ends with `Vornik is up and ready.`, and:

```bash
curl -fsS http://localhost:8080/readyz
# → {"status":"ready","checks":[{"name":"database","status":"ok"}]}
```

**If not:** `journalctl --user -u vornik -e` — the loader names any config
placeholder that is empty. Triage table: `deployments/podman/README.md`.

Binaries land in `~/.local/bin` (`vornik`, `vornikctl`). If `vornikctl` is
not found, prefix commands with `~/.local/bin/` or fix `PATH`.

### A2. Connect an LLM (the setup API)

The first-run web guide at `http://localhost:8080/ui/setup` and this API
are the same flow; as an agent, drive the API. Ask your user for their
OpenAI-compatible endpoint, model ID, and API key (rule 2), then:

```bash
SID=$(curl -fsS -X POST http://localhost:8080/api/v1/setup/session \
  -H 'Content-Type: application/json' -d '{}' | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')

# Validate first — this makes a live call to the provider:
curl -fsS -X POST "http://localhost:8080/api/v1/setup/session/$SID/validate" \
  -H 'Content-Type: application/json' \
  -d '{"endpoint":"<ENDPOINT>","api_key":"<KEY>","model":"<MODEL>"}'
```

**Check:** the response reports the endpoint and model reachable. Fix
endpoint/key/model until it does — committing a broken config wastes a
restart.

```bash
# Commit chat, then resolve the memory step (disabled is a supported,
# sensible default — enable later from /ui/setup with an embedding endpoint):
curl -fsS -X POST "http://localhost:8080/api/v1/setup/session/$SID/commit" \
  -H 'Content-Type: application/json' \
  -d '{"endpoint":"<ENDPOINT>","api_key":"<KEY>","model":"<MODEL>"}'
curl -fsS -X POST "http://localhost:8080/api/v1/setup/session/$SID/memory/commit" \
  -H 'Content-Type: application/json' -d '{"enabled":false}'
```

This writes the key to `~/.config/vornik/secrets/chat.env` (the daemon
sources that directory itself on start) and patches
`~/.config/vornik/config.yaml`. Manual fallback: set `VORNIK_CHAT_API_KEY`,
`CHAT_ENDPOINT`, `CHAT_MODEL` in `~/.config/vornik/vornik.env`.

### A3. First project + dispatcher

```bash
vornikctl init project --template personal-assistant assistant \
  --config-dir "$HOME/.config/vornik/configs"
vornikctl config reload    # loads the new project into the running registry
curl -fsS -X POST http://localhost:8080/api/v1/setup/dispatcher \
  -H 'Content-Type: application/json' -d '{"project_id":"assistant"}'
```

**Check:** `vornikctl project list` shows `assistant`, and the dispatcher
commit returns `"committed":true`.

### A4. One restart, then verify

Config from A2/A3 activates at process start:

```bash
systemctl --user restart vornik
curl -fsS http://localhost:8080/readyz        # ready again (poll ~30s)
vornikctl doctor                              # feature readiness incl. chat
curl -fsS http://localhost:8080/api/v1/setup/status
# → "fresh_install":false — onboarding is complete
```

### A5. Hello world

```bash
vornikctl task submit -p assistant --prompt "Say hello and introduce yourself."
# Poll the returned task ID (first run pulls/warms the agent container — allow a few minutes):
vornikctl task get <taskId> -p assistant
vornikctl task tail <taskId> -p assistant
```

**Check:** the task reaches `COMPLETED` and `task get` shows the agent's
answer. Show your user the result and the chat UI:
`http://localhost:8080/ui/projects/assistant/chat`.

---

## Runbook B — wire yourself in (companion + RAG)

Vornik can serve **you**: an MCP endpoint for delegating async work to
swarm agents and a per-project RAG memory you can `remember`/`recall`
across sessions. Set it up for yourself after Runbook A.

### B1. Capability check

```bash
curl -fsS http://localhost:8080/api/v1/capabilities
```

**Check:** `features.companion-v1` and `features.companion-mcp` are `true`.

### B2. Companion project + scoped key

```bash
vornikctl init project --template companion "companion-$USER" \
  --config-dir "$HOME/.config/vornik/configs"
vornikctl config reload

# Mint a key scoped to YOUR CLI. Native companion plugins exist for Claude Code
# and Codex only, so set --client to match: claude-code OR codex. If you are
# Codex, also pass --repo-scope=<canonical git remote, e.g. github.com/ORG/REPO>:
# Codex has no SessionStart scope injector, so the key default backstops any
# memory call that omits repo_scope (keeps chunks from landing NULL-scoped).
vornikctl companion grant \
  --project="companion-$USER" \
  --client=claude-code \
  --label="$(hostname)/$USER" \
  --workflows=companion-architectural-review,companion-doc-review,companion-research-gather,companion-report-summarize,companion-rag-ingest \
  --budget-usd=25 \
  --memory-all \
  --skill-all
```

`--memory-all` grants RAG `remember`/`recall`; `--skill-all` grants the
knowledge-skill store (`skill_read`/`write`/`admin`) — both are the
recommended default for a companion project so you can capture reusable
procedures as **knowledge skills** and approve them without a second grant.
Drop `--skill-admin` (use `--skill-read --skill-write`) if you want proposals
to require a separate operator approval; drop `--skill-all` entirely for a
delegate-only key.

**Check:** the grant prints a `sk-vornik-…` secret — it is shown once.
Store it per rule 2 (next step), never in your transcript.

### B3. Install the plugin (Claude Code / Codex only)

Native companion plugins are packaged for **Claude Code** and **Codex** only.
First export the connection env in your shell profile (both clients):

```bash
export VORNIK_URL="http://localhost:8080"
export VORNIK_COMPANION_TOKEN="<the sk-vornik-… secret>"
```

**Claude Code** — the plugin ships in this repo (`contrib/claude-code-companion`).
Inside the CLI:

```
/plugin marketplace add <path-to-this-checkout>
/plugin install vornik-companion@vornik
```

Full options and MCP wiring: `contrib/claude-code-companion/README.md`.

**Codex** — the adapter ships in `contrib/codex-companion`. Register the
companion MCP endpoint:

```bash
codex mcp add vornik \
  --url http://localhost:8080/api/v1/mcp/companion \
  --bearer-token-env-var VORNIK_COMPANION_TOKEN
```

Codex has no slash-command / SessionStart layer, so there is no `/plugin`
step and no auto session digest — call the `mcp__vornik__*` tools directly
(`catalog`, `recall`, `delegate`, `remember`, …). Full options:
`contrib/codex-companion/README.md`.

**Any other CLI** (Gemini CLI, opencode, …): there is **no** native companion
plugin — packaging covers Claude Code and Codex only. Skip this step and drive
the companion over plain HTTP (B4); the `remember` / `recall` / `delegate`
methods all work as stateless JSON-RPC, so every CLI can use them without a
plugin.

### B4. RAG round-trip smoke test

You can verify the memory path over plain HTTP before a session reload —
the plugin's MCP endpoint is stateless JSON-RPC:

```bash
MCP="$VORNIK_URL/api/v1/mcp/companion"
AUTH="Authorization: Bearer $VORNIK_COMPANION_TOKEN"

# remember a canary…
curl -fsS -X POST "$MCP" -H "$AUTH" -H 'Content-Type: application/json' -d '{
  "jsonrpc":"2.0","id":1,"method":"tools/call",
  "params":{"name":"remember","arguments":{
    "content":"AGENTS.md smoke canary: companion RAG round-trip works on this install.",
    "tags":["agents-md-smoke"]}}}'

# …recall it (embedding is async — if the first recall misses, wait ~5s and retry once)
curl -fsS -X POST "$MCP" -H "$AUTH" -H 'Content-Type: application/json' -d '{
  "jsonrpc":"2.0","id":2,"method":"tools/call",
  "params":{"name":"recall","arguments":{"query":"AGENTS.md smoke canary"}}}'
```

**Check:** remember returns `"decision":"ALLOW"`; recall's hits include the
canary. Clean up after yourself so the canary never pollutes real recalls:

```bash
curl -fsS -X POST "$MCP" -H "$AUTH" -H 'Content-Type: application/json' -d '{
  "jsonrpc":"2.0","id":3,"method":"tools/call",
  "params":{"name":"memory_correct","arguments":{
    "wrong_claim":"AGENTS.md smoke canary: companion RAG round-trip works on this install.",
    "max_refutes":1}}}'
```

Once wired in, start a fresh session. On **Claude Code** the plugin's
SessionStart digest lists finished delegations and recent project memory, and
`recall`/`remember`/`delegate` are available as native tools. On **Codex** the
`mcp__vornik__*` tools are available natively (no session digest — call
`recent_memory` yourself to catch up). On any **other CLI**, keep using the
HTTP calls above.

### B5. Capture a knowledge skill (with `--skill-all`)

RAG memory is for **facts**; a **knowledge skill** is a reusable **procedure**
the agent should apply — a troubleshooting flow, a deploy sequence, a
non-obvious fix. Once an operator approves it, vornik injects it into swarm
roles AND serves it to every companion client, so you write it once and it's
available everywhere.

On **Claude Code**: `/skill-propose`, `/skill-search`, `/skill-approve`,
`/skill-set-global`. On **Codex**: the `skill_propose` / `skill_search` /
`skill_get` / `skill_set_global` MCP tools (see the `knowledge` skill). A
proposal lands as a `draft` and does not fire until approved (`skill_admin`).

**Cross-project (global) skills.** By default a skill only injects into its
home project. Mark it **global** — `skill_propose global:true`, or
`/skill-set-global <id>` / `vornikctl knowledge set-global <id>` — and it
injects into **every** project's roles, so a procedure captured in your
`companion-$USER` project reaches the autonomy roles (e.g. `family`,
`assistant`) too. Global drafts are labelled "affects ALL projects" wherever
they're reviewed.

---

## Runbook D — updating an existing install

For a host installed via Runbook A, **upgrade in place** with the update
script — do NOT re-run the install one-liner to update. It rebuilds the
binaries in the same ephemeral golang container the quickstart uses (no host
Go), takes a DB dump + binary/config backup first, and prints exact rollback
commands.

```bash
cd ~/vornik/deployments/podman
./vornik-update.sh --check                  # is a newer tag available? (changes nothing)
./vornik-update.sh                          # upgrade to the newest tag (asks to confirm)
./vornik-update.sh --ref origin/main        # or track the tip of main
./vornik-update.sh --yes                    # skip the prompt (automation)
```

**Check:** the run ends with `Upgrade complete.` and prints `/readyz : ready`
plus the `DB migr : vN -> vM` bump.

```bash
curl -fsS http://localhost:8080/readyz      # ready again (poll ~30s)
vornikctl doctor
```

**If not:** the script prints ROLLBACK commands (restore the `*.prev` binaries
from `~/vornik-upgrade-backup-<UTC>/` and `git checkout` the prior commit) and
tails the journal. Full step-by-step + the optional daily "update available"
timer: `deployments/podman/UPDATING.md`.

**Ground rule 1 still applies:** the cutover restarts the service (interrupting
any running task); the script warns first unless you pass `--yes`.

---

# Part 2 — Changing the code

Operative rules for anyone — human or agent — changing this codebase. They are
stated here, in full, rather than linked, because every one of them has been
carried as a pointer that was re-injected into context, read, and not followed.
A rule behind a link is a rule the reader chooses; that choice is what this file
removes.

**Build, test, lint** (the commands; §2 says which runs are required before
claiming anything works):

```bash
make build             # go build (daemon + CLI)
make test              # unit + agent shell + script tests
make test-integration  # needs PostgreSQL with pgvector; the Postgres lane
make lint              # golangci-lint + the LLD contract checks + shellcheck
```

Start with `https://docs.vornik.io` and `https://docs.vornik.io` for how the
pieces fit; user-facing docs live in `docs/public/`.

## 1. Design before code

**Recall the subsystem's design before reading or editing its code, and again
when the subsystem changes.** The designs in `https://docs.vornik.io` are the
authoritative record; code confirms what a named file, flag or function still
is, because a design freezes facts at write time.

Then one of three things is true, and each has an obligation:

| the design | you |
|---|---|
| is silent on what you are about to do | add it |
| contradicts what shipped | correct it, dated, and mark the stale sections inline |
| does not exist for this surface | write one |

**An undesigned surface is designed before it is changed, in this order:**

1. **As-built first.** Write the low-level design of what the surface does
   TODAY, reverse-engineered from the code, its configuration and its
   observed behaviour (measured where it can be: run times, sizes, counts),
   not from memory or intent. Say what was measured and what was inferred.
2. **Review the as-built record** until GREEN. A wrong picture of the
   present makes every proposal built on it wrong.
3. **Then the change**, as a dated section of the same design: what moves,
   why, what it costs, and what it gives up. Review it until GREEN.
4. **Then TDD**: the failing test, the change, the test passing, the
   design amended with what actually shipped.

Never fold the as-built record and the proposal into one unreviewed draft,
and never let "it is only configuration" (a workflow, a Makefile, a script)
excuse a surface from this: those are where undesigned behaviour lives
longest.

**Touch a surface a design covers → amend that design in the same change.**
Never a second document about one surface: retrieval serves whichever chunk
ranks highest, and neither document says it is the incomplete one.

The failure this prevents is not hypothetical and not rare. It is skipped most
often on changes that look contained — "one line", "obviously right" — and those
are the ones where the design turns out to disagree.

*(In the public Community repository `https://docs.vornik.io` is not present;
the rule still holds for whatever design record that fork keeps.)*

## 2. Verify at CI scope, not at "it compiles"

Before claiming anything works:

```
go test ./...                 # the sqlite lane
make lint                     # golangci-lint + the LLD contract checks + shellcheck
make test                     # unit + agent shell + script tests
```

and, **for any change under `internal/persistence` or that touches SQL**, the
Postgres lane as well:

```
make test-integration         # -tags=integration; the pgvector lane
```

`go test ./...` is SQLite. A repository can diverge between the two drivers and
pass — that is how a JSONB byte-exactness difference reached CI, and how a
`LIKE` pattern silently matched nothing on one driver for weeks.

**Read the exit code directly.** Never through a pipe: `go test ./... | tail`
reports the exit status of `tail`.

## 3. A bug fix without a failing test is a guess

Write the test that fails before the fix and passes after it, and name the
incident in a comment. A fix whose test cannot distinguish the broken code from
the fixed code has not been demonstrated.

The same applies to a rare bug: make the interleaving deterministic (a seam, a
hook) rather than relying on a 1-in-600 stochastic reproduction as the gate.

**The test sits at the level where the bug was FOUND, or higher, never only
below it** (operator, 2026-10-14), because every level below already passed.
The levels, lowest first: `unit`; `integration` (real DB, git or HTTP
handler); `executor-e2e` (the real daemon loop); `e2e-lane` (a real
container); `production` (operator observation), which a test cannot reach,
so it is compared as `e2e-lane`. A `review` or audit find also names the
level where a user would observe it, and counts there. The fix's commit
message says `Found at: <level>` and `Regression test at: <level> <test>`,
plus `Lower because: <reason>` when the test sits lower; the local commit-msg
hook (after `make install-hooks`) refuses a `fix:` commit without them.
Canonical text: `https://docs.vornik.io` §4.7 to §4.9.

## 4. Say what is true

- A number with no scope reads as a guarantee — publish what it does NOT cover.
- A control that cannot distinguish "examined and clean" from "never examined"
  reports the first and means the second. **A denylist is that failure by
  construction** — it reports clean about every shape not on the list, so
  publish the DENOMINATOR: how many things were examined, not only how many
  were found. A back-merge guard matching one wording missed twenty-four
  back-merges of another while printing "none new" (2026-09-23).
- A documented behaviour nothing implements is worse than an absent one: it
  stops the next person looking.
- Report failures with their output. "Tests pass" after a partial run is a
  claim about a run that did not happen.

## 5. Before improvising, check what exists

This codebase is large and most surfaces have been solved once. Search for the
prior art before building the second one — a safety check with two
implementations has one that is wrong, and the wrong one is usually the newer.

The same holds for the operator-facing tooling: prefer the shipped skills and
`vornikctl` verbs over hand-rolled commands.

## 6. Work as a team lead, and finish what is in hand

- **Delegate first, never idle-wait.** Reviews, edge-case brainstorms,
  critiques, summaries and research go to Vornik (the companion workflows or
  your agent namespace's team) before you start your own part; keep one in
  flight and read results between steps. Verify what comes back.
- **Teams, not individuals.** A project is a domain, a workflow a capability,
  the swarm a team of roles with distinct jobs, tools and models.
- **Park side tasks.** An off-goal bug or task is filed, with evidence, as a
  GitHub issue (or with the backlog_deposit tool, which lands in the inbox
  that is swept into issues) or handed to a parallel agent or Vornik task;
  only production breakage or a blocker interrupts the goal. Keep an ordered
  in-flight list.
- **Write every operator idea down at once** (a GitHub issue, the
  backlog_deposit tool, or the design), so a compaction cannot lose it.
- **Recall the design every time you touch one**, at minimum, not once per
  session.

## 7. Commit discipline

- `make lint` before every commit.
- Stage the exact paths you changed. Never `git add -A` or `git add .` — other
  work happens in this tree concurrently.
- Commit locally; do not push unless asked.
- The commit message explains WHY, and states what was verified.

## 8. Security-relevant code

If a change touches any of these, state the security impact in the commit
message, make sure a test covers the invariant you relied on, and re-review
the public security model (`docs/public/security-model.md`; it is anchored to
the main files below, so `make lint` asks for it):

- Authentication, admin class and project scoping:
  `internal/api/middleware.go`, `internal/api/admin_class_auth.go`
  (`OperatorSurfaceAccessFor`, `ErrorTextAccessFor`), `internal/api/routes.go`,
  `internal/httpx/realip/`
- Process starts: `internal/spawn/` (the only daemon-linked package that may
  start a process) and `internal/architecture/spawn_law_test.go`
- Sandbox one-shots: `internal/sandboxtool/` (hardening flags, limits,
  `ffmpeg_guard.go`)
- Agent containers and workspaces: `internal/runtime/manager.go`,
  `internal/runtime/pool.go`, `internal/executor/artifacts.go`,
  `internal/safepath/`
- Secrets and redaction: `internal/secrets/secrets.go`,
  `internal/egressscan/`, `internal/report/scrubbed_view.go` (`ScrubLines`),
  `internal/chatview/`
- Untrusted content: `internal/outputguard/`, `internal/untrusted/`,
  `internal/taintlineage/`, `internal/memory/gates.go`, `internal/memoryfirewall/`
- Outbound: `internal/netguard/`, `internal/mcp/client.go`,
  `internal/mcp/mutating_tools.go`, `internal/config/stdio_mcp_guard.go`
- Approvals: `internal/approval/` (`CanonicalSHA256`), `internal/approverdevice/`,
  `internal/brokeractions/worker.go`, `internal/agentadmin/`
- Data protection: `internal/datasubject/`, `internal/aidisclosure/`,
  `internal/retention/`

Rules every such change keeps:

- A new outbound HTTP client has an explicit timeout, reads through a size
  cap, and dials through `netguard` unless it is loopback by construction.
- A new process start goes through `internal/spawn`; the import law refuses
  anything else.
- A new podman invocation goes through `internal/sandboxtool` (one-shot) or
  the runtime manager (agent), never a hand-built argv.
- A new place that stores or forwards model or tool text passes a secrets
  checkpoint.
- `go test -race -short ./...` and `make vulncheck` (`govulncheck`, a
  required CI job) stay green.
