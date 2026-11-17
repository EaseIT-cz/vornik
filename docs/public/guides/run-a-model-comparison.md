# Run a model comparison

Compare two models on Vornik's agent benchmark: the same tasks, the same
harness, two arms, and a scoreboard that refuses to compare what is not
comparable. How the benchmark works and what it does not measure:
[How the agent benchmark works](../benchmarks/methodology.md).

A full pass is long. The [published agent results](../benchmarks/results.md)
ran 30 tasks × 3 repeats in about 10 hours per arm on one GPU (a median of
about 5 minutes per run). A release gate needs 10 repeats per task, so plan
for days, not hours. Start with `--smoke`.

## 1. A bench daemon, not your production one

Work from a checkout of the repository and build the CLI from it:
`go build -o /tmp/vornikctl ./cmd/vornikctl` (the script says so if the
`vornikctl` it finds lacks `bench agent`).

The harness writes tasks, workspaces and memory. Run a second Vornik daemon
for it, with its own database, listener and config tree holding only
autonomy-off projects. The script's defaults expect that daemon on port 8090
with its config in `~/.config/vornik-bench/`; `--daemon-config`,
`--daemon-binary` and `--workspace` point it elsewhere.

```bash
export VORNIK_URL=http://127.0.0.1:8090        # the bench daemon
export VORNIK_API_URL="$VORNIK_URL"            # vornikctl reads this one
export VORNIK_COMPANION_TOKEN=<a key from: vornikctl companion grant -p agentbench --client claude-code ...>
export VORNIK_BENCH_DSN="host=... dbname=... user=... password=... sslmode=disable"   # the bench database
```

The script refuses to run against a database other than the one the bench
daemon writes.

## 2. Prove the pipeline

```bash
scripts/agentbench-reproduce.sh --smoke
```

One task, one run. Fix whatever it reports before spending a full pass.

## 3. Write a pre-registration

Copy one from `preregistrations/` in the `agentbench` package of the
repository's `internal` directory. Edit it before
you run anything: the two arms, the metric, the difference worth detecting
(δ) and the repeats it needs. The harness records its hash in every journal,
and the release gate's `calibrate` and `noise-floor` refuse a journal whose
pre-registration was edited after the run.

## 4. Run both arms

Configure the bench daemon's agent model for the first arm, then run it;
change the model, restart the bench daemon, run the second. Each run writes
its journals under `./agentbench-runs` (`--out` moves it):

```bash
scripts/agentbench-reproduce.sh --arm baseline  --preregistration my-prereg.json --repeats 10
scripts/agentbench-reproduce.sh --arm candidate --preregistration my-prereg.json --repeats 10
```

`--repeat-batch` splits a long pass into chunks that journal separately, and
`--budget-tokens` caps what a pass may spend against a prepaid provider. The
script's header lists every option.

## 5. Read the result

```bash
vornikctl bench agent report  <journal>               # one arm's scoreboard
vornikctl bench agent rollup  <journal>...            # cost, efficiency, accuracy, success rate
vornikctl bench agent compare <journal-a> <journal-b> # the difference, or a refusal
```

`compare` refuses two arms that differ in more than the axis you
pre-registered. A refusal is a result: it says the runs measured different
things.

## What a comparison does not tell you

- It measures these tasks, with this harness, on your machine.
- A task set published before a model was trained may be in its training
  data; prefer the newest set for a decision.
- Fewer than 10 repeats gives no usable σ; the gate refuses to decide on it.
