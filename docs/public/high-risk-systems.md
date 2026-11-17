# Building a high-risk system on Vornik

**Last updated:** 15 November 2026

This page is for you if you put Vornik to a use listed in Annex III of the EU
AI Act: for example, filtering job applications or evaluating
creditworthiness. Vornik is not placed on the market as a high-risk system.
Give it such a purpose and the AI Act makes you the **provider** of that
high-risk system (Article 25(1)(c)). You then take on the provider's duties
under Article 16, including the instructions for use of Article 13. For Annex III systems
these apply from 2 December 2027 (Article 113, point (c)(i), as amended by
Regulation (EU) 2026/1744).

EaseIT Labs, as the initial provider, owes you cooperation (Article 25(2)): the
information, technical documentation, known limitations and failure modes,
and technical access you need. This page is that information, in the order
of Article 13(3), so you can write your own instructions for use from it.
Later sections add the technical-documentation map (Annex IV), serious
incidents (Article 73) and data governance (Article 10); human oversight
(Article 14) is under point (d). It does not make
Vornik a certified high-risk system, and it does not replace your own
conformity assessment.

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

Article 14(3) leaves the oversight measures to you: you build them into your
system, or name them for your deployers. Each capability Article 14(4) asks
for, the Vornik control you can use for it, and what that control does not
do:

| Article 14(4) | Vornik control | What it does not do |
|---|---|---|
| (a) understand the system and monitor it, including anomalies | the table under (b)(iii); each task's page; `vornikctl doctor`; the [metrics and dashboards](guides/observability.md); the operator Inbox, which lists failures | it shows what ran, not whether a decision about a person was right |
| (b) stay aware of automation bias, in particular for recommendations | [approval steps](guides/approvals.md) show the proposed action before it runs; replies say they come from an AI system ([AI transparency](ai-transparency.md)) | nothing stops a reviewer approving without reading: that is your deployers' training and your interface |
| (c) interpret the output | each task's steps, prompts and tool calls, and `task_explain` (see (b)(iv)) | it explains what the agents did, not why a model answered as it did |
| (d) decide not to use, disregard, override or reverse the output | reject an approval; a result is a file that changes nothing until a person or an approved write tool uses it; a change to the control plane can be rolled back on the admin console's Proposals tab | an action an approved write tool already sent (an email, an order) is not undone by Vornik |
| (e) intervene or stop, to a safe state | cancel a task on its execution page or with `vornikctl task cancel <taskId>`: its agent container is stopped and the task marked cancelled. `vornikctl task pause` pauses one; `autonomy.enabled: false` stops a project's self-created work ([Autonomy](guides/autonomy.md)) | a tool call already in flight may complete; the stop is per task or per project |

For a remote biometric identification system (Annex III point 1(a)),
Article 14(5) asks for two people to verify each identification. A Vornik
approval step takes one approver, so you build the second verification.

Your deployers assign oversight to people with the competence, training and
authority for it (Article 26(2)); that part is organisational.

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

## Technical documentation (Annex IV)

Article 11 makes you draw up technical documentation with at least the
Annex IV points. Vornik cannot write the parts about your purpose, your
risk management or your tests. For each point, what Vornik's documentation
covers ("Vornik"), covers in part ("partly") or leaves to you ("you"):

