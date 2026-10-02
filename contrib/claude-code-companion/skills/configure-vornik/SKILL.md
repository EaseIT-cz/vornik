---
name: configure-vornik
description: |
  Teaches Claude how to help an operator configure a Vornik deployment —
  daemon settings, projects, swarms, workflows, models, secrets, channels.
  Use this skill whenever the user wants to set up, change, or add something
  to Vornik's configuration: "add a project", "change the model", "connect a
  channel", "where do I put this API key", "how do I turn X on", and
  "connect Claude / Codex / Hermes to Vornik", "let my assistant use Vornik
  as a broker", "pair my phone". The core
  discipline is: find the tree the running daemon actually reads, scaffold
  instead of hand-writing YAML, then validate → reload → CONFIRM. Most failed
  Vornik config changes are not wrong YAML — they are correct YAML written to
  a directory the daemon never looks at.
---

# Configuring Vornik

Vornik's configuration lives in two places: a daemon config file, and a
**registry tree** holding `projects/`, `swarms/`, and `workflows/`. Changing
either is easy. Changing the *right copy* of either is where operators lose
afternoons.

Your job is to locate the live configuration before touching anything, use
the scaffolding commands rather than hand-authoring YAML, and then **prove**
the change took effect. "I edited the file and reloaded" is not proof.

## Step 1 — Find the tree the daemon actually reads

Do this first, every time, even if the user seems sure. Read the paths off
the running unit rather than assuming defaults:

```
systemctl --user show vornik.service \
    --property=Environment --property=EnvironmentFile
```

Drop `--user` for a packaged system-unit install. That gives you
`VORNIK_CONFIG` (the daemon config file), `VORNIK_DATA_DIR`, and the
`EnvironmentFile=` paths that carry secrets. Confirm the daemon's own view
with:

```
vornikctl config show                              # effective config, secrets redacted
vornikctl config show --provenance --json=false    # ...and WHERE each value came from
vornikctl config show --trees --json=false         # which file supplied each project/swarm/workflow/role
```

"Effective" is the point — it reflects what the daemon actually loaded,
which is the only claim worth making. `--provenance` is the fastest answer to
"did the daemon read my change": every key prints with its origin — `file`,
`env`, `placeholder`, `alias`, `default`, `derived`, `secret_file`, or
`unset`, the zero value nothing touched, which is exactly how a key written to
a tree the daemon never reads looks. `env_invalid` names a variable that was
set and did not parse. `--trees` lists every registry object with the file and
layer that supplied it, and the files the strict loader REFUSED with the
decoder's error — a project with a typo'd key is in that list, not in
`project list`. Both follow a hot reload (the dump did not, before 2026-09-04).

The **registry tree is not a single environment variable**. It is resolved
by a fallback chain, and the daemon's chain is not identical to
`vornikctl`'s. Both begin with `VORNIK_CONFIGS_DIR`, then try
`<dir-of-config>/configs` and `<dir-of-config>`. `vornikctl` additionally
tries `~/.config/vornik/configs` and `/etc/vornik/configs`. Both end at a
*relative* `configs`. If the daemon's chain misses entirely, it resolves to
no registry tree at all — and loads **no** projects, swarms, or workflows.

Three hazards follow from that, and all three fail *silently*:

**The two-trees trap.** A checkout of the Vornik repo, or an older source
config directory, is not necessarily what the daemon reads. Editing a source
copy produces a confident "done" and zero behaviour change. Confirm the path
you are about to edit is the one the running daemon resolved.

**`VORNIK_CONFIGS_DIR` is validated, not trusted.** It is honoured only if
that directory *already* contains all three of `projects/`, `swarms/`, and
`workflows/`. Point it at a directory missing any one of them and it is
skipped — no error, no warning — and resolution falls through to the next
candidate. Whenever the symptom is "my environment variable is set, why is
it reading the wrong tree", check this first.

**The cwd trap.** When everything else misses, resolution falls back to a
relative `configs`, evaluated against whatever directory the process is in.
For `vornikctl` that is the user's shell; the same command gives different
answers from `~` than from a repo checkout. Never run config-mutating
`vornikctl` commands from a directory that happens to contain a `configs/`.

## Step 1a — Point `vornikctl` at the right daemon

Every command below talks to a daemon over HTTP, and **`vornikctl` resolves which
one from `VORNIK_API_URL`**. Unset, it falls back to `http://localhost:8080`.

On a single-host deployment that default **is the production daemon**, so a command
you believed was aimed at a test instance lands on production instead. The failure is
quiet in the worst way: read-only commands return production's data (looking merely
"wrong"), and a mutating command like `vornikctl companion grant` succeeds against
production if the project name happens to exist there.

