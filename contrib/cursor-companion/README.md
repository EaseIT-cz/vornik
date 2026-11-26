# Vornik companion for Cursor

Connects Cursor to a Vornik daemon's companion endpoint: project memory
(`recall`, `remember`), async delegation to Vornik workflows, and knowledge
skills. Design: `https://docs.vornik.io`.

## Install

```bash
printf '%s\n' "$VORNIK_COMPANION_TOKEN" | vornikctl companion connect cursor --key-stdin
```

`connect cursor` checks the key against the daemon, stores it in a file only
you can read, and installs this plugin into
`~/.cursor/plugins/local/vornik-companion`. Reload the Cursor window after.
`vornikctl companion doctor --client cursor` checks the install;
`vornikctl companion disconnect cursor` removes it.

## What the plugin adds

- **MCP server `vornik`**: runs `vornikctl companion mcp-bridge`, which reads
  the key from its file. No Cursor config holds the key.
- **Rule `vornik.mdc`** (always applied): Vornik's working rules, the same
  text the daemon sends every companion session.
- **Session hook**: a `sessionStart` hook runs `vornikctl companion
  session-start`, which adds your outstanding delegations and recent project
  memory to a new conversation. When the hook does not run (cloud agents),
  the rule tells the model to call `recent_memory` itself.
- **Skills**: delegate, knowledge, develop-with-memory, configure-vornik,
  troubleshoot-vornik, validate-install, report-problem, backlog-deposit,
  vornik-admin and vornik-docs.

## Not covered

- Cursor cloud agents cannot run a local `vornikctl`; see the Cursor guide in
  the Vornik docs for a remote entry.
- The marketplace listing comes later.

## Maintaining

The skills are the Codex bundle's, rewritten by `skills-from-codex.sed`. After
editing a Codex skill, run `./sync-skills.sh`; a test fails until you do.
Bump `version` in `.cursor-plugin/plugin.json` on every change.
