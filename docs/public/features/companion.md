---
sources:
    - path: internal/api/companion_mcp.go
      sha256: dcd24085951349d7a45beabcd0fb51ee767d6186b29ec955229f3d8fcb4b39b2
    - path: contrib/claude-code-companion/.claude-plugin/plugin.json
      sha256: 94bb71bc19dd3bd7892ac8977f993ba8d6f2450d0ff509a8bd795f1c377c55ee
    - path: contrib/codex-companion/.codex-plugin/plugin.json
      sha256: 9f735cfd2f5fb414adfb95bf77ab5ef74edcce7573097411204c9205946ca6e3
---
# Companion plugin

!!! note "Community Edition"

    Included in the free, open-source **Community Edition**. See [Editions](../editions.md).


The vornik **companion** connects your host LLM session to a running vornik
daemon. It ships as a Claude Code plugin and as a Codex plugin; both use the
same companion MCP endpoint and scoped key model. It gives you two things
without leaving your coding session:

- **Project memory** — semantically recall what vornik knows about the repo
  you're in, and deposit notes back into that memory.
- **Async delegation** — hand long-running work (reviews, audits, research,
  bulk ingestion) to vornik's agents, then poll for the result — so the heavy
  lifting runs on vornik's compute instead of burning your editor's context.

The plugin talks to the daemon over MCP-over-HTTP and is gated by its own
scoped key, so it never needs your admin credentials.

## The tools

The companion exposes these MCP tools:

| Tool | Purpose |
|------|---------|
| `recall` | semantic search over the project's memory (ranked snippets + provenance) |
| `remember` | deposit a note into the project's memory |
| `recent_memory` | the most recently learned chunks, newest first |
| `list_scopes` | list the repo scopes the project's memory is partitioned into |
| `memory_correct` | soft-refute a wrong or stale memory chunk, optionally storing the correction |
| `whoami` | show this key's project, the repo scope your calls resolve to right now, the database this daemon writes (so a destructive tool can verify its target instead of trusting a name you typed), and this project's embedding readiness — how much of its memory is semantically searchable, plus the embed-queue depth, so a caller can wait for ingest to finish instead of querying a half-indexed corpus. Also reports the daemon's build revision and its resolved embedding model, which a benchmark needs to attribute a run to a release and to an embedding space. |
| `report_problem` | build an anonymized problem report + prefilled issue URL for you to review and submit |
| `delegate` | queue an async task on vornik; returns a task id and a poll hint |
| `status` | check a delegated task's status |
| `result` | fetch a completed task's output inline |
| `cancel` | cancel a task that hasn't finished |
| `list` | list recent companion-created tasks for the project |
| `catalog` | show which workflows this key may delegate to, with cost estimates and whether each can reach the network |
| `skill_propose` | propose a knowledge skill (instructional know-how) as a draft (needs `skill_write`) |
| `skill_search` | find active/trusted knowledge skills by scope, domain, role (needs `skill_read`) |
| `skill_get` | fetch one knowledge skill's full body (needs `skill_read`) |
| `skill_list` | enumerate knowledge skills by maturity for management (needs `skill_read`) |
| `skill_approve` | promote a draft skill to active — the human gate (needs `skill_admin`) |
| `skill_reject` | retire/revoke a knowledge skill (needs `skill_admin`) |
| `skill_set_global` | set/clear a skill's cross-project (global) reach (needs `skill_admin`) |

The `skill_*` tools are the client surface of the daemon-owned **knowledge-skill
store**: instructional know-how authored from any client (a proven procedure, a
hard-won gotcha) that, once approved, is served to swarm roles and every
companion client. They are distinct from the `SWARM-SKILL.md` capability skills
(workflow + roles) and share no storage with project memory. A proposed skill
lands as a `draft` and never fires until an operator with a `skill_admin` key
approves it. A skill can also be marked **global** (`skill_propose global:true`
or `skill_set_global`) so it injects into every project's roles, not just its
home project — the way a procedure captured in your companion project reaches
the autonomy roles. See [Knowledge skills](knowledge-skills.md).

