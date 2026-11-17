---
workflowId: "recovery-probe"
displayName: "Harness: recovery-path probe"
description: "BENCH-ONLY harness fixture. Fails its first step the way production failures actually look, so the lead recovery hop can be measured end to end. Not a shipped workflow: it exists only for the benchmark, published with the task sets (#609)."
version: "2.0.0"
author: "Vornik <vadim@vornik.io>"
license: "Proprietary"
entrypoint: "doomed"
maxStepVisits: 2
maxIterations: 8
maxWallClock: "20m"
# WHY THIS EXISTS (2026-08-19).
#
# The lead recovery hop (`type: plan` with a populated recovery context) had no
# end-to-end coverage: every other bench project sets `pedantic: true`, which
# drops on_fail straight to `failed`, so recover hops never run. This fixture
# and the non-pedantic `recoverybench` project exist to exercise that path.
#
# HOW THE FAILURE IS INDUCED, AND WHY v1's WAS WRONG.
#
# v1 used `timeout: "1s"`, which the step could not beat. Deterministic, and a
# bad instrument. A container killed at 1s produces a podman/runtime error, and
# runContainerStep appends 50 lines of container log to any step error — so the
# lead's recovery context became a crash blob. Measured consequence: the lead
# generated ~1 MB responses taking ~149s, which tripped the model-health circuit
# breaker, which then failed 24 of 27 attempts with MODEL_UNHEALTHY. Every
# `tool_call_wrapper` schema-leakage signal in the ledger came from this fixture
# and none from real recovery paths. The instrument was generating the failures
# it measured.
#
# v2 induces the class that ACTUALLY triggers recovery in production. Counted
# over the ledger's real recover hops (plan-and-write, research):
#
#   plausibility_violation   57
#   verify_claims_failed     29
#
# Both are short, structured, CONTENT failures — an agent claiming something the
# filesystem contradicts — not runtime crashes. So the induction is an output
# contract the agent cannot satisfy no matter how well it behaves: the declared
# path is inside the project's `.git`, which the runtime bind-mounts READ-ONLY.
#
# The agent is asked honestly, tries honestly, and cannot comply. It is never
# instructed to misbehave — earlier attempts that told it to skip its output
# failed, because require_output_glob rides in the agent payload and the runtime
# corrects before finishing. Agents are also told the git mount is read-only
# (974a69d2), so the expected behaviour is a clean early give-up rather than a
# retry loop, and the resulting step error is the one-line output-contract
# message rather than a crash dump.
#
# Runs on `recoverybench`, the one non-pedantic bench project.
steps:
  doomed:
    type: "agent"
    role: "ingestor"
    # UNSATISFIABLE BY CONSTRUCTION, and physically so. `project/` resolves to
    # the project directory (output_contract.go), whose .git is a read-only
    # bind mount — so no agent, however cooperative, can place a file here.
    # This is the induction; see the note above for why it is shaped this way.
    require_output_glob: "project/.git/recovery-probe-output.md"
    on_success: "done"
    on_fail: "recover"
    # A REAL timeout. v1's 1s was the defect; the step must be allowed to run
    # normally so the failure is its output contract and nothing else.
    timeout: "8m"
  recover:
    # The path under test: a lead plan hop with a populated recovery context,
    # which must emit a decision checkpoint (outcome=checkpoint, kind=decision)
    # rather than prose or a continue.
    type: "plan"
    role: "lead"
    on_success: "failed"
terminals:
  done:
    status: "COMPLETED"
  failed:
    status: "FAILED"
    message: "Recovery probe reached its failed terminal (expected when the lead declines to pivot)"
---

# Harness: recovery-path probe

Bench-only fixture. The first step cannot satisfy its declared output contract,
so it fails into the lead recovery hop.

**PASS:** the `recover` step records outcome `ok` AND the task lands in
`AWAITING_INPUT` carrying a `decision` checkpoint with selectable options. Check
both — a step outcome of `ok` alone is not evidence a decision reached anyone.
The confound-proof query is the one that counts delivered checkpoints:

```sql
SELECT count(*) FROM task_messages m JOIN tasks t ON t.id = m.task_id
WHERE t.project_id = 'recoverybench' AND m.message_kind = 'checkpoint';
```

**FAIL:** `recover` records `schema_violation` / `degenerate_loop` /
`iteration_exhausted`, or the run dies with
`recovery contract violation: … the lead emitted outcome="missing"`.

**Before trusting any number from this fixture**, check the window for
`MODEL_UNHEALTHY` — a tripped health breaker fails every attempt for reasons
that have nothing to do with recovery, and it has already invalidated two
measurements here.

## Prompts

### doomed

Write a two-line summary of the task payload to
`project/.git/recovery-probe-output.md`.

If you cannot write that file, say so plainly in your result and stop — do not
retry the write, and do not substitute a different path. This step is expected
to fail its output contract; that is the point of the fixture, and no
alternative you choose can satisfy it.
