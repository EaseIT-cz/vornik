---
sources:
    - path: internal/config/config.go
      sha256: 902d27206d07d18368cbbb8ac14251cf688d6c20dbb7482bf9546333d36bf164
    - path: internal/executor/executor.go
      sha256: c281ff12df80cf1a4eb409f19044be54dff5678cf5cc70f03ae69b22c376b28b
---
# Named secrets

Named secrets give a project's agents a credential as an ordinary environment
variable, only in the projects you allow. The key stays out of project
documents and workflow definitions.

## Declaring secrets

Add a `named_secrets` list to the daemon configuration:

```yaml
named_secrets:
  - name: WEATHER_API_KEY
    value: "${WEATHER_API_KEY}"     # resolved when the configuration loads
    allowed_projects:
      - forecast-bot
  - name: SHARED_ANALYTICS_TOKEN
    value: "${ANALYTICS_TOKEN}"
    allowed_projects: []            # empty = available to every project
```

| Field | Meaning |
|---|---|
| `name` | The environment-variable name the agent will see. |
| `value` | The secret value. Supports `${VAR}` placeholders (below). |
| `allowed_projects` | Projects whose agents receive this secret. Empty = all projects. |

## Keep the value out of the file

Write a `${NAME}` placeholder instead of the value. When the configuration
loads, each placeholder resolves from the first of:

1. the daemon's environment, when it holds a non-empty value;
2. the **credential store**: the operator credential named `NAME`, stored
   encrypted with the store key (`secrets/store.key`);
3. nothing: the value is empty.

Store a value in the credential store from a shell (the value is read from
standard input, never an argument), or on the console's Credentials page:

```bash
printf '%s' "$WEATHER_KEY" | vornikctl credential set WEATHER_API_KEY
```

A placeholder resolves when the configuration loads. Named secrets are given
to the executor at start, so a new or rotated value reaches agents after a
daemon restart (`systemctl --user restart vornik`).

When the environment and the store hold different values for one name, the
environment wins, and the daemon logs `credential NAME: the environment value
shadows the stored one` at start and at each reload (never a value).
`vornikctl config show --provenance` shows origin `credential` for a key the
store filled.

The `database:` block never reads the store: the database is what opens it.
Its placeholders resolve from the environment and the env files below.

### Where the env files are read from

Before resolving placeholders, vornik sources these files itself, so `${VAR}`
resolves the same way for the daemon and for `vornikctl`, which has no systemd
`EnvironmentFile` of its own:

1. `<config-dir>/vornik.env`: the file the podman quickstart seeds
2. `<config-dir>/secrets/env`
3. `<config-dir>/secrets/*.env`: one file per service, alphabetical

A variable already present and non-empty in the real environment always wins,
so an explicit `export` or a systemd `EnvironmentFile` still overrides these
files. Earlier entries in the list win over later ones.

These files are plaintext. To move the secrets your configuration uses into
the store, run `vornikctl credential import-env`: it lists each name it would
move and each it leaves, with the reason, and never prints a value. Run it
again with `--apply` to store them and remove their lines. Names the
`database:` block uses stay on disk. `vornikctl doctor` reports the secrets
still in a file (check `credentials_in_env_files`).

If a required setting comes out empty because its placeholder had nothing to
resolve to, the error names the variables involved:

```
configuration validation failed: database name is required (unset config
placeholders: ${POSTGRES_DB}, ${POSTGRES_USER} — set them in
<configDir>/vornik.env or <configDir>/secrets/*.env, or export them before
running)
```

## Scoping with `allowed_projects`

`allowed_projects` controls which projects' agents receive each secret:

- **Empty list** → every project's agents get the variable.
- **Non-empty list** → only the named projects' agents get it; agents in any
  other project never see the variable at all.

Scope every secret as narrowly as the work requires — a project that doesn't
need a credential should never receive it.

## How the agent sees it

