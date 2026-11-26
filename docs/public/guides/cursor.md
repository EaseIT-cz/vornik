# Use Vornik from Cursor

One command connects Cursor to Vornik's companion: project memory (`recall`,
`remember`), work handed to Vornik (`delegate`, `result`), and Vornik's
working rules in every chat. The key stays in a file only you can read; no
Cursor config holds it.

**What is measured, and what is not.** The plugin's MCP entry and its session
hook were run against Vornik's companion endpoint exactly as installed, in
Vornik's own tests. Cursor itself has not run them yet: what Cursor does with
the plugin is taken from Cursor's documentation, and this page says where.

## What you need

- `vornikctl` on the machine where Cursor runs (Linux or macOS).
- A companion key, `sk-vornik-…`, minted with a default repo scope. An
  operator mints it on the Vornik host:

  ```bash
  vornikctl companion grant --project=<project> --client=cursor \
    --label="$(hostname)/$USER" --memory-all --skill-read \
    --workflows=companion-research-gather,companion-report-summarize \
    --budget-usd=10 --repo-scope=github.com/ORG/REPO
  ```

  The repo scope matters: Cursor has no reliable hook to pass one, so the
  key's default is the only scope your notes get. `connect` refuses a key
  without one unless you pass `--allow-unscoped`.

## Connect

```bash
printf '%s\n' "$VORNIK_COMPANION_TOKEN" | \
  vornikctl companion connect cursor --key-stdin --url https://vornik.example
```

It checks the key against Vornik, stores it in a file only you can read
(`~/.config/vornik/companion/default.key` on Linux), and installs the plugin into
`~/.cursor/plugins/local/vornik-companion`. Then reload the Cursor window
(Developer: Reload Window).

`connect` writes `vornikctl`'s absolute path into the plugin's MCP entry and
session hook. If you move `vornikctl`, run `connect` again; `doctor` reports
the stale path. A plugin copied by hand instead needs `vornikctl` on the
PATH Cursor starts commands with.

**Check:** `vornikctl companion doctor --client cursor` passes every line. In
Cursor, ask: "recall what Vornik knows about this repository".

## What the plugin adds

| Part | What it does |
|---|---|
| MCP server `vornik` | Runs `vornikctl companion mcp-bridge`, which reads the key file. |
| Rule `vornik.mdc` (always applied) | Vornik's working rules: when to delegate, recall and remember. |
| Skills | delegate, knowledge, develop-with-memory, configure-vornik, troubleshoot-vornik, validate-install, report-problem, backlog-deposit, vornik-admin, vornik-docs. |
| `sessionStart` hook | Adds your delegations and recent project memory to a new chat. When it does not run, the rule tells the model to call `recent_memory`. |

## Cloud agents

A Cursor cloud agent cannot run a local `vornikctl`, and Cursor runs no
`sessionStart` hook for it. Give it a remote MCP entry that reads the key from
the cloud agent's environment:

```json
{
  "mcpServers": {
    "vornik": {
      "url": "https://vornik.example/api/v1/mcp/companion",
      "headers": { "Authorization": "Bearer ${env:VORNIK_COMPANION_TOKEN}" }
    }
  }
}
```

Whether a cloud agent expands `${env:…}` in a header has not been checked;
Cursor's forum reports that some builds send it unexpanded. Never paste the
key itself into the entry: that puts it in Cursor's config.

## Disconnect

```bash
vornikctl companion disconnect cursor
```

It removes the plugin and the key file. It revokes nothing; the operator who
minted the key revokes it (`vornikctl companion keys`).

## Not yet

- The plugin is not in Cursor's marketplace yet; `connect` installs it
  locally.
- Tool names: Cursor shows Vornik's tools under the `vornik` server, and
  the plugin names them bare (`recall`, `delegate`).
