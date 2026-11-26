# Use Vornik from opencode

One command connects opencode to Vornik's companion. It gives opencode:
- project memory (`recall`, `remember`);
- work handed to Vornik (`delegate`, `result`);
- Vornik's working rules and a session digest in every session;
- Vornik's skills.

No opencode config holds your key.

**What is measured.** On 2026-11-21, opencode 1.14.37 was connected this way
to a Vornik daemon, with a small local model. In one session the model:
- quoted Vornik's working rules;
- listed the Vornik skills;
- called `recall` through Vornik's MCP entry;
- quoted the session digest.

## What you need

- opencode, and `vornikctl` on the same machine (Linux or macOS).
- A companion key, `sk-vornik-…`, minted with a default repo scope. An
  operator mints it on the Vornik host:

  ```bash
  vornikctl companion grant --project=<project> --client=opencode \
    --label="$(hostname)/$USER" --memory-all --skill-read \
    --workflows=companion-research-gather,companion-report-summarize \
    --budget-usd=10 --repo-scope=github.com/ORG/REPO
  ```

  The repo scope matters. opencode passes none, so the key's default is the
  only scope your notes get, and `connect` refuses a key without one unless
  you pass `--allow-unscoped`.

## Connect

```bash
printf '%s\n' "$VORNIK_COMPANION_TOKEN" | \
  vornikctl companion connect opencode --key-stdin --url https://vornik.example
```

`connect` first checks the key against Vornik, then stores it in a file only
you can read (`~/.config/vornik/companion/default.key` on Linux). It writes
into opencode's config directory, `$XDG_CONFIG_HOME/opencode` (by default
`~/.config/opencode`):

| Written | What it does |
|---|---|
| MCP entry `vornik-companion-default` in `opencode.json` | Runs `vornikctl companion mcp-bridge`, which reads the key file. Every other setting is kept. |
| `plugin/vornik-companion.js` and its guidance file | Adds Vornik's working rules, and a digest of your delegations and recent project memory, to opencode's system prompt. |
| `skills/vornik-*` | delegate, knowledge, develop-with-memory, configure-vornik, troubleshoot-vornik, validate-install, report-problem, backlog-deposit, admin, docs. |
| `vornik-companion/scripts/` | The script the backlog-deposit skill runs. |

Start a new opencode session after `connect`.

`connect` refuses, and writes nothing, in two cases:
- `opencode.json` has comments, or is not a JSON object. Your comments would
  be lost, so add the entry from the
  [any-agent guide](any-agent.md#2-add-the-entry-to-your-agent) by hand instead.
- A `plugin/vornik-companion.js` or its guidance file exists that Vornik did
  not write.

`connect` also writes `vornikctl`'s absolute path into the MCP entry and the
plugin. If you move `vornikctl`, run `connect` again; `doctor` reports the
stale path.

**Check:** `vornikctl companion doctor --client opencode` passes every line.
In opencode, ask: "recall what Vornik knows about this repository".

## How the digest works

When a session sends its first request, the plugin runs `vornikctl companion
session-start`, which takes up to 8 seconds. The answer is kept for the
session and added to every request, so it survives a compaction. If Vornik
does not answer in time, the session still gets Vornik's rules, plus a line
telling the model to call `recent_memory` itself.

## Disconnect

```bash
vornikctl companion disconnect opencode
```

It removes the MCP entry, the plugin and its guidance, the `vornik-*` skills,
the `vornik-companion/` scripts and the key file. Your own settings, plugins and skills are left alone. It
revokes nothing; the operator who minted the key revokes it.

## Limits

- **Tested version.** The plugin was tested with opencode 1.14.37, and it uses
  an opencode hook marked experimental (`experimental.chat.system.transform`).
  A later opencode may change that hook. The MCP tools keep working, but
  Vornik's rules and the digest would stop reaching the model. `doctor` warns
  when your opencode version differs from the tested one.
- **Commented configs.** `opencode.jsonc`, or an `opencode.json` with
  comments, gets no automatic entry.
- **Tool names.** opencode shows Vornik's tools as
  `vornik-companion-default_recall` and so on. The skills and rules name them
  bare (`recall`).
