# Bench-only harness workflows

Workflow definitions that exist to **exercise** the daemon, not to ship with it.
They live here rather than in `configs/workflows/` for one reason: everything in
`configs/workflows/` is a shipped surface and reaches the public CE export.
These must not.

`internal/agentbench/tasksets/` is pruned from the CE export
(`scripts/export-public-ce.sh`, and the `EE_PATH_RE` gate asserts it stays
pruned), so this is a safe home that is still under version control. Before
2026-08-19 `recovery-probe.md` existed only in the deployed bench tree, where
nothing tracked it.

## Installing

Not installed by any target — copy it into the bench tree and reload:

```sh
cp internal/agentbench/tasksets/fixtures/bench-workflows/recovery-probe.md \
   ~/.config/vornik-bench/configs/workflows/
VORNIK_API_URL=http://127.0.0.1:8090 VORNIK_API_KEY=<admin-key> \
   vornikctl config reload
```

Deliberately manual. An installer would make it easy to land a fixture in the
production tree, and a workflow that intentionally fails steps has no business
there.

## `recovery-probe.md`

Exercises the lead recovery hop, which nothing else covers: every other bench
project sets `pedantic: true`, which routes `on_fail` straight to `failed` and so
never runs a recover step. Pair it with the non-pedantic `recoverybench` project.

Its header carries the full rationale for how the failure is induced, and why the
first version's induction was a bad instrument. The short version: an induced
failure must resemble the failures that actually occur, or the measurement
describes the harness instead of the system.

## Reading results from a harness fixture

Two checks belong on every measurement taken through one, both learned by having
reported numbers that did not survive them:

1. **Check the instrument before the result.** Count `MODEL_UNHEALTHY` in the
   window — a tripped health breaker fails every attempt for reasons unrelated to
   what you are measuring, and it invalidated two measurements here.
2. **Prefer an end-to-end, binary signal over a rate.** "A decision checkpoint
   reached the operator" survived a model outage, a confounded time-window
   comparison, and a fixture artifact. Step-outcome rates survived none of them.
