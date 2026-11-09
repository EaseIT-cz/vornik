# Use Vornik from Claude Desktop and Cowork

Claude Desktop and Cowork can use Vornik's project memory (`recall`,
`remember`) and delegate work to it, the way the Claude Code plugin does. One
command connects them. It needs no Node or Python, and no token goes into
Claude's config files.

## What you need

- A companion key, `sk-vornik-…`, for your project. An operator mints it on
  the Vornik host with `vornikctl companion grant` (see
  [Companion](../features/companion.md#setting-it-up)).
- `vornikctl` on the machine that runs Claude Desktop (see
  [Getting started](../getting-started.md)).
- Vornik's address, for example `https://vornik.example.com`. Over the
  network it must be `https://`; plain `http://` works only on the same
  machine.

## Connect

```bash
printf '%s\n' "$VORNIK_COMPANION_TOKEN" | \
  vornikctl companion connect claude-desktop --key-stdin --url https://vornik.example.com
```

`connect` checks the key against Vornik, then:

- stores it in `~/.config/vornik/companion/default.key` (macOS:
  `~/Library/Application Support/vornik/companion/default.key`), readable
  only by you;
- adds one entry, `vornik-companion-default`, to Claude Desktop's config. The
  entry runs `vornikctl companion mcp-bridge`, which reads that file. Your
  other servers stay as they are.

It prints the project the key belongs to. Restart Claude Desktop, then ask it
to recall something from Vornik. Cowork uses the same entry: Claude Desktop
starts the bridge, so Cowork's sandbox does not need to reach Vornik itself.

Without `--key-stdin`, `connect` reads `VORNIK_COMPANION_TOKEN`. Claude
Desktop itself never reads that variable: apps started from the Dock or the
Start menu do not see your shell profile, which is why the key goes into a
file.

To keep two keys (two projects, or two Vornik installs), give each a name:
`--name work`, `--name home`. Each gets its own file and entry.

## Check it

```bash
vornikctl companion doctor --client claude-desktop
```

One line per check, each failure with its fix:

| failure | what it means |
|---|---|
| no entry in Claude Desktop's config | run `connect` |
| an entry runs `npx` to reach Vornik | `npx` needs Node, which many machines do not have; remove that entry and run `connect` |
| the key file is missing or readable by others | run `connect` again |
| Vornik does not answer | check the address, and that the daemon runs |
| the key is refused (401) | the key is wrong, revoked, or not a companion key; ask the operator for a new one |
| an agent-admin key | it administers Vornik and has no memory tools; use `vornikctl agent connect claude-desktop` for that |

## Disconnect

```bash
vornikctl companion disconnect claude-desktop
```

This removes the entry and the key file. It does not revoke the key; the
operator who minted it can (`vornikctl companion keys`).

## Claude Team and Enterprise

An org-wide connector, set up once by an org owner for every member, is
planned. Until then, each member connects with the steps above.
