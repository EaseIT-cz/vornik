# Benchmarks

Vornik is measured, not asserted: what we measure, how to reproduce it, and
where Vornik loses. Results: [Results by release](results.md).

| Benchmark | Question it answers | Reproducible by you |
|---|---|---|
| **Memory retrieval** (LongMemEval) | Does memory find what matters? | Yes: public dataset, shipped harness |
| **Agent quality** | Does the control logic carry tasks to a correct finish: what the lead granted, whether roles followed their schemas, whether agents called tools correctly? | No: the task set and answer key are not published |

Every number comes from `vornikctl bench memory` or `vornikctl bench agent`,
which ship with the product; nothing is transcribed by hand. Every result
carries a **comparability key**, a digest of the dataset revision, models,
retrieval budget and prompts, so two results that are not comparable refuse
to be compared.

**Agent results name their model.** They are measured on self-hosted
open-weight models on one machine (a 27B dense model in 2026.9.0; a 26B
mixture-of-experts model with 4B active on one RTX 3090 in 2026.11.1), which
is what a self-hosting user runs, and harder than a large hosted model.

## Two tiers for memory, never blended

- **Tier 2, judge-free (the primary result).** Context recall, context
  precision and MRR, comparing retrieved document ids with the dataset's gold
  labels. No model is involved, so the numbers stay reproducible after any
  model is retired.
- **Tier 1, judged accuracy (supporting).** One LLM answers from the retrieved
  context and a second grades it. The judge's own noise is about 4.5% standard
  deviation at n=30, enough to manufacture or erase a "win". Any judged figure
  names an open-weight, version-pinned judge, so it can be re-run.

## Reproducing a result

The harness needs a running Vornik daemon, a companion key with memory access, and a
database that is **not** your production one.

```bash
# 1. The dataset is not vendored — it belongs to its authors. Note the repo name:
#    `longmemeval-cleaned` is the Sept 2025 revision. The older `longmemeval` repo is
#    DEPRECATED and its numbers are not comparable to these.
curl -sLO https://huggingface.co/datasets/xiaowu0162/longmemeval-cleaned/resolve/main/longmemeval_oracle.json

# 2. A key scoped to a throwaway project.
vornikctl companion grant --project bench --client claude-code --memory-all

export VORNIK_URL=http://127.0.0.1:8080
export VORNIK_COMPANION_TOKEN=<the secret printed above>

# 3. Name the database YOUR daemon writes. The harness asks the daemon and refuses
#    when the two disagree, so a wrong value fails loudly instead of writing
#    somewhere you did not intend.
scripts/bench-reproduce.sh --dataset-path ./longmemeval_oracle.json \
    --database my_bench_db --category temporal-reasoning --items 6 --runs 3
```

- **Use a bench deployment, not production.** The harness bulk-writes a
  corpus. Give the bench daemon its own database, listener and a config tree
  with only autonomy-off projects: a daemon that loads a production project
  tree starts doing that project's real work.
- **Item selection is deterministic:** the first N items of the category in
  file order, or the first N of each category for an all-ability run. The
  six-item rows list their question ids; the 120-item rows use
  `--max-items-per-category 20` with no category filter, and the scored ids
  are in each run's journal.
- **Warm and cold do not mix.** Before 2026-08-21 the harness did not clear
  the store between runs, so those rows are warm and closed; the tracked axis
  is cold. `bench memory aggregate` and the comparability key refuse to mix
  them.
- **Compare keys.** `scripts/bench-reproduce.sh` pins every axis that changes
  a result and prints the key. A key that differs from a published one means
  the runs measured different things.

### Two guards you will meet

Both caught real incidents; neither can be skipped silently.

**It refuses to write the wrong database.** It asks the daemon which database
it writes and compares that with the one you named, and fails closed when it
cannot get an answer:

```
error: refusing to run: you named "vornik_bench" but the "vornik" system actually
writes "production_db"
```

**It waits for ingest to finish.** Embedding is asynchronous; recalling while
the queue drains measures keyword-only retrieval, which once produced a
recall figure 8 points too low. The harness waits for the queue to empty and
refuses if it never does.

## What these numbers do not tell you

- **They are on our scale.** Scores use our judge and our retrieval budget; do
  not compare them with figures published elsewhere under other conditions.
- **Tier 2 cannot measure abstention.** Questions whose answer is deliberately
  absent have no gold document, so they are excluded from the mean, not scored
  zero. Abstention is judged only.
- **Small n is small.** Every result states its item count; a six-item screen
  is a smoke test.
- **Chunking differs between systems.** Vornik splits long sessions on turn
  boundaries at the per-deposit size cap. Where a compared system does not,
  the two saw differently shaped inputs; the harness records this.