**Proposals are checked for near-duplicates.** Before a skill is written,
`skill_propose` scores it against the whole catalogue — every repo scope, every
maturity including retired, and other projects' global skills. On a hit it
returns `{"blocked": true, "matches": [...]}` and writes nothing. Answer the
block rather than retrying:

- `supersedes: "<id>"` — this replaces that skill. The old one is retired and
  its body kept (still readable by id); it is never overwritten.
- `confirm_distinct: "<why they differ>"` — they are genuinely different. The
  justification is required and is stored on the skill.

You do not need to search first: the preflight already looks wider than
`skill_search`, which filters by repo scope and so hides skills that would be
injected alongside yours anyway. Matching is semantic where an embedding backend
is configured, and falls back to a weaker lexical comparison otherwise — a
preflight can miss, but it never blocks authoring when the embedder is down.

In Claude Code, several tools are also wrapped as slash commands — for example
`/recall`, `/remember`, `/delegate`, `/review` (a one-shot architectural
review), `/peek` (recent tasks), and `/upload` (attach files to a delegation).
In Codex, use the MCP tools directly; the Codex adapter ships a `delegate` skill
that teaches the same recall-before-delegate and file-attachment rules.

## Operator skills

Both companion plugins bundle **five operator skills** that ship enabled by
default and teach your host LLM to drive Vornik's own tooling instead of
improvising. They cross-reference each other, so a session can walk from "where
is this documented" to "set this up" to "is it set up right" to "why is it
broken" to "file it upstream" without you naming the next step.

**`vornik-docs`** — where the documentation lives. It carries the site map and
an order of preference that puts your **installed** CLI's own `--help` first,
then a local checkout, then this site, and the model's own recall last — used to
decide where to look, never as the answer. It exists because the characteristic
failure when answering Vornik questions is a confident, plausible, nonexistent
config key or CLI flag, and that failure is expensive: unknown keys are
frequently ignored, so you get no error and the setting silently does nothing.
The skill also states where the docs deliberately stop, so an honest gap is
reported rather than filled in.

**`configure-vornik`** — configuring a deployment: daemon settings, projects,
swarms, workflows, models, secrets, channels. It leads with the config
hazards that silently no-op an otherwise correct change. The registry tree
holding `projects/`, `swarms/`, and `workflows/` is resolved by a *fallback
chain*, not one environment variable, and `vornikctl`'s chain differs from
the daemon's — so a file can be perfectly valid and still sit in a directory
the daemon never reads. `VORNIK_CONFIGS_DIR` is honoured only when the
directory already contains all three subdirectories; otherwise it is skipped
with no error at all. The skill then pins the apply loop — validate with
`vornikctl doctor`, `vornikctl config reload`, then **confirm** with
`vornikctl config reload-status`, where validation errors actually surface —
and the restart-versus-reload boundary: systemd resolves the daemon's
environment only at start, so unit and env-file edits need a restart.

**`validate-install`** — checking a deployment against the
[reference architecture](../reference/reference-architecture.md): the expected
shape of a healthy install, and where yours diverges. Strictly read-only, which
matters more than it sounds — `POST /api/v1/config/reload` looks like the natural
way to ask whether the registry is valid, and it answers by *applying* the tree,
so an audit could push a half-edited file into service. The skill's discipline is
**configured versus observed**: a config value is a statement of intent, never
evidence of behaviour, so it reads the daemon's resolved state from the boot log
and then the usage ledger that proves the subsystem actually ran. Findings are
split by severity, because the failure mode of a validator is noise — absence of
an optional subsystem is never reported, and neither are your names or model
choices.

**`troubleshoot-vornik`** — diagnosing a deployment that is down, degraded,
or failing tasks, routed by symptom. Daemon down goes to
`vornikctl doctor --offline`, the static escape hatch that needs no running
daemon. Degraded goes to `vornikctl doctor` and `doctor feature`. A failed
task goes to `vornikctl task explain` and then `vornikctl playbook show
<CLASS>` — the failure-class corpus that already carries a written
remediation for the error the executor stamped. The skill also states plainly
which findings `--fix` can repair and which are diagnostic only, so you don't
get sent in a circle.