```
echo "${VORNIK_API_URL:-http://localhost:8080 (default!)}"
vornikctl config show | head          # confirm it is the daemon you meant
```

**Do not confuse it with `VORNIK_URL`.** That is a *different* variable, read by
companion/bench clients rather than by `vornikctl`. Setting only `VORNIK_URL` and
assuming the CLI follows is a real and easy mistake — the CLI silently keeps using
its own default. When you operate more than one instance on a host, set
`VORNIK_API_URL` explicitly per command rather than exporting it, so no later command
inherits a target you have stopped thinking about.

## Step 2 — Scaffold, don't hand-write

```
vornikctl init project      # project YAML
vornikctl init swarm        # SWARM.md from a preset template
```

These emit valid, current-schema files. Hand-written project and swarm files
are the single most common source of validation failures, because the schema
moves and a model's memory of it does not.

`vornikctl init` resolves the registry tree against **your shell**, not the
daemon's unit — and the unit's environment is usually absent from an
interactive shell. On a non-default install it will happily write a
perfectly valid file into a tree the daemon never reads. Export
`VORNIK_CONFIGS_DIR` to match the daemon before scaffolding, and verify
where the file actually landed afterwards.

## Step 3 — Secrets never go in the config file

Config values reference environment variables; the literal secret lives in
an env file the unit loads via `EnvironmentFile=`. A config file carrying a
plaintext token is a finding, not a working setup — `vornikctl doctor`
reports it under `config_secret_hygiene`, and loose file permissions under
`secrets_permissions`.

## Step 4 — The apply loop

```
vornikctl doctor              # config_validation catches schema errors first
vornikctl config reload
vornikctl config reload-status
```

`config reload-status` is where the last reload's outcome and any validation
errors surface. A reload that appears to succeed but changed nothing is
diagnosed *there*, not from the reload command's own output. Never report a
config change as applied without reading it.

## Step 5 — When a reload is not enough

The reload path covers the config file and the registry tree. It does **not**
cover the daemon's environment. systemd resolves both `Environment=` and
`EnvironmentFile=` at `ExecStart` only, so any unit environment change — a
secret in an env file, a `VORNIK_*` knob on the unit, a drop-in override — is
invisible to the running daemon until a **restart**.

`vornikctl doctor` flags the env-file half of this as `env_file_freshness`
(it catches `EnvironmentFile=` entries modified after daemon start). It does
**not** catch an edited `Environment=` line or a drop-in override — there is
no check for those at all. A clean `env_file_freshness` therefore does not
mean "the environment is fresh". After changing the unit itself, restart;
don't wait for a check to tell you to.

Rule of thumb: edits *inside* the config file or registry tree reload; edits
to what the unit hands the process do not.

## Step 6 — The restart guard

**Never restart the daemon while tasks are `RUNNING` or `LEASED`.** Check
first and wait for an idle window:

```
vornikctl task list
```

A restart mid-task loses in-flight work. If a restart is genuinely required
and tasks are live, say so plainly and let the operator choose when.

## Step 7 — Verify, then say what you did

Re-run `vornikctl config show --provenance --json=false` and check the keys
you changed read `file` (or `env`, if that was the intent) rather than
`default` or `unset`; for registry changes, `--trees` shows the object's file
and layer, and confirm it is actually being served:

```
vornikctl project list      # projects the daemon is serving
vornikctl swarm list
```

An object that is absent from these lists did not load, however good the YAML
looks on disk.

## Connecting an assistant to Vornik (agent administration)

