# Timeouts on your own hardware

Vornik's time budgets — how long a step may run, how long a task lease is held —
are absolute wall-clock values. They were chosen on particular hardware. Move to
hardware that generates tokens at a different speed and those numbers stop
meaning what they meant.

The gap is not small. Two self-hosted endpoints measured on the same day:

| | marginal decode | per-request overhead |
|---|---:|---:|
| a fast GPU host | 206 tok/s | 58 ms |
| a modest local box | 12 tok/s | 499 ms |

Seventeen times. A replay of recorded step shapes at the slower rate predicted
that the 5-minute task lease would expire under 16% of steps. A live run on a
slower host still (about 8 tok/s, September 2026) showed otherwise: the lease is
renewed while a step runs, and steps of 30 minutes and more held it without
trouble. What does bind on slow hardware is listed in
[What binds on a slow backend](#what-binds-on-a-slow-backend) below, with what
to set for each.

## Measure first

```sh
vornikctl profile
```

With no arguments it reads the daemon's own database and fits every model that
has recent work:

```
MODEL                      STEPS  DECODE tok/s  ±   PER TOOL CALL  FIXED
Qwen/Qwen3-Coder-Next-FP8    689           215  6%          0.80s   -0.0s
```

Three separate numbers, deliberately:

- **DECODE** is the model's own rate. This is the one a timeout should scale on.
- **PER TOOL CALL** is what your tools cost. A rising value here is a tool
  problem, and must not buy the model more time.
- **±** is how uncertain the decode rate is. Above 30% the command refuses to
  report a rate at all: a figure that loose is a range, not a number.

The naive measure — tokens divided by step duration — folds all three together,
and would make a deployment look slower simply for having slow tools.

### A machine with no history

A fresh install has nothing to fit, which is exactly when you want to know
whether the hardware is usable. Probe the endpoint directly:

```sh
vornikctl profile \
  --probe-endpoint http://your-host:8000/v1 \
  --probe-model your-model \
  --suggest-config
```

This needs no database and no prior work. It measures decode as the **slope**
between a short and a long generation, so per-request overhead cancels instead of
dragging the rate down.

Probe and fit measure different things and are never averaged. The probe is what
the hardware can do; the fit is what it does under your real workload. When both
exist the command prints them together, and the **gap between them is your
contention** — a scheduling story, not a model one.

## Then enable (what it scales)

> **As built, September 2026:** the factor scales every **step timeout**, after
> the task's tier factor, and the task lease. Per-call LLM and shell timeouts
> follow the step budget. It is time only: tool-call budgets never move. A
> warm-pool role is stretched on slow hardware but never shrunk on fast. The
> factor is computed once at startup from the DECLARED rates below, so change
> them and restart. The `hardware_too_slow` failure class described below is
> designed but not built.

```sh
vornikctl profile --suggest-config
```

emits a block to paste into the config the daemon actually reads (confirm with
`vornikctl config show`):

```yaml
scheduler:
  speed_aware_timeouts:
    enabled: true
    reference_tokens_per_sec: 215
    observed_tokens_per_sec: 215
    min_factor: 0.5
    max_factor: 8.0
```

`reference_tokens_per_sec` is the only value needing judgement. Read it as a
claim you are making: *"the timeouts in this deployment work as they are, on
hardware that decodes at this rate."* If that is true of the machine you just
measured, use its number. Every slower host then scales against a baseline that
demonstrably worked.

It cannot be inferred for you. If each deployment used its own measurement as its
own reference, the factor would be 1.0 everywhere and the feature would never do
anything.

The feature is **off by default**, and off is byte-identical to not having it.

### When hardware is too slow

`max_factor` is deliberately lower than the slowest hardware would ask for. A 12
tok/s box against a 215 tok/s reference wants roughly 18x; the ceiling stops at
8x, logs a warning, and a step that then times out fails as `hardware_too_slow`
rather than as a generic timeout. *(Designed, not yet built: today a step that
runs out of time fails as an ordinary timeout.)*

That is on purpose. The honest answer at 18x is that the hardware cannot run the
workload in this shape — not a two-hour step timeout arrived at by arithmetic.
Raise `max_factor` if you disagree; it is your call, made knowingly.

## Budgets this does not scale

Build and test tools are **compute-bound, not decode-bound** — a `go build` does
not get faster because the model does. They have their own knob:

```sh
VORNIK_TOOL_TIMEOUT_FACTOR=2.5    # in the agent container's environment
```

It is a factor rather than absolute overrides so the deliberate ratios survive: a
Rust build stays longer than a typecheck.

## What binds on a slow backend

Measured on a commodity Ollama host (a 27B model at 4-bit, about 8 tok/s decode,
about 80 tok/s prefill; one 26K-token prompt took 337 s). That prefill figure is
unrelated to `reference_tokens_per_sec` above, which is a decode rate. These are the limits
it hit, in the order they fired, with what to set for each.

**1. Step budgets, scaled DOWN by the task's tier.** A workflow step's declared
`timeout` is multiplied by the task's complexity tier before it reaches the
container. The factors are `tool_budget.factors` (trivial 0.25, standard 0.5,
complex 1.0, open-ended 2.0 by default). A `review: 30m` step on a trivial task
gets 7.5 minutes, and a raise you make for slow hardware is scaled down with it.
Three ways out, best first:

- Enable `scheduler.speed_aware_timeouts` with your measured rate (see
  [Then enable](#then-enable-what-it-scales)). Every step budget is then
  stretched by `reference / observed`, after the tier factor. Your declared
  timeouts keep meaning what they mean on the reference hardware.
- Declare step timeouts at your real need divided by the smallest factor your
  tasks get. That is 4x for a step trivial tasks reach.
- Or set the factors you use to 1.0 in `tool_budget.factors`. That costs the
  tier's time-rationing on every host that daemon serves.

Until a task has a tier the factor is 1.0, so a workflow's first step, such
as `analyze` or `plan`, runs at its declared budget: it is the step that
decides the tier.

**2. One LLM call longer than its timeout.** Each agent call is bounded by
`agent_llm.timeout` (300 s if unset), clamped to half the step's budget. The
first call of a step carries the whole prompt. At 80 tok/s prefill, a
20,000-token prompt needs about 250 s before the first output token. Set
`agent_llm.timeout` above your largest cold prompt divided by your prefill rate,
and keep step budgets at least twice that. The two interact: the half-step
clamp applies to the step budget AFTER the tier factor. So on a trivial task,
a 30m review step (7.5 minutes after scaling) caps every call at 225 s,
whatever `agent_llm.timeout` says. Raising the declared step timeout (item 1) is
also what relaxes this clamp. Later calls in a step are much
cheaper where the backend caches prompt prefixes (Ollama does).

**3. Optional LLM work competing with the task.** Narration lines, memory
titles and classification, consolidation narratives and search reranking all
call the model under short deadlines of their own (10 to 30 s). On a slow
backend they time out on every call, and they queue on the same GPU as the task
traffic. Switch them off for that model:

```yaml
chat:
  optional_work:
    disabled_models: ["your-slow-model"]   # or disabled: true for every model
```

Refused calls make no request, and each feature degrades as it does without an
answer: template narration, unranked search, chunks left unclassified for later.
`vornik_chat_optional_work_refused_total` counts what was skipped. Before you
switch it on, `vornik_chat_best_effort_deadline_total` rising for a model is the
sign you need it. Those timeouts no longer mark the model unhealthy, so they
cannot take task traffic down with them.

**4. The context window you declared versus the one the server runs.**
`agent_llm.model_limits.<model>.context` must match what the server actually
serves, not the model's advertised maximum. Ollama, for example, runs whatever
`num_ctx` it was started with. An over-declared window produces prompts the
server truncates or refuses.