**`report-problem`** — filing an **anonymized** Vornik problem report (a bug,
a crash, a misbehaving swarm, or an install failure) as a prefilled
`github.com/grinco/vornik` issue. The work is done by the deterministic
`vornikctl report` CLI (rich diagnostics when the daemon is up, `--offline`
static checks when it is down) plus the quickstart installer's own failure URL
for pre-daemon install errors; the skill is the guardrail — review the
anonymized body before submitting, and you file it under your own GitHub
account (nothing is ever posted automatically).

## Project memory and repo scope

`recall` and `remember` operate on the memory of the vornik **project** your key
is bound to. Because one project can back several repositories, every note
carries a **repo scope** — a token derived from the repo you're working in. A
recall returns matches for the current scope plus anything marked
cross-cutting, so two repos served by the same project don't pollute each
other's results. Claude Code resolves the scope from your checkout's git remote
in its SessionStart hook. Codex ships no SessionStart hook, so its `delegate`
skill and plugin prompt instruct the model to derive the same remote token and
pass it explicitly as `repo_scope` on every memory call.

As a backstop for clients without an automatic injector, a companion key can be
minted with a **default repo scope** (`vornikctl companion grant --repo-scope
<token>`). When set, the daemon stamps that scope on any `recall` / `remember` /
`recent_memory` / `delegate` call that omits `repo_scope`, so a forgotten
argument can't silently land a note un-scoped. An explicit per-call `repo_scope`
still overrides the key default — keep passing it on a key reused across repos.

Three scope values are worth distinguishing:

- a **specific token** (`github.com/<org>/<repo>`) — the note is scoped to that
  repo; a recall in a different repo won't see it.
- `"*"` — **cross-cutting**. The note applies across every repo the project
  serves and surfaces in every scoped recall. Use it for project-wide material
  like coding standards, policies, and operator-wide conventions:
  `remember(content="…", repo_scope="*", class="policy")`. It works from any key
  (an explicit `repo_scope` always overrides the key default).
- **NULL** (no scope) — the legacy/uncategorized bucket for notes ingested
  before repo scopes existed. A non-strict recall still surfaces NULL-scoped
  notes across scopes (a migration-grace fallthrough), but `strict_scope=true`
  drops them. Don't deliberately deposit NULL for project-wide notes — use
  `"*"` instead; NULL is meant to be promoted out of via the retag CLI.

`recall` matches the current scope plus `"*"` plus (when `strict_scope` is off)
NULL-scoped notes. `strict_scope=true` narrows to the current scope plus `"*"`
only — handy for spotting NULL-scoped leaks or confirming a scope is actually
populated.

```text
/recall how does the scheduler lease tasks?
/remember the broker API rejects fractional shares with error 10243 — keep shares whole
```

Notes go through vornik's full ingest pipeline (secret scanning, dedup, policy),
and large content should be sent through the ingest workflow rather than a single
`remember` call.

### Re-ingesting a document

When you change a document and ingest it again, recall should serve the new
version, not both. Claude's `/rag-ingest` sends each file's path within its git
repository (`docs/design/index.md`, say) along with the bytes. That path is
the document's identity: ingesting the same path again, in the same repo
scope, retires the earlier versions, and recall serves only the newest. Two
different files that share a name, such as two `index.md` files, stay
separate because their paths differ.

`/rag-ingest` takes the repo scope from the repository that holds the files,
not from the directory you run it in. It refuses an upload that mixes files
from two repositories, or a repository with loose files, and names the scopes
it found, so each repository's files land under their own scope. A file
outside any repository is ingested as before, with no path. Codex, staging
files by hand, sends the same `path` field on each input artifact.

Documents ingested before paths existed carry only a file name. Re-ingest them
with a current plugin, then retire the old versions with
`vornikctl memory supersede-legacy-documents -p <project> --scope <scope> --root <checkout>`.
It is a dry run until you add `--apply`, and it leaves alone any name that more
than one file in the checkout shares.

