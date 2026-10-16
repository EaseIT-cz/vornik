# codex-companion (Codex plugin)

Async-offload companion for Codex. It connects Codex to the vornik companion
MCP endpoint so long reviews, audits, validations, research, summarization, and
project RAG memory work can run in vornik-managed containers instead of inside
the current Codex turn.

This is the Codex adapter for the same daemon-side contract used by
`contrib/claude-code-companion`. The MCP tool surface is shared; the host-client
packaging is different.

## What you get

- MCP tools from the daemon: `delegate`, `status`, `result`, `cancel`, `list`,
  `catalog`, `whoami`, `recall`, `remember`, `recent_memory`, `list_scopes`,
  `memory_correct`, `memory_list` (what Vornik remembers, paged; export is a
  link to your paired approver device, never through the model), and
  `report_problem`.
- A `delegate` skill that teaches Codex when to recall, delegate, attach files
  with `inputArtifacts`, and pull results.
- A `knowledge` skill for the daemon-owned knowledge-skill store.
- A `backlog-deposit` skill + the shipped `scripts/vornik-backlog-deposit.sh`
  it drives — capture an off-scope finding into the repository's own backlog
  without derailing the current task, with dedup against every existing item
  and a secret scan over content that is about to be committed. Client-side by
  design: the daemon must not write your checkout, and the daemon-side tool
  flattens an item to a single 600-character line. Byte-identical to the
  Claude Code copy; a parity test fails the build on drift. A bug deposit
  can carry `--found-at <level>` (unit, integration, executor-e2e, e2e-lane,
  production, review): its regression test belongs at that level.
- `develop-with-memory`, the same text as the Claude Code skill: recall
  before reading code, design before changing it, keep memory current, and,
  where a recall's `design_record` finds no design stored with this
  repository's scope, an as-built record of the part being changed, reviewed,
  then ingested. Codex loads it when the task matches; it has no session-start
  channel.
- The operator lifecycle skills, mirrored from the Claude Code bundle:
  `vornik-docs` (where the documentation lives, with the real site map, and
  `vornikctl --help` ahead of recall so Codex quotes flags that exist),
  `configure-vornik` (find the config tree the daemon actually reads,
  scaffold, then validate → reload → confirm), `validate-install` (check a
  deployment against the published reference architecture, read-only, on
  resolved state rather than configured state), `troubleshoot-vornik`
  (route by symptom through the doctor, the failure-class playbook, and
  task post-mortems), and `report-problem` (file an anonymized issue on
  `github.com/EaseIT-cz/vornik` that the user submits themselves). They
  cross-reference each other so Codex can walk the lifecycle.
- The same companion bearer-key model as Claude Code, but minted with
  `--client=codex`.

This adapter does not ship Claude slash commands (`commands/`) or a
SessionStart hook. Codex should call MCP tools directly and, when file bytes
are needed, stage them as `inputArtifacts` in the `delegate` tool call.

### The recall nudge (hooks/hooks.json)

The bundle ships one hook: an offline, just-in-time nudge to recall a
subsystem's design before reading its code. Codex has no file-read tool, so
the hook reads the text of each shell command Codex runs and looks for paths
given to a read program (`cat`, `head`, `tail`, `sed`, `rg`, `grep`, `find`,
`ls` and a few others). It **parses** the text and never runs it. A command
it cannot parse counts as nothing.

- On the third read into one subsystem (for example `internal/executor`)
  with no successful `recall` in the last 15 minutes, it adds one line of
  context telling Codex to call `mcp__vornik__recall` for that subsystem's
  design.
- It fires once per subsystem per session, never blocks a command, makes no
  network call, and keeps no file names, commands or queries: only
  subsystem names, counters and a timestamp, under the plugin's data
  directory.
- Codex runs plugin hooks only after you trust them. At startup it shows
  "Hooks need review"; choose to trust them to turn the nudge on, or continue
  without trusting to leave it off.

