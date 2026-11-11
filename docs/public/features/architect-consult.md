# Architect consultation over A2A

!!! note "Community Edition — use requires a subscription"

    The consultation ships in the **Community Edition**, **off by default**.
    Using it requires a vornik subscription. The gate is your service
    contract, not a licence check in the software. Turning it on is
    recorded in an audit log, and so is every consultation.

The [configuration assistant](config-assistant.md) can ask the **vornik
architect**, a remote expert agent reached over the A2A protocol, one
question per request. It does so when it needs knowledge it does not hold:
what a configuration key means in practice, how a workflow is usually
shaped for a purpose, why a measurement looks the way it does.

## What the architect knows

It answers from two sources only:

- implementation briefs, which describe how each feature works end to end
  and are approved for disclosure before they are added;
- this documentation.

It does not read Vornik's internal design record, legal material, source
code or anyone's instance configuration. When a question would need them,
the answer is "Not found in the design record."

By default an answer is at most 32 KiB of prose
(`config_assistant.consult.max_answer_bytes`); it carries citations and no
code blocks. The 8 KiB figure is the *question* cap
(`config_assistant.consult.max_question_bytes`), not the answer. Every
citation names a brief or a page of these docs. A run that breaks a rule
returns no answer, and the consultation counts as not having happened.

## What is sent

Only a minimized, sanitized question: never your configuration tree, a diff
or a secret. The question passes the same secret screen as the assistant's own
model call and its judge call. A question that trips the screen is not sent,
and the request carries on without the consultation.

The answer is advice, never an approval. It enters the assistant's reasoning
as untrusted text. It cannot grant tools or make a proposal apply itself.

## Bounds

- At most one consultation per assistant request.
- A deadline per consultation (`config_assistant.consult.timeout`, default
  two minutes) and a cap on the answer's size.
- The only destination is the `a2a.peers` entry you configure. The model
  cannot choose a host, and the destination and your consent are checked
  again just before each send.
- An unreachable or timed-out peer degrades the answer: the proposal says the
  consultation did not happen. It never fails the request and is never
  retried on its own.

## Audit

Each attempt is written to the admin audit log before any network call. If
the log is not wired, or the write fails, no call is made. The log holds:
turning the feature on and off, changes of destination, each request's
attempt and outcome, and each project's first attempted and first
successful use. That record is the evidence the contract rests on. Counters
add to the log; they do not replace it.

## Turning it on

```yaml
a2a:
  peers:
    vornik_architect:
      url: https://architect.vornik.cloud/a2a/v1/agents/vornik/architect
      api_key: ${VORNIK_ARCHITECT_API_KEY}
config_assistant:
  consult:
    enabled: true
    peer: vornik_architect
```

```bash
vornikctl doctor feature enable architect-consult
```

The doctor refuses unless the configuration assistant is on, the peer entry
exists and the admin audit log is wired.

To publish your own workflow as an expert with the same controls, see
[Publish a workflow as an A2A expert](../guides/publish-a2a-expert.md).