### Digging harder when the fast answer misses

`recall` is tuned for interactive use: one search pass, ranked by fusing semantic
and keyword matches, back in well under a second. When that misses something you
are confident is in there, pass `sufficient` to switch to the slower,
higher-quality retrieval mode:

```text
/recall sufficient=true which design covers the fork-bomb PID limit
```

That mode widens the search when the first pass returns too few strongly-relevant
results, and re-orders candidates with a model rather than by fusion score alone.
It costs an extra model call and takes noticeably longer, which is why it is off by
default — but it is the same mode agents use when assembling context for a task, so
it is the closest thing to "search the way the swarm searches".

### Dating a note

By default a note is filed under the moment you deposited it, and `recall`'s
date filters match on that. When you are recording something that *happened* at
a different time — an incident from last March, a decision taken in Q1, a
meeting note written up a week later — pass `event_time` so date-filtered recall
finds it by when it happened rather than by when you stored it:

```text
/remember event_time=2026-03-14 the ibgateway warm-up outage traced to a stale session cookie
```

It accepts `YYYY-MM-DD` or full RFC3339. Leave it off when you don't know, and
recall falls back to the deposit time exactly as before. A value that isn't a
date is rejected rather than quietly ignored, so a typo can't file the note
under the wrong clock without telling you.

This matters most for bulk ingests: a whole document set deposited in one pass
shares a single deposit timestamp, so without `event_time` a query like "what
changed in July" matches all of it or none of it.

## Delegation

`delegate` hands a job to vornik and returns immediately with a task id and an
estimated time — you keep working while a swarm agent does the task on vornik's
infrastructure. `status` and `result` poll it. Because the work happens on
vornik and `result` returns only the final output artifact, a long review or
audit doesn't consume your editor's token budget. File-bearing workflows must
receive files as `inputArtifacts`; Claude's `/upload` command wraps that flow,
while Codex should call `delegate` directly with base64 `inputArtifacts`. When
the target workflow declares `require_input_artifacts`, the daemon stages your
upload as a raw file rather than extracting it into project memory first — so
the agent reads exactly the bytes you sent, and no client has to opt into that
behaviour.

An upload that cannot fit the reviewing agent's context is refused at delegate
time rather than part-way through the run. The refusal says how much you sent,
how many prompt tokens that is, and what the budget was:

```
ARTIFACTS_EXCEED_CONTEXT: 248 KB of staged artifacts is about 84718 prompt
tokens, against a usable budget of 41808 of the reviewer's 100000-token context
```

The remedy is to split the upload — one delegation per subsystem or per file —
and to quote what the agent needs from a large document inline in the prompt
instead of attaching the whole thing. This is worth refusing early because the
alternative is what used to happen: the agent runs, spends its budget, and then
fails on its output contract, so you pay for a review you do not get.

The check applies only to workflows where an agent reads the upload. An
ingest workflow with no agent step, such as `companion-rag-ingest`, stores each
file directly and is exempt, so a large document can be ingested whole.

One note on `/upload`'s output, because its failure used to be quiet. A run that
completed prints `VORNIK_UPLOAD_END` as its last line; output without that
marker means the command was cut before it ran and there is no task to poll,
however much the rest of it looks like a success. Backticks in a prompt used to
cause exactly that and no longer do (bundle 0.24.0) — the commands' shell blocks
are fenced, so a backtick is ordinary text. A line consisting of three backticks
still closes a fence, which is what the marker is there to catch.

The shipped delegation workflows include:

- **architectural review** — a second opinion on a diff, PR, or design doc
  (handy as a pre-merge gate),
- **test-coverage audit**, **doc review**, **data validation**,
- **research gather** and **report summarize**, and
- **RAG ingest** — bulk-load files into the project's memory.

`catalog` lists exactly which of these your key is allowed to run. When you're
about to delegate something vornik may already know, `delegate` can surface a
hint from memory first, so you don't spend compute re-deriving it.