For an allowed project, the secret is injected into the agent container as an
environment variable named exactly by `name`. An agent reads it the usual way
(for example, `os.Getenv("WEATHER_API_KEY")`). When the same variable name is
set in more than one place, per-task environment overrides a named secret,
which overrides a role's static environment.

Named secrets are declared in the daemon configuration only; their values can
come from the credential store (above).

# Carrying tool-issued credentials to you

Named secrets push credentials *into* an agent. The opposite problem is a
credential that comes *out* of a tool: some tools mint an access credential for
the artifact they produce — canonically PageDrop, whose publish tools return a
**viewing password** for the page. You need that password, but the outbound
secret-redactor (which scrubs agent chat replies and notifications) treats a
password-shaped value like any other secret and replaces it with
`[REDACTED:…]` before it reaches you.

**Credential carryover** closes that gap. The daemon captures the credential
**deterministically from the trusted tool's own output** — no model decides
what's a credential — stores it against the task, and surfaces it
**code-formatted and one-tap-copyable** in the task's completion notification
(Telegram) and on the task-detail page, instead of redacting it. The
notification carries the credential to **private chats only**. A group chat
watching the task gets a note and a link to the task page instead, so the
group's members never see it.

## Enabling it

Add a `tool_credentials` mapping under `secrets`. Each entry names a tool (by
prefix) and how to extract the credential from its output — either a **text
pattern** (a regexp with one capture group, for tools that return prose) or a
**JSON field** (a dotted path, for tools that return a JSON object):

```yaml
secrets:
  # No trusted_output_tools entry is needed: the tool-audit log keeps the
  # captured value redacted, and its one copy is the sealed one below.
  tool_credentials:
    # PageDrop returns prose ("View: <url> … Password: <pw>"), so use patterns:
    - tool: "mcp__pagedrop__pagedrop_"      # prefix — matches publish/republish
      credential_pattern: "Password:\\s*(\\S+)"
      artifact_pattern: "View:\\s*(\\S+)"    # optional; links the credential to a URL
      label: "viewing password"             # operator-facing name
    # A tool that returns JSON would use field paths instead:
    # - tool: "mcp__example__publish"
    #   credential_field: "data.password"   # dotted path; no array indexing
    #   artifact_field: "url"
    #   label: "access token"
```

| Key | Meaning |
|---|---|
| `tool` | Tool-name prefix whose output carries the credential. |
| `credential_pattern` | Regexp (one capture group) extracting the credential from text output. Takes precedence over `credential_field`. |
| `artifact_pattern` | Optional regexp (one capture group) for the URL the credential unlocks. |
| `credential_field` | Dotted path into a JSON result to the credential (e.g. `data.password`). No array indexing. |
| `artifact_field` | Optional dotted path to the artifact URL. |
| `label` | Operator-facing name shown next to the value (defaults to `credential`). |

With no `tool_credentials` entries (the default) nothing is captured and
behaviour is unchanged.

## What it does and doesn't do

- **The model is never involved** in deciding what's a credential — capture is
  driven by the operator-configured tool + field/pattern, reading the tool's
  daemon-proxied output that an agent cannot forge.
- **Strong keys are never captured.** A value matching a strong, prefix-anchored
  pattern (OpenAI / Anthropic / GitHub / AWS / JWT / …) is refused even from a
  configured tool and always redacts — only the lower-confidence
  "viewing-password" shape is eligible. So a misconfigured mapping can't turn a
  real API key into a carryover, and it isn't an exfiltration channel.
- **Retries don't leak stale credentials.** Only the task's most recent
  execution's credential is surfaced; an earlier attempt's password is hidden.
- **The value is sealed at rest.** It is stored with the task, encrypted
  with the secret store key (`store.key`, the key that protects OAuth tokens
  and agent secrets), and removed when the task is purged. The tool-audit log
  shows `[REDACTED:carried_credential]` in its place. Back the key file up
  beside the database: without it, stored credentials cannot be read (a
  re-publish issues a new one).
- **Capture needs secret scanning.** With no secrets detector, nothing is
  captured, since the strong-key check above could not run.
