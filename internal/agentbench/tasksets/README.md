# Benchmark task sets

**A task set is bound to a swarm, and the binding is semantic — not just that
the role names resolve.**

That was learned the hard way on the first live run (2026-08-14). The original
single set paired `companion-*` workflows with `dev-swarm` because `dev-swarm`
has roles called `analyst` and `reviewer`, which is what those workflows
declare. Every step failed with `verify_claims_failed`: the declared output file
(`artifacts/out/review.md`, `artifacts/out/findings.md`) was never written,
through a shape retry AND a model fallback, on every attempt. The same workflow
succeeds on a companion swarm.

Vornik validates that a workflow's role names RESOLVE in the project's swarm. It
does not check that the resolved role can satisfy the step's output contract, so
a semantically incompatible pairing loads clean and fails at run time, every
time, expensively.

| Set | Swarm | Workflows |
|---|---|---|
| `dev-swarm-tasks-v1.json` | `dev-swarm` | `simple-workflow`, `dev-pipeline` |
| `companion-swarm-tasks-v1.json` | a companion swarm | `companion-research-gather`, `companion-data-validation`, `companion-test-coverage-audit` |

Only the dev-swarm set has been run end to end. The companion set is authored
but unvalidated: it needs a benchmark project bound to a companion swarm, and
until that exists its numbers would measure the mismatch above rather than
anything about agent quality.

Compute a set's digest — which `bench agent gold` fences against — with:

    vornikctl bench agent taskset-hash <file>

Do not compute it as a sha256 of the file: the digest is order-independent and
length-prefixed, so a reordered file hashes the same and a rename cannot
compensate for an edit.

**Use a current binary, and check what you captured.** `bench agent` ships in
every edition since 2026.11; an older binary does not have it. A binary without it prints this command group's HELP
to **stdout** and exits **0**, so a pipeline like

    HASH=$(vornikctl bench agent taskset-hash "$TASKS" | awk '{print $1}')

captures the help text — one word per line — and reports success. On
2026-08-14 exactly that produced `dev-swarm-gold-v1.json` with

    "taskSetSha256": "Benchmark\n\nUsage:\nvornikctl\n\nAvailable\nmemory\n…"

which fences against every task set. The tool-grant probe was dark from then
until 2026-08-19, and `dev-swarm-gold-v1.json` was deleted rather than repaired:
its 18 entries also predated the set growing to 30, so a corrected digest over
stale ground truth would have been worse than none.

`GoldManifest.Validate` now refuses a digest that is not 64 lowercase hex
characters, at build, merge and load; `scripts/agentbench-reproduce.sh` uses the
pinned binary and checks the shape. Re-record with `--gold` before relying on
the tool-grant probe.