**Vornik agents cannot browse.** An agent can only act through the tools its
role grants, and most companion roles hold nothing that reaches the network —
so handing one a URL gets you a fluent answer assembled from nothing at all.
`catalog` reports this per workflow as `network_access`, either `none` or
`possible`, next to the description. A `delegate` whose prompt contains a URL
is refused outright when the target workflow is `none`, rather than queued and
reported as under way.

To get external pages into project memory, fetch them in your editor with its
own web tools, save them as local files, and send those files to the
**RAG ingest** workflow — `/upload` in Claude Code, or `delegate` with base64
`inputArtifacts` in Codex. If a URL in your prompt is incidental — a link
quoted for context in a diff you want reviewed — pass
`acknowledge_workflow_cannot_fetch: true` and the delegation proceeds.

## Broker projects: a front-end agent without the keys

The companion can also serve a **front-end agent**, a conversational assistant
such as Hermes Agent that your users talk to all day. The pattern is privilege
separation: the front agent holds **no credentials** for your mailbox, CRM or
accounts. Vornik holds them. When the front agent needs something from those
systems, it runs a **broker workflow** on Vornik and gets back a distilled
result. It never gets raw access or raw content.

A **broker project** is an ordinary project with `broker: true`. Everything
about it is stricter:

- **Broker workflows only.** A key on a broker project can run only workflows
  that declare a `broker:` block, and a broker workflow runs nowhere except in
  a broker project.
- **Typed inputs, no prompt.** A broker workflow declares an `input_schema`.
  `delegate` takes `inputs` that must match it, and refuses a free-text
  `prompt`. Every string input must be bounded (`enum`, a date or time
  `format`, or `pattern` with `maxLength`), or be marked `x-untrusted: true`
  with a `maxLength` of 512 or less. All untrusted strings together may carry
  at most 1024 characters. Untrusted values reach the broker's agent wrapped as
  data, never as instructions.
- **One document out.** `result` returns only the file named in
  `egress.output`, validated against `egress.schema` and capped at
  `egress.max_bytes` (at most 64 KiB). No other artifact of the task is
  returned, a document that is too large is refused rather than truncated, and
  `status` reports a closed `error_class` instead of the raw error text. Every
  string in the document is scanned for injection phrasing and, by default,
  wrapped as third-party data.
- **No memory, no skills.** Keys on a broker project cannot use `recall`,
  `remember` or the skill tools, and `vornikctl companion grant` refuses to
  grant them there. Give the front agent its long-term memory in a separate
  project, with a key minted `--no-delegate`.
- **Read-only tools, declared.** Each MCP server a broker workflow uses must be
  marked `broker_read_only: true` in the project, with an explicit
  `allowed_tools` list, and the workflow's roles must name each tool exactly.
  Vornik cannot check that a server's tools only read; that flag is your
  statement that they do.

A minimal setup, with two keys in two projects:

```bash
# the project that holds the mail server, with broker: true in its config
# (configs/examples/broker-mail.yaml is a complete example)
vornikctl companion grant -p broker-mail --client hermes --workflows mail-digest,mail-reply --budget-usd 5
# the front agent's memory, which can never delegate
vornikctl companion grant -p assistant-memory --client hermes --memory-all --no-delegate
```

`result` accepts `wait_seconds` (up to 25): the call is held open until the
task finishes or the time runs out, so a chat agent does not have to poll.

**Completion push.** A harness that can receive a webhook passes
`notify: {url, token}` to `delegate` (when the daemon advertises
`companion-push`). Vornik then POSTs `{task_id, state}` when the task ends,
and `{task_id, action_id, action, state}` when a proposed write changes state.
Pushes carry ids and closed states only, with `Authorization: Bearer <token>`
if you gave one; fetch anything else with `status` or `result`. A push can
arrive twice or report only the latest state, so treat it as a prompt to
check, not as the record. Delivery is retried on later passes and survives a
daemon restart. The response to `delegate` says `"push": "registered"`, or
`"not_registered"` if the webhook could not be stored (the task still runs;
poll it). A `notify` the daemon refuses creates no task.