| Variable | Effect |
|---|---|
| `VORNIK_NUDGE=off` | silence the nudge |
| `VORNIK_NUDGE_THRESHOLD` | reads into one subsystem before the nudge (default 3) |
| `VORNIK_NUDGE_RECALL_WINDOW_S` | how long a recall suppresses it, in seconds (default 900) |

Without `VORNIK_COMPANION_TOKEN` the hook stays silent, since the recall tool
cannot work.

## Prerequisites

1. A running vornik daemon. The companion capability flags should be enabled:

   ```bash
   curl ${VORNIK_URL:-http://localhost:8080}/api/v1/capabilities | jq '.features'
   ```

2. A companion project:

   ```bash
   vornikctl init project --template companion companion-$USER
   ```

3. A Codex-scoped companion API key:

   ```bash
   vornikctl companion grant \
       --project=companion-$USER \
       --client=codex \
       --label=$(hostname)/$USER \
       --workflows=companion-architectural-review,companion-test-coverage-audit,companion-doc-review,companion-data-validation,companion-research-gather,companion-report-summarize,companion-rag-ingest \
       --budget-usd=25 \
       --memory-all \
       --repo-scope=github.com/EaseIT-cz/vornik
   ```

   Copy the printed `sk-vornik-companion-...` secret. It is shown once.

   `--repo-scope` is strongly recommended for Codex. Codex ships no SessionStart
   scope injector (see below), so a model that forgets to pass `repo_scope`
   would otherwise deposit NULL-scoped chunks. With a default scope on the key,
   the daemon stamps it on any memory call that omits `repo_scope`; an explicit
   per-call scope still overrides it. Set it to the canonical git-remote token
   of the repo this key is used in. For a key reused across repos, prefer
   passing `repo_scope` per call (the `delegate` skill explains how to derive
   the canonical token) and treat the key default as a backstop.

## Configure

Set the companion bearer token before starting Codex:

```bash
export VORNIK_COMPANION_TOKEN="sk-vornik-companion-...."
```

The bundled Codex plugin points at
`http://localhost:8080/api/v1/mcp/companion`. Codex requires plugin MCP URLs to
be concrete absolute URLs; shell-style expressions such as
`${VORNIK_URL:-http://localhost:8080}` are not expanded by the plugin loader.
For a remote daemon, add a local MCP override with the URL you need:

```bash
codex mcp add vornik \
  --url https://vornik.internal.example.com:8080/api/v1/mcp/companion \
  --bearer-token-env-var VORNIK_COMPANION_TOKEN
```

`VORNIK_COMPANION_TOKEN` intentionally has no fallback.

## Use

Inside Codex, call the vornik MCP tools directly:

- `mcp__vornik__catalog` to see the workflows this key may use.
- `mcp__vornik__recall` before paying for fresh research or review work.
- `mcp__vornik__delegate` to queue async work.
- `mcp__vornik__status` and `mcp__vornik__result` to pull progress and output.
- `mcp__vornik__remember` and `mcp__vornik__memory_correct` for project memory.

For local files, never put a path in the delegate prompt and assume the agent
can read it. Read and base64-encode the file locally, then pass it in
`delegate.inputArtifacts`:

```json
{
  "workflow": "companion-architectural-review",
  "prompt": "Review the attached design doc for architectural issues.",
  "repo_scope": "github.com/EaseIT-cz/vornik",
  "inputArtifacts": [
    {
      "name": "design.md",
      "content": "<base64 file bytes>"
    }
  ]
}
```

Workflows that declare `require_input_artifacts` (`companion-architectural-review`,
`companion-doc-review`, `companion-rag-ingest`) receive raw staged files: the
daemon forces `skip_auto_extract` for them so upload-time extraction can't
suppress staging. Pass `skip_auto_extract=true` yourself only when staging a raw
file for a workflow that makes no such declaration.

## Validate

From the repo root, using the Codex plugin-creator validator (ships with Codex
under `~/.codex`):

```bash
python3 ~/.codex/skills/.system/plugin-creator/scripts/validate_plugin.py contrib/codex-companion
```