| Annex IV | Who | Where |
|---|---|---|
| 1(a) purpose, provider, version | you | Vornik's version is in its [release notes](release-notes/index.md) |
| 1(b) interaction with other software and AI systems | partly | [Architecture](concepts/architecture.md): models, MCP servers, channels; you list yours |
| 1(c) software versions and update requirements | Vornik | [release notes](release-notes/index.md), with each release's upgrade requirements |
| 1(d) forms it is supplied in | partly | Vornik ships as an install script and source ([Getting started](getting-started/index.md)); you state how you supply your system |
| 1(e) hardware it runs on | Vornik | [Getting started](getting-started/index.md), prerequisites; [Reference architecture](reference/reference-architecture.md) |
| 1(f) product photographs and markings | you | only where your system is part of a product |
| 1(g) the user interface | partly | the [guides](guides/index.md) describe Vornik's console and chat; you describe what your deployers use |
| 1(h) instructions for use | you | written from this page |
| 2(a) development methods, third-party models | partly | Vornik calls the models you configure; you describe how you built your workflows |
| 2(b) design logic and key choices | partly | [Architecture](concepts/architecture.md) for the platform; your workflows, prompts and tools are yours |
| 2(c) system architecture and resources | Vornik | [Architecture](concepts/architecture.md), [Reference architecture](reference/reference-architecture.md) |
| 2(d) training data | you | Vornik trains no model; see Data governance below |
| 2(e) human oversight assessment | partly | (d) above; the assessment for your purpose is yours |
| 2(f) pre-determined changes | you | see (c) above |
| 2(g) validation and testing, with test logs | you | [Evals](guides/evals.md) give you the means; Vornik's [benchmarks](benchmarks/results.md) are not figures for your task |
| 2(h) cybersecurity measures | partly | the [security model](security-model.md) for the platform; your deployment's measures are yours |
| 3 monitoring, functioning and control | partly | (b)(ii), (b)(iii), (d) and (f) above |
| 4 appropriateness of the performance metrics | you | |
| 5 risk management system (Article 9) | you | |
| 6 changes through the lifecycle | partly | Vornik's [release notes](release-notes/index.md); your own changes are yours |
| 7 standards applied | you | Vornik claims conformity with no harmonised standard |
| 8 EU declaration of conformity (Article 47) | you | |
| 9 post-market monitoring plan (Article 72) | you | the logs under (f) and the metrics support it |

## Serious incidents (Article 73)

The report to the market surveillance authority is yours. Report
immediately once you establish a causal link between your system and a
serious incident, or its reasonable likelihood; after a death, as soon as
you suspect one. Counted from when you, or where applicable your deployer,
become aware of the incident, the outer limits are 15 days; two days for
a widespread infringement or a serious incident under Article 3, point
(49)(b); and 10 days in the event of a death.
Article 73(6) also has you investigate without delay, cooperate with the
authority, and not alter the
system in a way that could affect the evaluation of the causes before you
inform the authority. With Vornik, while you investigate from the records:

1. Take a backup at once: `vornikctl backup --out <file>`
   ([Storage and retention](guides/storage-and-retention.md)). It holds every
   record present when it is taken; tool calls are kept at least 180 days.
2. Raise the retention periods, so the sweeper deletes nothing after it.
3. Make no upgrade or configuration change that alters the system until you
   have informed the authority. An upgrade migrates the database; a
   configuration change alters workflows and prompts.

Vornik has no single switch that suspends every deletion; steps 1 and 2 do
the same together. Article 73(1) speaks of systems "placed on the Union
market"; if you only put your system into service for your own use, plan for
Article 73 anyway: your deployers must tell you and the authority of a
serious incident either way (Article 26(5)).

## Data governance (Article 10)

Vornik trains and fine-tunes no model. A system that uses models without
training them falls under Article 10(6): only the testing data sets you use
must meet Article 10's criteria, and they are yours. If you train or
fine-tune a model, Article 10 applies to that work in full, outside Vornik.
Vornik's project memory is input to each task, not a training, validation or
testing data set; keeping input data relevant and representative falls to
whoever controls it (Article 26(4)).

## Article 25(2): what else EaseIT Labs gives you

- **Technical documentation.** The architecture and security documentation
  on this site, mapped to Annex IV above.
- **Known limitations and failure modes.** The table under (b)(iii) and the
  security model's [residual risks](security-model.md#residual-risks).
- **Technical access for testing and validation.** A test instance and the
  evaluation tools on request.

Enterprise customers can agree these in writing (Article 25(4)) as part of
their contract; users of the free [Community edition](editions.md) can ask
at **enterprise@vornik.io**.