Pushes go to public addresses only, unless the project lists the private
range the receiver lives in:

```yaml
companion_push:
  # RFC 1918, fd00::/8-style unique-local, or 100.64.0.0/10 (Tailscale and
  # similar overlays). List the narrowest range that reaches your agent: in
  # some clouds 100.64.0.0/10 fronts provider-internal services.
  allowed_cidrs: ["192.168.1.0/24"]
```

Loopback and link-local addresses are always refused, redirects are refused,
and a hostname is judged by the address it resolves to when Vornik connects.

**Upgrading.** Nothing changes for existing keys or plugins until you set
`broker: true` on a project. Deploy the new daemon before you add that flag:
an older daemon rejects a project file containing it. Plugins detect support
through the `companion-broker` and `companion-result-wait` flags in
`/api/v1/capabilities` and fall back on an older daemon. `vornikctl companion
grant --no-delegate` refuses to hand out a key from a daemon too old to honour
the flag: it revokes the key and tells you to upgrade.

**What this does and does not promise.** The front agent never holds the
credential and never sees content outside the declared egress schema. That is
not the same as "nothing sensitive reaches it": a summary of a sensitive email
is itself sensitive. Injection phrasing in the source data is reduced and
labelled, not made impossible, which is why a broker workflow cannot write
anything on its own: at most it proposes a write that a person approves (see
below). If you ever set `broker: false` on a broker project
again, run `vornikctl memory wipe --project <project>` first. The project's
memory holds summaries of every broker task, and without the flag they become
recallable.

### Proposed writes: a person approves every one

A broker workflow may **propose** a write, such as a reply to an email,
instead of performing it. The workflow declares each kind of write under
`broker.proposes`: the MCP tool that performs it and an `args_schema` bounding
the arguments the agent drafts. The agent writes a proposal file; it never
holds the tool. After the task completes, the proposal appears in `/inbox` as a
**Needs approval** card showing the tool, the requesting key and the complete
arguments, with the fields a model drafted from third-party content flagged.
**Approve & send** binds to exactly those arguments, and the daemon then calls
the tool once. Nothing retries a write: if the daemon cannot tell whether a
call reached the vendor, the action is left `unknown` for an operator, who
checks and records the outcome with `vornikctl broker-action resolve`.

Writes are off unless you turn them on. They need:

- `broker.writes: on` in the daemon config (default `off`; while off, a
  workflow that proposes is refused at `delegate` with
  `BROKER_WRITES_DISABLED`, and read-only workflows keep working);
- an MCP server in the broker project declared `broker_write: true`, listing
  the tool in `allowed_tools` (see [MCP tools](../guides/mcp-tools.md)).

The front agent sees each proposed write in `result` and `status` as an
`actions` list of `{action_id, action, state, expires_at}`, never the
arguments and never the tool's response. `state` is one of
`pending_approval`, `approved`, `executing`, `executed`, `failed`, `rejected`,
`expired`, `unknown`, `proposal_missing` and `proposal_invalid`. `catalog`
lists each workflow's `proposes` as `{action, tool}`, and the daemon
advertises `companion-broker-actions` in `/api/v1/capabilities` while writes are
on. If the action store cannot be read, `result` and `status` for a proposing
workflow fail with a retryable error rather than report no actions. An
unapproved proposal expires after its `approval_ttl` (default 24 hours).
When a Telegram bot is configured, operators get a message with the project,
the task and a link to `/inbox`: no arguments, and no way to decide from
Telegram.

The shipped reference is the `mail-reply` workflow: it finds one message,
drafts a reply the operator reviews in `/inbox`, and returns only whether the
message was found and a one-line summary of the draft.
`configs/examples/broker-mail.yaml` declares the send-capable server it needs.

**Hermes Agent** has a ready-made plugin in `contrib/hermes-companion/`. It
adds the broker tools, a skill teaching Hermes when to use them, a hook that
announces finished tasks, and a `vornik` memory provider. Install steps are in
its README.

