# Publish a workflow as an A2A expert

Another agent can call one of your workflows over the A2A protocol, ask it a
question, and get an answer back: an **expert**. This page shows how to
publish one and which controls bound what it can say.

**The boundary is the expert's memory.** Every control below limits what the
expert can retrieve or send, but a model can paraphrase anything it
retrieved. Put only material that may be disclosed in the expert's project,
and keep private material in another project.

## 1. Publish the workflow

In the workflow's front matter:

```yaml
a2a:
  publish: true
  projects: [my-expert]        # the projects it is published under
```

`publish: true` lists the workflow in the daemon's agent card
(`/.well-known/agent.json`, which is public by the A2A specification) and
opens `POST /a2a/v1/agents/<project>/<workflow>/tasks` for it. Workflows are a
shared library, so `projects` names where it is published; without it the
workflow is published only under the project whose default workflow it is.

Give the expert its own project, with its own swarm and memory. Do not
publish a workflow from a project whose memory holds private material.

## 2. Give callers a key that reaches only the expert

A caller authenticates with an API key of the expert's project:

```bash
vornikctl key create -p my-expert --name partner-agent
vornikctl key update -p my-expert <key-id> --set-workflows my-expert-answer
```

With a workflow list, the key reaches only the A2A routes of those workflows.
A key from `vornikctl key create` holds no memory grant, so it cannot read
the project's memory directly, and it cannot manage keys. Each task the key submits is recorded against it:

| bound | default | when it trips |
|---|---|---|
| `a2a.dailyQuotaPerKey` on the workflow | 50 tasks per key per UTC day | 429 |
| the key's spend cap | none unless set | 402 once its attributed spend reaches the cap |
| rate | 1 request per second, burst 10, for a key with a workflow list and no rate limit of its own | 429 |

Concurrent submits can pass the daily quota by up to 10.

## 3. Send only the answer: `a2a.answer`

Without an `answer` block, a caller receives every output artifact of the
run. With one, it receives one field set from one artifact:

```yaml
a2a:
  publish: true
  projects: [my-expert]
  answer:
    artifact: result.json        # a bare .json file name the role writes
    fields: [answer, citations]  # the only top-level keys sent
    maxBytes: 8192               # 1 to 65536
    codeBlocks: refuse           # or strip; refuse when omitted
```

The daemon sends the answer only when every check passes, in this order,
and otherwise sends nothing and reports the run as failed:

1. the task completed (a failed or cancelled run sends nothing);
2. the artifact exists and is at most 64 KiB before it is parsed;
3. it is a JSON object;
4. only the listed fields are kept;
5. `citations`, when listed, is an array of strings, each a clean path
   under one of the answering role's `memorySearch.sourcePrefixes` (below);
6. a fenced code block in any kept text field is refused (the run fails),
   or with `codeBlocks: strip` replaced by "[code omitted]";
7. the re-encoded answer is at most `maxBytes`. It is never truncated.

The loader refuses an `artifact` that is not a bare `.json` file name, an
empty or repeated `fields` entry, a `maxBytes` outside 1 to 65536, and a
`codeBlocks` value other than `refuse` or `strip`.

## 4. Narrow what the expert can retrieve: `memorySearch`

On the swarm role that answers, a `memorySearch` policy is applied by the
daemon to every search the role makes; the model cannot widen it:

```yaml
roles:
  - name: consultant
    allowedTools: [memory_search, file_write, current_time]
    memorySearch:
      repoScope: github.com/your-org/your-docs   # required by every field below
      strictScope: true          # drop chunks that carry no repo scope
      authority: provenance      # only files the daemon checked against the trusted branch
      sourcePrefixes: [docs/public/, docs/briefs/]
      maxSensitivity: public     # public, internal or confidential
      limit: 10                  # 0 to 20: hits per call when the call names none, and the most a call may ask for; 0 keeps the tool's default
```

| field | what it admits |
|---|---|
| `repoScope` | chunks of that repository (or `*`) |
| `strictScope` | drops chunks with no repo scope |
| `authority: provenance` | only chunks whose bytes the daemon matched against the trusted branch; `any` (the default) admits every chunk |
| `sourcePrefixes` | only chunks whose source path begins with one of the prefixes; each prefix is path segments ending in `/` |
| `maxSensitivity` | chunks up to that tier; a chunk with no tier counts as confidential unless it is a checked file under the daemon's `memory.public_source_prefixes` |

The loader refuses a policy that sets `strictScope`, `authority`,
`sourcePrefixes` or `maxSensitivity` without `repoScope`, an unknown
`authority` or `maxSensitivity`, a malformed prefix, a `limit` outside 0 to
20, and any narrowing policy on a role that also holds `run_shell` (such a
role could build its own request).

Give the answering role only the tools it needs. A role that can call the
web, a shell or another workflow can reach material its memory policy does
not cover.

## 5. Keep the expert's own output out of its memory

```yaml
ingest_output_artifacts: false
```

on the workflow keeps each run's output files out of the project's memory, so
an answer, or a file the role wrote, does not become something a later
question can retrieve. Input artifacts follow `ingest_input_artifacts` as
before.

## 6. Check it

- `vornikctl doctor` reports `a2a_corpus_walk` when a caller key's A2A tasks
  retrieved more than 500 distinct memory chunks in 24 hours, a sign the
  caller is walking the expert's corpus. It reports; the quota and the rate
  limit bound how fast such a walk can go.
- `vornikctl doctor` reports `memory_sensitivity_cap`: how many chunks each
  capped role can retrieve.
- Ask the expert the questions you expect, as a caller, and read what comes
  back.

What none of this covers: a model can restate, in its own words, anything it
retrieved, and nothing at egress can tell a paraphrase of private material
from an answer. The expert's memory is the boundary.
