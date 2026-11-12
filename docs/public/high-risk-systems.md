# Building a high-risk system on Vornik

**Last updated:** 12 November 2026

This page is for you if you put Vornik to a use listed in Annex III of the EU
AI Act: for example, filtering job applications or evaluating
creditworthiness. Vornik is not placed on the market as a high-risk system.
When you give it such a purpose, the AI Act makes you the **provider** of that
high-risk system (Article 25(1)(c)), with the provider's duties under Article
16, including the instructions for use of Article 13. For Annex III systems
these apply from 2 December 2027 (Article 113, point (c)(i), as amended by
Regulation (EU) 2026/1744).

EaseIT Labs, as the initial provider, owes you cooperation (Article 25(2)): the
information, technical documentation, known limitations and failure modes,
and technical access you need. This page is that information, organised by
the points of Article 13(3), so you can write your own instructions for use
from it. It does not make Vornik a certified high-risk system, and it does not
replace your own conformity assessment.

## What Vornik is

Vornik is an agent orchestration platform. It runs AI-agent workflows in
isolated containers, calls the models you configure, keeps a memory of past
work, and stops for a human where you tell it to. Its intended purpose
names no Annex III use. Defining the high-risk purpose, and everything that
follows from it, is yours.

## Article 13(3), point by point

### (a) The provider

You are the provider of your high-risk system and name yourself in your
instructions for use. EaseIT Labs s.r.o. (Czech Republic) is the initial
provider of Vornik and is established in the Union, so no authorised
representative applies to it. Contact: **enterprise@vornik.io** (see
[Support](support.md)).

### (b)(i) Intended purpose

Vornik's own purpose is orchestrating AI agents. The Annex III purpose is the
one you define: the task, the people it affects, and the decisions it informs.

### (b)(ii) Accuracy, robustness and cybersecurity

Vornik states no accuracy for your task. Accuracy depends on the models,
prompts, tools and data you choose, which Vornik cannot measure for you. It
gives you the means to measure it:

- [Evals](guides/evals.md): repeatable test suites for a swarm, with a
  scoreboard and a regression diff between runs.
- The audit logs described under (f) below, to examine what each run did.
- The figures Vornik has measured on its own reference tasks, each with what
  it does not show: [Benchmark results](benchmarks/results.md). They are not
  figures for your task.

Robustness and cybersecurity controls, and what they do not cover, are in the
[security model](security-model.md), including its
[residual risks](security-model.md#residual-risks).

### (b)(iii) Circumstances that can lead to risk

The ones that matter most for a decision about a person:

| Circumstance | What Vornik does about it | What remains |
|---|---|---|
| A model is wrong | Workflows can add a review step or an approval before a result is used | A model can still be confidently wrong; a reviewer who does not read defeats the step |
| Prompt injection from content the agents read | Untrusted content is scanned and wrapped; writes can require approval | Mitigated, not solved ([residual risk 4](security-model.md#residual-risks)) |
| A tool is misused | Each role gets only the tools its job lists; write tools can require approval | A tool you allow is a tool an agent can call |
| Work runs unattended | Autonomy runs only in projects where it is enabled, under per-hour caps and budgets | An autonomous project acts without a person in each run |

### (b)(iv) Explaining output

Each task keeps its steps, the prompt each step received, and every tool call
with its redacted input and output. `task_explain` (on the companion
interface) and the task page show why a task ended as it did: its failure
class, each step's outcome and the last error.

### (b)(v) Performance on specific groups of persons

Vornik trains no model and holds no data about groups of persons. Performance
on the people your system affects depends on the models you choose and on
your prompts and data; measure it with your own test sets (evals above).

### (b)(vi) Input data

Vornik does not train or fine-tune models. It sends the content of each task
to the model endpoints you configure. What those models were trained on is
their providers' to state.

### (b)(vii) Interpreting output

Conversational replies tell people they come from an AI system. Published
output carries a machine-readable mark where the channel can carry one; where
it cannot (a Telegram message), the deployment's ledger records it over the
exact bytes. A reader checks content against that deployment's own ledger. A
mark can be stripped, and an edited copy is not recognised. Details and
limits: [AI transparency](ai-transparency.md).

### (c) Pre-determined changes

Vornik has had no conformity assessment, so no change is pre-determined in
that sense. Releases carry calendar versions; each release's changes, and any
action it needs before an upgrade, are in the
[release notes](release-notes/index.md).

### (d) Human oversight (Article 14)

The measures Vornik offers, which you configure for your purpose:

- **Approval steps** pause a workflow until a person approves; a restart does
  not resume past one ([Approvals](guides/approvals.md)).
- **Approval of task creation** for autonomous work, in the operator Inbox.
- **Approver devices**: a paired phone approves the actions and credentials
  that need a person.
- **Autonomy** runs only where a project enables it; `autonomy.enabled: false`
  stops a project's self-created tasks, and per-hour caps and budgets limit
  them ([Autonomy](guides/autonomy.md)).
- **Stopping a task**: `vornikctl task cancel <taskId>`.

To interpret an output before deciding, a reviewer reads the task's steps,
prompts and tool calls described under (b)(iv).

### (e) Resources, lifetime and maintenance

Vornik runs on Linux with rootless Podman and PostgreSQL with pgvector
([Getting started](getting-started/index.md)). It has no fixed end of life.
Releases come several times a month (seven in October 2026), and security
fixes target the latest release ([Security](security.md)). The podman
updater, `vornik-update.sh`, upgrades in place, takes a database and
configuration backup first, and prints the commands to roll back; read each
release's upgrade requirements in the [release notes](release-notes/index.md)
before upgrading. Retention of each record type is configurable
([Storage and retention](guides/storage-and-retention.md)).

### (f) Logs (Article 12)

- **Tool calls**: every tool call an agent makes, with redacted input and
  output, kept at least 180 days. Only `tool_audit_floor_override: true` with
  a stated basis lowers that.
- **Chat and step records**: conversation audit, step prompts and step
  outcomes, scanned for secrets before they are stored (the doctor check
  `redaction_seams` warns when that is off; see the
  [vornikctl reference](reference/vornikctl.md)).
- **Exports**: `vornikctl subject` exports what is linked to one person;
  backups export the rest.

Retention periods and what each record holds:
[Storage and retention](guides/storage-and-retention.md).

## Article 25(2): what else EaseIT Labs gives you

- **Technical documentation.** The architecture and security documentation
  on this site; an Annex IV skeleton is in preparation.
- **Known limitations and failure modes.** The table under (b)(iii) and the
  security model's [residual risks](security-model.md#residual-risks).
- **Technical access for testing and validation.** A test instance and the
  evaluation tools on request.

Enterprise customers can agree these in writing (Article 25(4)) as part of
their contract; Community users can ask at **enterprise@vornik.io**.
