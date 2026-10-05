# Set up Hermes with Vornik

[Hermes Agent](https://hermes-agent.nousresearch.com) can use Vornik as a
broker: when a request needs your mail, calendar or accounts, Hermes runs a
workflow you chose on Vornik and gets back a short, schema-checked answer.
Hermes never holds the credential. Vornik can also be Hermes's long-term
memory.

This page covers the usual layout: Vornik and Hermes on one machine, each
under its own OS user. It also works with Hermes on another machine (see
[the end of this page](#hermes-on-another-machine)).

!!! warning "If you are Hermes, or another assistant, reading this"
    The steps run as `vornik` below need the operator's admin key and the
    operator's paired phone. **Do not run them yourself.** Give the operator
    the commands for their terminal, and wait for them to tell you the setup
    is done. Only the plugin install in step 4 runs as the Hermes user. Never
    ask for the admin key, never put it in your own environment or files,
    and never mint a key for yourself. A key you mint is a key you chose the
    limits of.

## Two setups

| Setup | What Hermes can do | Who decides what it can reach |
|---|---|---|
| **Broker** (the default) | Run the workflows you allowed, within a budget, and read their answers | You, once, when you grant its key |
| **Admin** (opt-in) | Build its own projects, workflows and connections in its own namespace | You, on your phone, for each change |

Start with the broker. It gives Hermes the least: a fixed list of
workflows, a spending cap, and answers whose shape you approved. Use the
admin setup only when you want Hermes to build automations itself; it is
described in [Let your assistant set up Vornik](assistant-setup.md).

## Who runs what

In this guide `vornik` is the OS user Vornik runs as, and `hermes` is the OS
user Hermes runs as. Use your own names.

| Step | Run as | Why |
|---|---|---|
| Pair your phone, create projects, grant keys | `vornik` (the operator) | Needs the admin key and the daemon's config tree |
| Write Hermes's `.env` | the operator, writing into `hermes`'s home | Hermes reads its tokens from there |
| Install and enable the plugin, check status | `hermes` | The plugin lives in Hermes's home |

Hermes's user runs no `vornikctl` for the broker setup, and never needs
the admin key. The operator writes Hermes's `.env`, but as the `hermes`
user, so the file belongs to Hermes.

## Broker setup

### 1. Two projects: the broker and the memory

The broker key and the memory key live in **two different projects**. A
broker project (`broker: true`) refuses memory and skills on every key, so
nothing Hermes delegates can be recalled later as raw text. Hermes's memory
goes in an ordinary project whose key cannot delegate.

As `vornik`, create the broker project in the daemon's projects directory
(by default `~/.config/vornik/configs/projects/`; check
`vornikctl config show` if you changed it). This one reads mail and never
writes:

```yaml
# ~/.config/vornik/configs/projects/broker-mail.yaml
projectId: "broker-mail"
displayName: "Mail broker"
description: "Read-only mail access for Hermes. Broker workflows only."
swarmId: "broker-swarm"
defaultWorkflowId: "mail-digest"
defaultPriority: 50
maxConcurrentTasks: 2

broker: true

mcp:
  servers:
    # Narrows the daemon-level google-workspace server and declares the
    # narrowing read-only. Vornik cannot verify that claim: list only tools
    # that change nothing.
    - name: "google-workspace"
      broker_read_only: true
      allowed_tools:
        - "time_getCurrentDate"
        - "gmail_search"
        - "gmail_get"

permissions:
  allowedTools:
    - "current_time"
    - "file_write"      # writes the workflow's own result file, not mail
    - "mcp__google-workspace__time_getCurrentDate"
    - "mcp__google-workspace__gmail_search"
    - "mcp__google-workspace__gmail_get"
```

It narrows a `google-workspace` MCP server that must already be configured
at the daemon level with your mail account; see
[Connect your tools (MCP)](mcp-tools.md). `mail-digest` and `broker-swarm`
ship with Vornik (`make install-config-assets` installs them). To let Hermes also draft replies that
you approve before they are sent, see
[Proposed writes](../features/companion.md#proposed-writes-a-person-approves-every-one).

Scaffold the memory project rather than writing it by hand:

```bash
vornikctl init project hermes-memory --config-dir ~/.config/vornik/configs \
    --display-name "Hermes memory"
```

Load both, and check that the daemon accepted them:

```bash
vornikctl config reload
vornikctl config reload-status
vornikctl project list            # expect broker-mail and hermes-memory
```

### 2. Two keys

Still as `vornik`:

```bash
# delegation only, to the workflows you name, within a budget
vornikctl companion grant -p broker-mail --client hermes \
    --workflows mail-digest --budget-usd 5
# memory only: this key can never delegate
vornikctl companion grant -p hermes-memory --client hermes \
    --memory-all --no-delegate
```

Each prints its secret once. Grant nothing else to a Hermes key:

- no `--skill-all` or other skill flags: the knowledge-skill store is for
  coding assistants;
- no `companion-*` workflows: those are developer workflows
  (reviews, ingest) for the coding-assistant companion, not for Hermes;
- no memory flags on the broker key: grant refuses them on a broker project,
  and that refusal is the design.

The generic recipe in [Companion plugin → Setting it up](../features/companion.md#setting-it-up)
is for Claude Code and Codex; do not adapt it for Hermes.

### 3. Hand the keys to Hermes

Hermes loads `$HERMES_HOME/.env` (by default `~/.hermes/.env`) into its
environment every time it starts, including as a service (read in Hermes
v2026.9.24, 0.21.5; if your Hermes is a different version and
`hermes vornik status` shows the tokens unset, check its own docs on
`.env`). Put the URL and
both keys there, not in an `export` in some shell: a gateway started by
systemd, or after a reboot, never sees a shell's exports.

From the operator's terminal, open the file in an editor as `hermes`, so
the secrets go into neither Hermes's conversation nor your shell history:

```bash
sudo -u hermes -H sh -c 'umask 077; mkdir -p ~/.hermes; touch ~/.hermes/.env; chmod 600 ~/.hermes/.env; exec vi ~/.hermes/.env'
```

(`sudo` drops your `EDITOR`; put another editor in place of `vi` if you
prefer.)

and add:

```
VORNIK_URL=http://localhost:8080
VORNIK_BROKER_TOKEN=<the broker-mail secret>
VORNIK_MEMORY_TOKEN=<the hermes-memory secret>
```

If Hermes runs with `HERMES_HOME` set to another directory (a service
often does), edit `.env` there instead.

`http://localhost:8080` is right when both users are on the same machine
and Vornik listens on its default port. Plain `http` is only acceptable on
loopback; for any other host use `https`.

### 4. Install the plugin

As `hermes`:

```bash
hermes plugins install vornik-companion
hermes plugins enable vornik-companion
hermes config set memory.provider vornik-companion   # Vornik as long-term memory
hermes vornik status        # URL, which tokens are set, the daemon's capabilities
hermes memory status        # expect "installed" and "available"
```

Restart Hermes (or its gateway) so it reads the new `.env`. Then ask it to
list what it can run: it should name `mail-digest` and nothing else.

## Admin setup (opt-in)

Read [Let your assistant set up Vornik](assistant-setup.md) first; it
explains what an administering assistant can and cannot do. With Hermes on
its own OS user, two things differ from that page.

**Hermes's user needs its own `vornikctl`.** Connect writes the path of
the `vornikctl` it runs into Hermes's config, and Hermes runs that path
every time it starts (the MCP bridge) and for every approval it sends to
your phone. A `vornikctl` inside `vornik`'s home is usually not executable
by `hermes`, and the entry then fails at every start. Install a copy where
`hermes` can run it and where it stays put, then always run that copy:

```bash
sudo install -m 0755 "$(command -v vornikctl)" /usr/local/bin/vornikctl
```

When you upgrade Vornik, replace the file at the same path; the recorded
path then stays valid.

**Connect needs the admin key once, from your terminal.** The sign-in from
`vornikctl auth login` lives in `vornik`'s home, so `hermes`'s `vornikctl`
is not signed in. Give the key to that one command only, typed by you, so
it is in neither a command line nor a shell history, and not in anything
Hermes can read:

```bash
read -rs VORNIK_API_KEY && export VORNIK_API_KEY     # paste the admin key
# sudo drops your environment; --preserve-env carries this one variable
sudo --preserve-env=VORNIK_API_KEY -u hermes -H \
    /usr/local/bin/vornikctl agent connect hermes --dry-run
sudo --preserve-env=VORNIK_API_KEY -u hermes -H \
    /usr/local/bin/vornikctl agent connect hermes
unset VORNIK_API_KEY
```

Do not run `hermes vornik connect` with the admin key in Hermes's own
environment: it passes its environment to `vornikctl`, and that environment
is Hermes's. Connect stores the assistant's own key in
`~hermes/.config/vornik/agents/hermes.key`, readable by `hermes` only; the
admin key is not stored anywhere.

## Hermes on another machine

The broker setup is the same, with two changes: `VORNIK_URL` is Vornik's
`https://` address (a tunnel or a reverse proxy with a certificate), and
you carry the two secrets to the Hermes machine yourself: sign in there as
the Hermes user and write `.env` with the same `umask 077` and mode 600. For the admin
setup, follow [Vornik on another machine](assistant-setup.md#vornik-on-another-machine).

## When it does not work

| What you see | Why | Fix |
|---|---|---|
| `hermes vornik status` says a token is not set | It was exported in a shell, not written to `.env`, or Hermes was not restarted | Step 3, then restart Hermes |
| Hermes says it has no Vornik tools | The daemon is older than the broker (no `companion-broker` capability), or Hermes hides plugin tools behind `tool_search` | Check `hermes vornik status`; for small local models, `hermes config set tools.tool_search.enabled off` |
| `vornikctl companion grant` refuses `--memory-all` | Memory on a broker project is refused by design | Grant memory on the separate memory project |
| `vornikctl companion grant` says unauthorized | It ran without the admin key, for example as `hermes` | Run it as `vornik`, signed in |
| Admin setup: the Vornik MCP entry fails at every Hermes start | The `vornikctl` path connect recorded is not executable by `hermes` | Install `vornikctl` in `/usr/local/bin` and connect again with that copy |
| Admin setup: connect says it cannot reach Vornik, or is unauthorized | `hermes`'s `vornikctl` has no admin key, or a different URL | Pass the key as shown above; add `--url` if Vornik is not on `localhost:8080` |
| A memory note is refused | Vornik refuses notes under about ten words | Write the fact as a full sentence with its context |

More on the plugin itself, including what it does on your machine and how
forgetting works, is on its catalog page in the Hermes plugin catalog and in
the [Companion plugin](../features/companion.md#broker-projects-a-front-end-agent-without-the-keys)
page.
