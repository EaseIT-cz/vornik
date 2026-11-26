# Vornik companion for opencode

Connects opencode to a Vornik daemon's companion endpoint: project memory
(`recall`, `remember`), async delegation to Vornik workflows, and knowledge
skills, with Vornik's working rules and a session digest in every session.
Design: `https://docs.vornik.io`.

## Install

```bash
printf '%s\n' "$VORNIK_COMPANION_TOKEN" | vornikctl companion connect opencode --key-stdin
```

`connect opencode` checks the key against the daemon, stores it in a file only
you can read, and writes into opencode's config directory
(`$XDG_CONFIG_HOME/opencode`, default `~/.config/opencode`):

- the MCP entry `vornik-companion-<name>` in `opencode.json`, which runs
  `vornikctl companion mcp-bridge`; no opencode config holds the key;
- `plugin/vornik-companion.js` and its `vornik-companion-guidance.md`, which
  append Vornik's working rules and the session digest to the system prompt;
- the companion skills, as `skills/vornik-*`.

`vornikctl companion doctor --client opencode` checks the install;
`vornikctl companion disconnect opencode` removes what connect wrote.

Tested with opencode 1.14.37. The plugin uses opencode's
`experimental.chat.system.transform` hook, which a later opencode may rename.

## Maintaining

The skills are the Codex bundle's, rewritten by `skills-from-codex.sed`. After
editing a Codex skill, run `./sync-skills.sh`; a test fails until you do. Bump
`VERSION` in `plugin/vornik-companion.js` on every change.
