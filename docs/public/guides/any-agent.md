# Use Vornik from any agent

Any agent that speaks MCP can use Vornik's project memory (`recall`,
`remember`) and hand work to it (`delegate`, `result`), with no Vornik
plugin. Claude Code and Codex have plugins; Claude Desktop, Cursor and
opencode have their own guides ([Claude Desktop](claude-desktop.md),
[Cursor](cursor.md), [opencode](opencode.md)). This page is for the rest:
Gemini CLI and others, and opencode set up by hand.

**What works, measured.** On 2026-11-20, Gemini CLI 0.41.2 and opencode
1.14.37, set up as below, each completed remember, recall, delegate, result
and memory_correct against Vornik 2026.11.7, in about 100 seconds.

**What you do not get.** No session digest when a session starts, and no
hooks: the agent learns what to do from the instructions you add in step 4.

## What you need

- A companion key, `sk-vornik-…`, minted with a default repo scope. An
  operator mints it on the Vornik host:

  ```bash
  vornikctl companion grant --project=<project> --client=<your-agent> \
    --label="$(hostname)/$USER" --memory-all --skill-read \
    --workflows=companion-research-gather,companion-report-summarize \
    --budget-usd=10 --repo-scope=github.com/ORG/REPO
  ```

  `--repo-scope` matters here. An agent without Vornik hooks passes no
  repo scope, so without a default every note it stores is unscoped, and
  scoped recalls in other sessions may not find it.
- `vornikctl` on the machine where your agent runs.

## 1. Store the key and print the entry

```bash
printf '%s\n' "$VORNIK_COMPANION_TOKEN" | \
  vornikctl companion connect generic --print --key-stdin --url https://vornik.example
```

It checks the key, stores it in a file only you can read, and prints two MCP
entries to paste. The key is never printed. It warns when the key has no
default repo scope.

- **Form 1** runs `vornikctl companion mcp-bridge`, which reads the key from
  that file. Use it when your agent can start a command.
- **Form 2** is streamable HTTP, with the key read from the
  `VORNIK_COMPANION_TOKEN` environment variable. An agent started from a
  desktop does not read your shell profile.

## 2. Add the entry to your agent

Formats as measured on 2026-11-20. Your agent's version may differ: check its
own MCP documentation.

**Gemini CLI 0.41.2**, in `.gemini/settings.json` (the project) or
`~/.gemini/settings.json`:

```json
{
  "mcpServers": {
    "vornik": {
      "httpUrl": "https://vornik.example/api/v1/mcp/companion",
      "headers": { "Authorization": "Bearer ${VORNIK_COMPANION_TOKEN}" }
    }
  }
}
```

- Gemini CLI ignores a project's settings until you trust the folder. Run
  `gemini` once in it and accept, or set `GEMINI_CLI_TRUST_WORKSPACE=true`
  for a headless run.
- On the free tier, 0.41.2's default model routing failed: it asked for a
  retired model, then for one with no free quota. Pass a model,
  `gemini -m gemini-3.5-flash`, or upgrade the CLI.

**opencode 1.14.37**, in `opencode.json` (or let `vornikctl companion
connect opencode` write a stdio entry, with Vornik's rules, a session digest
and skills: [opencode](opencode.md)):

```json
{
  "mcp": {
    "vornik": {
      "type": "remote",
      "url": "https://vornik.example/api/v1/mcp/companion",
      "headers": { "Authorization": "Bearer {env:VORNIK_COMPANION_TOKEN}" }
    }
  }
}
```

**Any agent that starts a command:** paste form 1 from step 1.

## 3. Check the connection

```bash
vornikctl companion doctor --client generic --url https://vornik.example
```

One line per check: the key file, the daemon, the key itself, and its repo
scope. It says which key the bridge uses. It cannot see your agent's own
settings or environment.

In your agent, list its MCP servers (`gemini mcp list`, `opencode mcp list`):
Vornik should show as connected.

## 4. Tell your agent how to work with Vornik

```bash
vornikctl companion instructions --write AGENTS.md --url https://vornik.example
```

(Or export `VORNIK_URL` once, and leave out `--url` in every command.)

This writes Vornik's working rules into your `AGENTS.md`, between
`<!-- vornik:start -->` and `<!-- vornik:end -->`. Running it again replaces
that section and leaves the rest of the file alone. The rules come from your
Vornik, so they match its version. They also say two things an agent
without hooks needs:

- call `recent_memory` when a session starts;
- your agent shows Vornik's tools under a prefix: Gemini CLI as
  `mcp_vornik_recall`, opencode as `vornik_recall`. In one test a small local
  model (9B parameters) called `recall` without the prefix, was refused,
  and stopped.

Without `--write`, the command prints the text for an agent that reads its
instructions from somewhere else.

- **opencode** reads `AGENTS.md`.
- **Gemini CLI** reads `GEMINI.md` by default (per its documentation; not
  measured here). Write there with `--write GEMINI.md`, or add
  `"context": {"fileName": ["AGENTS.md", "GEMINI.md"]}` to its settings.

**Skills.** The companion skills (how to configure, troubleshoot and
delegate) are Agent Skills folders in the public repository,
`contrib/claude-code-companion/skills/`. Copy them where your agent reads
skills, if it reads any.

## 5. Try it

Ask your agent to store a test note in Vornik, recall it, and then refute
it. To check the same path without an agent, run the round trip in
[`AGENTS.md`](https://github.com/EaseIT-cz/vornik/blob/main/AGENTS.md),
Runbook B4.

## Disconnect

```bash
vornikctl companion disconnect generic
```

It deletes the key file. Remove the entry from your agent's settings
yourself. The key stays valid until the operator revokes it
(`vornikctl key revoke <id> --project=<project>`).