The shipped reference is the `mail-digest` workflow with the `broker-swarm`
swarm: a digest of recent mail carrying sender domain, time, category, whether
it needs a reply and a one-line summary, and never a body or an address.

## Setting it up

On the **daemon** side, an operator prepares a companion project and mints a
**companion-scoped key** (a plain API key won't be accepted on the companion
endpoint):

```bash
# confirm the daemon advertises the companion capabilities
curl "$VORNIK_URL/api/v1/capabilities"

# mint a scoped key (printed once) — memory + knowledge-skill capable
vornikctl companion grant \
    --project companion-$USER \
    --client claude-code \
    --workflows companion-architectural-review,companion-rag-ingest \
    --budget-usd 5 --memory-all --skill-all
```

`--memory-all` and `--skill-all` are the recommended default for a companion
project: they grant RAG (`remember`/`recall`) and the full knowledge-skill
store (`skill_read`/`write`/`admin`) so you can capture and approve procedures
without a second grant. Narrow them per key — e.g. `--skill-read --skill-write`
(propose but not self-approve), or drop `--skill-all` entirely for a
delegate-only key. `--memory-all` = `--memory-read --memory-write`.

Use `--client codex` instead when minting a key for the Codex plugin. For Codex
(no SessionStart scope injector), add `--repo-scope github.com/<org>/<repo>` so
memory calls that omit `repo_scope` inherit the right scope by default.

The key's allowed workflows, spend cap, and memory + skill permissions are
enforced server-side from the key itself — never from the request. The spend cap
counts what the key has spent across the tasks it created, including tasks it
created through the REST API rather than `delegate`; the per-key row on
[`/ui/spend`](../guides/observability.md#spend-per-api-key) is the same
attribution, over a wider set of rows.

On the **client** side, set the companion bearer token and install the plugin:

```bash
export VORNIK_COMPANION_TOKEN="sk-vornik-companion-…"   # the key from `companion grant`
```

For Claude Code, add the plugin from your vornik checkout's companion plugin
directory (`claude --plugin-dir <path>/contrib/claude-code-companion`), or
install it from the bundled plugin marketplace for a persistent setup. Once
loaded, the tools and slash commands are available in the session, and a
start-up digest brings recently-completed delegations and fresh memory back into
context.

The hook also fires **after a compaction**, and does something different there:
it re-plants the standing directives — repo scope, recall-before-reading-code,
skill capture — and deliberately does not reprint the delegation digest.
Compaction is where a long session has lost the most context, so the directives
are exactly what needs restoring; the digest is a point-in-time list you have
usually already acted on, and reprinting it would spend the context the
compaction just freed. Run `/peek` if you want it back.

For Codex, install `codex-companion` from the bundled Codex marketplace at
`<path>/.agents/plugins/marketplace.json`, or load the plugin directly from
`<path>/contrib/codex-companion`. Do not install the Claude marketplace at
`<path>/.claude-plugin/marketplace.json` into Codex; that package carries the
Claude manifest and will not register the companion MCP server in Codex. The
Codex plugin exposes the same MCP server and a Codex-native `delegate` skill,
but no Claude-only slash commands or SessionStart hook. The bundled Codex MCP
entry targets `http://localhost:8080`; for a remote daemon, override the MCP
entry locally:

```bash
codex mcp add vornik \
  --url <remote>/api/v1/mcp/companion \
  --bearer-token-env-var VORNIK_COMPANION_TOKEN
```

Keep the repo plugin portable: do not put host-specific URLs or shell-style
expressions such as `${VORNIK_URL:-http://localhost:8080}` into the plugin
`.mcp.json`. Codex does not expand that syntax and will fail before the MCP
handshake. If the local override supplies the remote daemon URL, disable only
the plugin-bundled MCP server in `~/.codex/config.toml` while leaving the plugin
itself enabled so its skill remains available:

```toml
[plugins."codex-companion@vornik".mcp_servers.vornik]
enabled = false
```
