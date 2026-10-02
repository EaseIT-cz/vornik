# vornik-companion for Hermes Agent

**A hardened, enterprise-grade broker for [Hermes Agent](https://hermes-agent.nousresearch.com).**
Hermes holds no credentials for your mail, calendar, documents or accounts.
When a request touches them, Hermes runs an operator-approved workflow on
your own [Vornik](https://vornik.io) daemon and gets back a bounded,
schema-validated result, never raw access. Vornik keeps every credential,
and every widening of Hermes's reach (a new connection, a new credential, a
write) is approved by you on your phone. Vornik can also be Hermes's
long-term memory.

## Two ways to use it

| Setup | What Hermes can do | What it needs |
|---|---|---|
| **Admin** | Set up projects, workflows and connections on Vornik for you, each change approved on your phone | `hermes vornik connect` once; no environment variables |
| **Broker** | Run the workflows your operator allowed, and get results | `VORNIK_URL` and `VORNIK_BROKER_TOKEN` (plus `VORNIK_MEMORY_TOKEN` for memory) |

### Admin setup

Install Vornik first (https://docs.vornik.io/getting-started/), sign
`vornikctl` in as the operator (`vornikctl auth login`) and pair your phone
(`vornikctl pair-device`). Then:

```bash
hermes plugins install vornik-companion
hermes plugins enable vornik-companion
hermes vornik connect          # runs: vornikctl agent connect hermes
hermes vornik status           # what is configured, and what the daemon supports
```

`hermes vornik connect` adds one MCP entry to Hermes's config. The plugin's
`vornik-admin` skill teaches Hermes to use it. The setup guide is
https://docs.vornik.io/guides/assistant-setup/.

### Broker setup

On the Vornik host, set up two projects and two keys (see
`configs/examples/broker-mail.yaml` for a complete broker project):

```bash
# the broker project holds the credentials; it has `broker: true` in its config
vornikctl companion grant -p broker-mail --client hermes --workflows mail-digest --budget-usd 5
# Hermes's long-term memory, which can never delegate
vornikctl companion grant -p assistant-memory --client hermes --memory-all --no-delegate
```

On the Hermes host:

```bash
hermes plugins install vornik-companion
hermes plugins enable vornik-companion
export VORNIK_URL=https://vornik.example.com
export VORNIK_BROKER_TOKEN=sk-vornik-…   # the broker-project key
export VORNIK_MEMORY_TOKEN=sk-vornik-…   # optional: the memory-project key
hermes config set memory.provider vornik-companion   # use Vornik as long-term memory
hermes memory status                     # expect "installed" and "available"
```

Checked against Hermes v2026.9.24 (0.21.5) by the end-to-end tests in
`test/e2e/hermes` of this repository.
Before 0.2.1 this README said to choose `vornik`; Hermes names a memory
provider after its plugin directory, so that selected nothing.

By default Hermes hides plugin tools behind its `tool_search` tool, and a
model has to search before it can call `vornik_delegate`. Large models do
this. Small local ones mostly do not. If Hermes never uses the Vornik tools,
offer them directly:

```bash
hermes config set tools.tool_search.enabled off
```

0.2.2 declares `kind: standalone` in `plugin.yaml`. Without it, Hermes
treated the plugin as an exclusive memory plugin and never loaded the broker
tools. It declares `vornik_delegate`'s `inputs` as an open object. Before that,
a model served by llama.cpp could only send `"inputs": {}`. It also reports a note that Vornik's memory gates refuse or hold for
review as an error to the model, instead of a result the model could read
as "saved". Vornik refuses notes under about ten words, so write a fact as
a full sentence with its context.

## What you get

| Surface | Name |
|---|---|
| Tools | `vornik_catalog`, `vornik_delegate`, `vornik_result`, `vornik_status`, `vornik_cancel` (broker setup only) |
| Skill | `vornik-companion:vornik-broker`: when to broker and how to treat results |
| Hook | `pre_llm_call`: announces finished broker tasks (ids and states only) |
| Commands | `/vornik-peek`, `/vornik-result <task_id>` |
| CLI | `hermes vornik connect`, `hermes vornik status` |
| Skill | `vornik-companion:vornik-admin`: how to administer Vornik through the admin setup |
| Memory provider | `vornik-companion`: `vornik_recall` / `vornik_remember`, prefetch before each turn, mirrors Hermes's curated `MEMORY.md` / `USER.md` writes |

## Works with older daemons

On start the plugin reads `/api/v1/capabilities`. The broker tools are offered
only when the daemon advertises `companion-broker`, and `vornik_result` waits
server-side only when it advertises `companion-result-wait`. Against an older
daemon the tools and `/vornik-peek` stay hidden and nothing breaks. The flags
are re-read every 10 minutes, so upgrading the daemon does not need a Hermes
restart.

## Proposed writes (0.2.0)

A broker workflow may **propose** a write, such as a reply to an email, instead
of performing it. Its `vornik_catalog` entry lists `proposes`, and
`vornik_result` / `vornik_status` carry an `actions` list with each write's
state (`pending_approval`, `executed`, `unknown`, and so on). A person approves
or rejects each draft in Vornik's `/inbox`; Hermes cannot approve, and the
skill tells it never to claim a write was sent before its state is
`executed`. The daemon advertises `companion-broker-actions` when writes are
enabled. On a daemon without it no workflow proposes, `actions` never appears,
and the plugin needs no gating. The reverse, a 0.1.0 plugin against a daemon
with writes on, sees an `actions` key its skill does not explain; it relays
results but may not say that a draft awaits approval. Plugins update off the
manifest version, and writes are off until the operator turns them on, so
update the plugin before enabling `broker.writes`.

## What it does on your machine

What you would want to know before installing (Hermes plugin catalog,
rule 13):

- **Network:** it talks only to the Vornik daemon at `VORNIK_URL`, your own.
  It calls no third-party service and sends no telemetry. With `VORNIK_URL`
  unset it opens no connection at all.
- **Before each turn:** the `pre_llm_call` hook asks the daemon which broker
  tasks finished (ids and states only); the memory provider, when enabled,
  prefetches relevant memories.
- **Memory:** when you select it as Hermes's memory provider, it mirrors
  Hermes's curated `MEMORY.md` and `USER.md` writes to Vornik.
- **Credentials:** it reads `VORNIK_BROKER_TOKEN` and `VORNIK_MEMORY_TOKEN`
  from the environment and nothing else. It stores no credential of its own
  and never reads another tool's login.
- **Programs:** `hermes vornik connect`, which you type, runs your local
  `vornikctl` (`vornikctl agent connect hermes`) with a 120-second limit. That
  writes one MCP entry into Hermes's config and a key file only you can
  read. Nothing else in the plugin runs a program: no tool, hook or slash
  command does, so Hermes's model cannot trigger it.
- **Never:** it downloads or installs nothing (without `vornikctl`,
  `connect` prints the install page), updates nothing by itself, runs no
  background process, and never answers an approval for you.

## Licence

This directory is licensed **Apache-2.0** (see `LICENSE`), although the
repository around it is AGPL-3.0: the repository's `LICENSING.md` maps each
directory to its licence. The companion plugins are installed into
third-party tools, so they carry a permissive licence on purpose.

## Testing

The unit tests (`python3 -m unittest discover -s tests -t .` from this
directory) run the plugin against a scripted daemon and a fake Hermes context.
The end-to-end tests in `test/e2e/hermes` (build tag `e2e_hermes`) run it
inside a pinned Hermes release against a Vornik daemon built from the tree and
a local llama.cpp model. They need podman and about 17 GB of downloads the
first time (the gpt-oss-20b model and the Hermes image). They are not part of
CI; the maintainers run them before every version bump of this plugin.
