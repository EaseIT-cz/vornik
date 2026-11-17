# How the agent benchmark works

The agent benchmark measures whether Vornik's control logic carries work to a
correct finish with a given model. It records what the lead role granted,
whether roles produced output in their schemas, whether agents called tools
correctly, and whether the task's own acceptance check passed. It does not measure what a
model knows. Results: [Results by release](results.md). To run it yourself:
[Run a model comparison](../guides/run-a-model-comparison.md).

Everything it uses is in the open repository: the harness (`vornikctl bench
agent`), the task sets with their answer keys and tripwires, the
pre-registrations, and the script that runs an arm end to end
(`scripts/agentbench-reproduce.sh`). The task sets are in `tasksets/` and the
pre-registrations in `preregistrations/`, both in the `agentbench` package of
the repository's `internal` directory.

## What one run measures

A run is one **arm**: one model and one configuration, over one task set,
each task repeated *n* times. For each task run the harness records:

- **task success**: the task's acceptance check passed;
- **tool grant**: the lead granted the tools the task demonstrably needed;
- **schema following**: each role's output matched its declared schema;
- **tool use**: agents called real tools with valid arguments;
- **cost**: tokens and money, per run and per success (`bench agent rollup`).

The scores come from the run's ledger, not from what an agent says about its
own work. A score the system under test authors is refused as a gate metric.

## The rules that keep a number honest

1. **Pre-registered.** An arm runs only against a committed pre-registration
   that states, before the run, the arms, the metric, the difference worth
   detecting (δ) and the number of repeats it needs. The run journal records
   the pre-registration's hash, and every reported figure prints it. Choosing
   what to compare after seeing results is the line between a benchmark and
   a press release.
2. **σ with its n.** Every published number carries its standard deviation
   and the number of runs it came from. The harness will not gate on a σ
   from fewer than 10 runs.
3. **Comparable or refused.** `bench agent compare` refuses two arms that
   differ in more than the axis under test.
4. **Losses are published.** A worse result is a new row, published like a
   better one.
5. **Append-only results.** A published row is never edited; a regression
   shows as a worse next row. `make lint` fails a change that edits or
   removes a published row of [results.md](results.md).

## Release gate: PASS, FAIL or REFUSED

Before a release, the candidate arm is compared with the baseline under a
pre-registered gate policy. Two artifacts come first:

- **Calibration** (`bench agent calibrate`), from a repeated run: every gate
  task must pass on some repeats and fail on others. A task that passes every
  repeat is saturated: it cannot show a regression. It moves to the
  tripwire tier, or it gets harder. A tripwire task must always pass: any
  failure alarms, and it never enters a score.
- **Noise floor** (`bench agent noise-floor`), from two arms with the same
  configuration: the difference two identical runs show by chance.

`bench agent gate` then answers **PASS**, **FAIL** or **REFUSED**. REFUSED
means the evidence cannot decide (too few runs, a saturated task, an
incomparable arm), and it is reported as such, never as a pass.

## The task sets and contamination

A published task set can be trained against, so a model released after it
may have seen it. The sets in the repository are published. A new set is cut
each year, or at a major release, and kept out of the open repository until
its first use in a release gate. It is published then. Newer agent rows
name the task set they came from: the 2026-10-31 rows ran `dev-swarm-tasks-v1`.
The 2026.9.0 row predates that rule and does not name its set's version. The native memory gold set
is not published: it is built from private design documents. The public
memory benchmarks are LongMemEval and LoCoMo.

## What these numbers do not tell you

- **They are this harness's tasks.** They exercise Vornik's swarms (coding,
  assistant and companion workflows), not general reasoning, chemistry or
  mathematics.
- **They name their model and machine.** A figure measured on one GPU with
  an open-weight model says nothing about a hosted model, and the reverse.
- **A published set may be contaminated** for a model trained after its
  publication; use the newest set for a gate.
- **Small n is small.** Every result states its repeats.
