# vornik-companion for Hermes Agent

Vornik as [Hermes Agent](https://hermes-agent.nousresearch.com)'s
**privileged-work broker** and **long-term memory**. Hermes holds no
credentials for your mail, calendar, documents or accounts. When a request
touches them, Hermes runs an operator-approved broker workflow on Vornik and
gets back a bounded, schema-validated result, never raw access.

## Install

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
# `hermes plugins install` takes a catalog name or a Git repository whose
# root is the plugin, so copy this directory into Hermes's plugin folder.
# The directory name is the plugin's name, and the memory provider's name.
cp -r contrib/hermes-companion "${HERMES_HOME:-$HOME/.hermes}/plugins/vornik-companion"
hermes plugins enable vornik-companion
export VORNIK_URL=https://vornik.example.com
export VORNIK_BROKER_TOKEN=sk-vornik-…   # the broker-project key
export VORNIK_MEMORY_TOKEN=sk-vornik-…   # optional: the memory-project key
hermes config set memory.provider vornik-companion   # use Vornik as long-term memory
hermes memory status                     # expect "installed" and "available"
```

Checked against Hermes v2026.9.24 by the end-to-end lane
(`make test-e2e-hermes`, https://docs.vornik.io).
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
| Tools | `vornik_catalog`, `vornik_delegate`, `vornik_result`, `vornik_status`, `vornik_cancel` |
| Skill | `vornik-companion:vornik-broker`: when to broker and how to treat results |
| Hook | `pre_llm_call`: announces finished broker tasks (ids and states only) |
| Commands | `/vornik-peek`, `/vornik-result <task_id>` |
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

## Testing

The unit tests (`python3 -m unittest discover -s tests -t .` from this
directory) run the plugin against a scripted daemon and a fake Hermes context.
`make test-e2e-hermes` runs it inside a pinned Hermes release against a
Vornik daemon built from the tree and a local llama.cpp model. It needs
podman and about 17 GB of downloads the first time (the gpt-oss-20b model and the Hermes image). It is not part of CI, and
`RELEASE=1 make test-e2e-hermes` is the gate before a version bump.