Use this when the user wants an assistant (Claude Code, including the desktop
app's Code-tab routines; Claude Desktop; Codex; Hermes) to set up and run its
own automations through Vornik as a broker: Vornik holds the credentials,
the assistant gets only approved results, and a paired phone approves every
widening of what the assistant can reach.

The assistant does the setup work itself afterwards, through MCP tools Vornik
serves (`create_project`, `define_workflow`, `add_api`, `request_credential`,
…). Your part is the one-time connection, and two steps of it belong to a
**person, never to an assistant**.

### The two steps an assistant must never do

- **Pairing the phone.** `vornikctl pair-device` prints a one-time code, and the
  first phone is paired by that code alone. An assistant that runs it, or sees
  its output, can redeem the code itself and become the approver for its own
  requests. A person runs it on the Vornik host and types the code on the phone.
- **Minting the key.** `vornikctl agent connect` needs the operator's admin key
  for that one command. Run it from a terminal the assistant does not control,
  with the key in that command's environment only
  (`VORNIK_API_KEY=… vornikctl agent connect …`). Never in a file, a shell
  profile, or the chat.

If the assistant has a shell or SSH access to the Vornik host as the daemon's
user, no Vornik control can hold: it can read the secret store and the
database directly. Remove that access, or give it a different OS user with no
access to Vornik's files, before connecting. Say this to the user plainly; do
not soften it.

### Steps

1. **Server ready.** On the Vornik host, the agent templates must be deployed:
   - a source install: `make install-config-assets`;
   - a CE install updated with `vornik-update.sh` from 2026.10.2 on: the updater deploys them;
   - an older install: re-run the quickstart.

   Agent administration is on by default (`agent_admin.enabled`). For workflows
   that propose writes (send mail, create drafts), set `broker.writes: "on"`
   and reload.
2. **Phone.** A person runs `vornikctl pair-device --label "<name>"` on the
   Vornik host and enters the code on the phone, at the address it prints. The
   phone must reach Vornik's public address.
3. **Connect, on the machine where the assistant runs, as the user it runs as.**
   - Which harness:
     - Claude Code, including the desktop app's Code-tab routines: `claude-code`;
     - Claude Desktop chat and Cowork tasks that run locally: `claude-desktop`;
     - Codex: `codex`;
     - Hermes: `hermes`.
   - When Vornik is on another machine, pass its HTTPS URL; connect refuses plain
     HTTP to a remote host. A tunnel or a reverse proxy with a certificate both
     work.

   ```
   VORNIK_API_KEY=<operator admin key> vornikctl agent connect claude-code --url https://vornik.example --dry-run
   VORNIK_API_KEY=<operator admin key> vornikctl agent connect claude-code --url https://vornik.example
   ```

   On a Mac, use the native `vornikctl` from the release assets (darwin/arm64 or
   darwin/amd64; verify `checksums.txt`, then
   `xattr -d com.apple.quarantine vornikctl`, since it is unsigned). The Lima
   install's `vornikctl` is a shim into its VM and cannot do this.

   For a shell-capable assistant (Claude Code, Codex), connect refuses when it
   finds Vornik's secret-store key readable, or cannot check because Vornik is
   on another machine. In the remote case pass `--accept-shared-user` only
   after confirming the assistant has no shell or SSH access to the Vornik
   host as the daemon's user (see above). On a same-host install, pass it only
   for a test install.
4. **Restart the assistant** so it loads the new MCP entry `vornik-<namespace>`.
5. **Verify.** Ask the assistant "What can you do with Vornik?". It should call
   `describe_installation` and explain that approvals happen on the phone. The
   operator page `/ui/admin/agents` lists the assistant.

### Afterwards

- Credentials are entered **on the phone**, never in the chat. If the
  assistant asks for a key in the chat, it is not using Vornik; tell it to
  request the credential through Vornik.
- Scheduled automations can either live in the assistant (it calls `delegate`
  and `result` on its Vornik workflow) or in Vornik (a workflow `schedule`,
  approved on the phone). Vornik's schedule runs even when the assistant's
  machine is off.
- Disconnect: `vornikctl agent disconnect <namespace>` on the same machine.
  It revokes the key and removes the entry.
- Guide for the user: `docs/public/guides/assistant-setup.md` (docs.vornik.io,
  Guides → Let your assistant set up Vornik).

## Edition note

Enterprise Edition adds a point-and-click integrations UI with credential
probing for channel and integration setup. The CLI loop above is the path
that works on every edition.

## Anti-patterns

- **Don't** edit a repo checkout or a stale source tree instead of the one
  the daemon resolved. This silently no-ops every subsequent step.
- **Don't** assume `VORNIK_CONFIGS_DIR` took effect because it is set — it is
  skipped without complaint when the layout check fails.
- **Don't** run `vornikctl init` from an arbitrary directory and assume the
  output landed where the daemon reads.
- **Don't** write a literal token, key, or password into the config file.
- **Don't** hand-author project or swarm YAML when `vornikctl init` exists.
- **Don't** restart to apply a change a reload would carry — and especially
  not while tasks are live.
- **Don't** claim a change is applied without reading `config reload-status`
  and confirming the object is served.

## When it still doesn't work

If the change validated, reloaded, and still has no effect, stop configuring
and start diagnosing — use the **troubleshoot-vornik** skill. Its symptom E
(an object missing entirely) and symptom D (a change that didn't take) both
route back to the resolution chain in step 1.

---

*Commands in this skill were verified against `vornikctl` 2026.7.4. If one is
rejected as unknown, your daemon is likely older or newer than this skill —
check `vornikctl <command> --help` and trust the CLI over this document.*
