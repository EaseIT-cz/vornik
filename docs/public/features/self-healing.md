---
sources:
    - path: internal/enterprise/blackbox/engine/detector.go
      sha256: 51047036fea8634d9e13dd6ea9ffb777fdf133b2ad2ed398696ded493ad40cfc
    - path: internal/workflowhealing/promoter.go
      sha256: 25cf72653b1716aadac734df013ff504b4efb8c1ce3939de11c41f58201d49ce
---
# Self-healing workflow genome

!!! abstract "Enterprise Edition"

    This capability is part of the **Enterprise Edition** — a proprietary overlay on the open-source core. See [Editions](../editions.md) for the full Community vs Enterprise matrix.

Self-healing notices when a workflow regresses, proposes a repair, proves the
repair against the runs that failed, and asks you to promote it. Nothing it
produces reaches production until an operator promotes it.

It is built on the [Autonomy Black Box](blackbox.md): the detector reads the
same telemetry, and the proof is a counterfactual replay over recorded
evidence. Everything here needs an admin-scoped key.

## The loop

```text
detect → generate candidate → trial (replay vs recorded failures) → scorecard → operator promotes
  ▲ automatic              ▲ operator-initiated, everything downstream is too
```

1. **Detect.** A background sweeper compares each workflow's recent runs with
   its own baseline and opens a *trigger* when it regresses.
2. **Generate a candidate.** You ask a trigger for a candidate. Vornik tries
   deterministic repair recipes first, then the configuration assistant.
3. **Trial.** The candidate runs against the recorded failures, with every
   real-world side effect blocked.
4. **Scorecard.** A comparison gate scores the candidate against the baseline
   and marks it passed or failed.
5. **Promote.** You promote a passing candidate. Only then is the workflow
   file written, validated and hot-reloaded.

The detector is the only automatic part, and it only opens triggers. It never
generates or promotes anything.

## Detection

The sweeper runs hourly. For every workflow with at least 10 runs in both
windows, it compares the **last 24 hours** with a **7-day baseline**, and opens a
trigger when a metric regresses past its threshold:

| Trigger class | Fires when | Default threshold |
|---------------|-----------|-------------------|
| `failure_rate_spike` | the failure rate rises | +25% relative |
| `cost_regression` | the average cost per run rises | +40% relative |

Each trigger keeps up to five **evidence executions**, failed runs first, then
the most expensive. A repair has to prove itself against these runs. Only one
trigger stays open per project, workflow and class, so a lasting regression
does not flood the list.

```bash
vornikctl blackbox trigger list --status open
vornikctl blackbox trigger dismiss <trigger_id>          # accepted, or not actionable
vornikctl blackbox trigger generate-candidate <trigger_id>
```

To change a threshold, or mute a noisy workflow for a while, per project,
workflow and class:

```bash
vornikctl blackbox override set --project P --workflow W --class cost_regression \
    --threshold-pct 60 --mute-hours 48
```

## Candidate generation

Generating a candidate is always your request; the detector never proposes.

1. **Deterministic recipes first:** today a retry-budget adjustment and a
   verifier-insertion step. A known structural fix is cheaper and more
   predictable than a model.
2. **Then the configuration assistant**, which proposes a change to the
   workflow from the trigger's evidence. If it finds no change is warranted,
   it says so; that is not an error.

**The assistant's candidates need a judge of another model family.** Every
assistant candidate is checked by a second model, and Vornik refuses to let a
model judge its own family's work (`JUDGE_SAME_FAMILY`). An install that runs
one model for everything therefore gets recipe candidates only: each assistant
candidate is refused, and the trigger stays open. Set a judge from another
family:

```yaml
config_assistant:
  judge_model: "<a model of a different family from your chat model>"
```

`vornikctl doctor` checks this pair in the `healing_judge_family` row, and the
daemon logs a warning at start when it is refused. The
[configuration assistant](config-assistant.md) page describes the judge.

## Proving a repair: trials and the scorecard

A candidate is a proposed new version of the workflow. It must pass a trial
before it can be promoted. There are two trial modes:

- **Static** checks the candidate's shape and policy only. It is a quick sanity
  pass and **not enough for promotion**.
- **Replay** re-runs the trigger's evidence executions through the candidate,
  using the Black Box replay engine, so every side-effecting tool is stubbed.
  This is the real test: does the candidate do better on the runs that failed?

The **scorecard** compares the candidate's trial with the baseline. By default
the candidate must not regress on any axis:

- the success rate must not drop;
- cost must stay within +10%;
- latency must stay within +10%;
- hallucination and verifier-failure counts must not increase.

The scorecard only scores. It never promotes.

## Promotion is always yours

You promote a passing candidate from the **Candidates** tab at
`/ui/admin/blackbox/candidates`, which shows the diff, the motivation, the
evidence links and the scorecard. Promotion:

- refuses a candidate without a passing trial;
- **requires a passing replay trial**: a static pass alone cannot be promoted;
- **refuses a candidate whose workflow changed after the candidate was
  generated.** A candidate carries a whole new version of the workflow file,
  so promoting an old one would silently undo every edit made since. Vornik
  records the fingerprint of the workflow each candidate was built from and
  compares it before applying anything. If they differ, you are asked to
  regenerate the candidate. A candidate with no recorded fingerprint is
  refused for the same reason;
- applies the change through the normal workflow path (write the workflow
  file, validate it, hot-reload it), recording who promoted it and when, and
  keeping the replaced file for rollback.

Nothing promotes in the background. If you never promote, nothing changes.

A workflow has at most one candidate waiting for a decision. Rejecting a
candidate clears that slot, so the next detection can propose another.

## Kill switches

| Control | Effect |
|---------|--------|
| `vornikctl blackbox override set … --mute-hours` | silences detection for one project, workflow and class |

## Where to find it

| Surface | Location |
|---------|----------|
| CLI | `vornikctl blackbox trigger {list, dismiss, bulk-dismiss, generate-candidate}` and `vornikctl blackbox override {list, set, delete}` |
| Web UI | `/ui/admin/blackbox`: **Triggers**, **Candidates** and **Overrides** tabs |
| API | `/api/v1/admin/workflow-healing/*` (admin-scoped key) |

Detection covers the failure-rate and cost regression classes. The trace and
replay foundation it builds on is described on the
[Autonomy Black Box](blackbox.md) page.
