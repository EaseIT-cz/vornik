# Results by release

Every result Vornik publishes, with its spread and what it does not show.
Rows are never edited after a release ships: a regression shows as a worse
next row, not a changed old one. A mean without its standard deviation is not
a result, because two systems whose intervals overlap have not been separated.

| Benchmark | Latest | Result | Section |
|---|---|---|---|
| Memory retrieval, LongMemEval, 120 items, cold corpus | 2026.11.5 | recall 0.9851, precision 0.9854, MRR 0.9958, sd 0.0000 (unchanged since 2026.9.4) | [cold axis](#longmemeval-retrieval-all-six-abilities-tier-2-cold-corpus) |
| Agent quality, open-weight model on one GPU | `2026.10.7-94` (in 2026.11.1) | 89 to 90 of 90 tasks; €1.34 to €1.40 of electricity per run | [below](#agent-quality-a-4b-active-open-weight-model-on-one-gpu-2026-10-31) |
| Agent quality, first scored arm | 2026.9.0 | 30 tasks, 100% task success, 9.7% of steps produced no output | [below](#agent-quality-202690-first-scored-arm) |
| Judged accuracy and the judge's noise | 24 items × 10 runs | accuracy 0.8241, sd 0.0340: a gate needs ±10.2 points | [tier 1](#longmemeval-judged-accuracy-tier-1-and-the-judges-noise-floor) |

## Agent quality — a 4B-active open-weight model on one GPU (2026-10-31)

**Result.** On one RTX 3090 workstation, `gemma-4-26b-a4b` completed 89 to 90
of 90 software tasks through Vornik's multi-agent `dev-pipeline`. The
electricity for a full run cost €1.34 to €1.40. The bench's price list puts the
same tokens on a hosted model at about $20.

Measured on `2026.10.7-94-g3246baad0`, which is part of 2026.11.1. The run was
made twice: once without container hardening (OFF) and once with a read-only
agent root filesystem and a 2,048-process limit (ON).

| | OFF | ON |
|---|---|---|
| run slots passed (30 tasks × 3 repeats) | 90 of 90 | 89 of 90 |
| acceptance suites passed (12 graded) | 9; 3 did not compile | 11; 1 did not compile |
| expected files produced | 76 of 90 | 79 of 90 |
| schema conformance (share of steps whose output matched the required structure) | 0.965 | 0.973 |
| tool-call validity | 1.000 | 1.000 |
| steps with no output | 3.0% | 3.5% |
| tokens per task | 324,067 | 325,975 |
| wall time per run (median) | 5.1 min | 4.6 min |
| run window | 9.94 h | 9.63 h |
| energy, model host | 3.87 kWh (389 W average) | 3.69 kWh (383 W average) |
| electricity cost | 34.18 CZK (€1.40) | 32.58 CZK (€1.34) |
| priced cost on a hosted model | $20.88 | $19.75 |

**Setup.** Model: `gemma-4-26b-a4b` (26B parameters, about 4B active per
token), GGUF Q4_K_M, served by llama.cpp with native tool calls, 131,072-token
context, about 141 tokens/s decode. Vornik's daemon, harness and agent image
were built from one commit. The task set is 20 `dev-pipeline` and 10
simple-workflow tasks, each run 3 times.

**Energy.** Metered at the model host's wall socket over each run window. The
meter covers the model host only, not the machine running Vornik. Priced at
8.83 CZK/kWh (36.26 euro cents): Eurostat's Czech household electricity price
for H1 2026, all taxes and charges included (`nrg_pc_204`), at the Czech
National Bank rate of 30 October 2026. Hardware cost is not included.

**What the numbers show.**

- **Vornik carries a small model to completion.** Malformed output was caught
  and re-asked (about one schema retry per run). Every step that produced
  nothing was recovered by a retry. No tool call was malformed.
- **Finishing is not the same as being right.** Task success says the
  pipeline finished. Only 4 of the 30 tasks carry an acceptance suite, so the
  correctness signal is 9 of 12 (OFF) and 11 of 12 (ON): mostly right, not
  always, on a small sample.
- **The hardening costs nothing measurable.** The one ON slot that failed
  was a model loop and two step timeouts; the daemon log of the whole arm has
  no read-only filesystem or process-limit error. One slot in 90 is not
  distinguishable from noise (Fisher's exact test, p = 1.0). Energy per hour
  differed by 1.5%.

**What this does NOT establish.**

- **No ranking against other models or the 2026.9.0 row.** The model, the
  hardware and the harness version all differ, and no comparison was
  pre-registered.
- **No noise floor.** One run per arm, three repeats per task.
- **One OFF slot was run twice.** The harness refused to start it because the
  previous task's failure left files in the workspace (a known harness
  gap), so it never reached a container. It was re-run on the same commit and configuration, outside the
  metered window, and passed.
- **Not independently reproducible.** As for the 2026.9.0 row, the task set
  and its answer key are not published.
- **The electricity price is a national household average.** A different
  tariff changes the cost in proportion.

## Agent quality — 2026.9.0 (first scored arm)

30 software tasks through a multi-agent `dev-pipeline` swarm, scored against an
operator-reviewed answer key. It measures the control logic's decisions (what
the lead granted, whether roles followed their output schemas, whether agents
called tools correctly), not retrieval.

| Release | Tasks | Task success | Schema conformance | Tool-call validity | Steps with no output | Cost/task |
|---|---|---|---|---|---|---|
| `2026.8.9-70-g5d247f72` (shipped as **2026.9.0**) | 30 | 100.0% | 0.985 | 1.000 | 9.7% | $0.29 |

Same arm: 667,801 tokens and 62.4 tool calls per task, 0 escalations, 0 schema
retries, $8.79 in total.

**Read the step figure with the success figure.** Task success is 100%, but
14 of 144 terminal steps (9.7%) produced no output: 8 hit the iteration cap,
4 entered a degenerate tool loop, 2 failed. Recovery absorbed them.

**The model is part of the result.** `Qwen/Qwen3.8-27B-FP8`, self-hosted on
one machine. Over about 244,000 tool calls on two deployments, it entered
identical-repeat tool loops 26 times as often as a mix of larger hosted models
(0.52% of calls against 0.02%), and changed approach after a nudge 36% of the
time against 82%. A self-hosted 27B is what someone running Vornik at home
has, so this row describes the harder case; a large hosted model behind the
same control logic sees the lower rate. The comparability key pins the
model, so an arm on another model cannot silently replace this row.

**Cost corrected on 2026-09-19.** First published as $0.74 per task and
$25.23 in total: the model was missing from the pricing table, so calls were
billed at the default $1.00/$3.00 per million tokens instead of $0.35/$2.75.
The corrected figures come from the arm's recorded tokens (22,353,287 prompt,
351,933 completion). They are an upper bound: this path did not report
prompt-cache reads, and a later pass on the same model served 79.5% of prompt
tokens from cache at a tenth of the input rate. Quality figures are
unaffected.

**What this row does NOT establish.**

- **No pass or fail.** `bench agent gate` refused a verdict: resolving a
  5-point effect at the inherited σ of 0.0604 needs 12 paired tasks; this arm
  has 5, which resolve 7.6 points.
- **No trend.** It is the first scored arm.
- **No noise floor for this task set.** The σ comes from an earlier 3-task
  measurement of a different set.
- **Not independently reproducible.** The task set and answer key are not
  published.

**Provenance.** The daemon binary and agent image are recorded by content
digest in the arm key, and batches that disagree on any axis are not merged.
Arms recorded before 2026-08-29 carry no agent-image identity (the executor
discarded it), so they cannot serve as a baseline. This is the first arm with
real image provenance.

## LongMemEval — retrieval, all six abilities (tier 2, cold corpus)

The open memory axis: 120 items, 20 from each of LongMemEval's six abilities,
scored without a judge (context recall, context precision and MRR against
the dataset's gold documents). The store is cleared before every run.

| Release | Items | Abilities | Context recall | Context precision | MRR | Runs | Key |
|---|---|---|---|---|---|---|---|
| `2026.8.8-40-g2698eb67` | 120 | all 6 | 0.9900 ±0.0036 | 0.9854 ±0.0000 | 0.9958 ±0.0000 | n=3 | `93a6e7a0729b` |
| `2026.8.9-50-g4b343821` | 120 | all 6 | 0.9891 ±0.0036 | 0.9854 ±0.0000 | 0.9958 ±0.0000 | n=3 | `93a6e7a0729b` |
| `2026.9.4-205-g97fb8b36e` | 120 | all 6 | 0.9851 ±0.0000 | 0.9854 ±0.0000 | 0.9958 ±0.0000 | n=3 | `28fd464fb163` |
| `2026.9.4-207-g61634a063` | 120 | all 6 | 0.9851 ±0.0000 | 0.9854 ±0.0000 | 0.9958 ±0.0000 | n=3 | `ed7ea84021e0` |
| `2026.9.6-117-gee40525c8` (2026.9.7) | 120 | all 6 | 0.9851 ±0.0000 | 0.9854 ±0.0000 | 0.9958 ±0.0000 | n=3 | `4a95cccf5f7a` |
| `2026.9.8-52-g86d3af802` (2026.10.1) | 120 | all 6 | 0.9851 ±0.0000 | 0.9854 ±0.0000 | 0.9958 ±0.0000 | n=3 | `0c4971cc78ae` |
| `2026.10.1-55-gc800c23be` (2026.10.2) | 120 | all 6 | 0.9851 ±0.0000 | 0.9854 ±0.0000 | 0.9958 ±0.0000 | n=3 | `dcfeaf709dc5` |
| `2026.10.1-77-gbb5238eb2` (2026.10.3) | 120 | all 6 | 0.9851 ±0.0000 | 0.9854 ±0.0000 | 0.9958 ±0.0000 | n=3 | `16a79559dd84` |
| `2026.10.4-171-g2213d297b` (2026.10.5) | 120 | all 6 | 0.9851 ±0.0000 | 0.9854 ±0.0000 | 0.9958 ±0.0000 | n=3 | `d2ffc5b3e11e` |
| `2026.10.5-535-g3a228c188` (2026.10.6) | 120 | all 6 | 0.9851 ±0.0000 | 0.9854 ±0.0000 | 0.9958 ±0.0000 | n=3 | `496e1e5efb22` |
| `2026.10.6-439-g67cafb8e9` (2026.10.7) | 120 | all 6 | 0.9851 ±0.0000 | 0.9854 ±0.0000 | 0.9958 ±0.0000 | n=3 | `9d32bf3c9a33` |
| `2026.10.7-130-ge8dc9aab6` (2026.11.1) | 120 | all 6 | 0.9851 ±0.0000 | 0.9854 ±0.0000 | 0.9958 ±0.0000 | n=3 | `be04eee379bd` |
| `2026.11.1-76-g8e62945d8` (2026.11.2) | 120 | all 6 | 0.9851 ±0.0000 | 0.9854 ±0.0000 | 0.9958 ±0.0000 | n=3 | `4888e3c70ece` |
| `2026.11.2-60-ge0920131c` (2026.11.3) | 120 | all 6 | 0.9851 ±0.0000 | 0.9854 ±0.0000 | 0.9958 ±0.0000 | n=3 | `83861e2a27ad` |
| `2026.11.4-93-g39bb57729` (2026.11.5) | 120 | all 6 | 0.9851 ±0.0000 | 0.9854 ±0.0000 | 0.9958 ±0.0000 | n=3 | `cc29288955b4` |

Figures from `bench memory aggregate` over three run directories; standard
deviations are sample (n−1).

**What each row adds.** Every row from the fifth on is identical to the one
before to four decimal places across three deterministic runs. Its key is new
only because `daemon_revision` moved (plus `harness_revision` for 2026.10.7
and 2026.11.1).
The dataset revision, `max_tokens`, item selection and embedder
(`openai/qwen3-embedding:0.6b@1024d`) are unchanged, and each run cleared a
store of 1,897 chunks.

| Row | Why it was measured | What it does NOT show |
|---|---|---|
| 1. `2026.8.8-40` | First run on the cold axis | Shares row 2's key, so row 2's limits apply |
| 2. `2026.8.9-50` | First release-over-release comparison: recall −0.0009 against sd 0.0036 and a 3σ threshold of ±0.0107, so no regression; sd reproduced exactly | Key PARTIAL. Reranker on (path `context-assembly+rerank`), so not deterministic by construction: right for reporting the system as shipped, wrong for a CI gate, which needs the reranker off. n=3 cannot resolve a sub-point move |
| 3. `2026.9.4-205` | First complete key (`observed_embedder` and `daemon_revision` were empty on every earlier run), so it starts a new comparable set. Recall 0.9851, −0.0040, a third of the 3σ threshold | Not a clean comparison: the extraction model was down, so the corpus was built by the embedder alone; that also explains sd 0.0000. Run with `--accept-unverified-path`, reranker off |
| 4. `2026.9.4-207` | The binary installed in production; identical metrics, new key because the build moved from `-205-…-dirty` | — |
| 5. 2026.9.7 | The retrieval path changed twice (2026.9.5 recency ranking and series supersession; 2026.9.7 document supersession, versions stored whole under artifact-salted chunk hashes) | Each item is ingested once into a cold store, so re-ingest supersession is never exercised; covered by the daemon's tests and verified live |
| 6. 2026.10.1 | The minimum deposit size from a companion client was lowered, on this benchmark's ingest path; the corpus stayed 1,897 chunks, so it admitted nothing new here | Not a test of the size floor itself |
| 7. 2026.10.2 | No-regression check (agent administration, update path, packaging) | — |
| 8. 2026.10.3 | No-regression check (macOS release check, Hermes catalog readiness, EULA 3) | — |
| 9. 2026.10.5 | Companion note ingest changed (a forgotten memory added again is reinstated; a refute takes the strictest route) and recall records each result's use | Never forgets or refutes, so those paths are covered by the daemon's tests only |
| 10. 2026.10.6 | Article 18 restriction became default-deny on every chunk read, and companion tools refuse undeclared arguments. A first attempt at `aa2dc8d5d` was refused at the leakage probe because the harness sent an undeclared `max_tokens`; the harness no longer sends it. Run 1 cleared 4 chunks (the failed attempt's leftovers) | One unrestricted project, so the restriction is exercised only on its pass-through side |
| 11. 2026.10.7 | Every recall path honours the repository scope; each hit carries a provenance label. Each item is deposited and recalled under its own scope with `strict_scope` | Deposits use class `research`, so the trust changes for `decision` and `spec` deposits and delegated ingests are covered by the daemon's tests only |
| 12. 2026.11.1 | The recall path changed (the architect answers only from the checked design record) and the embedding client now dials through the SSRF guard. Measured 2026-11-01 on `e8dc9aab6`; the commits after it are documentation only | The design-record filter applies to architect answers, which this benchmark does not ask for; covered by the daemon's tests |
| 13. 2026.11.2 | The search SQL gained a sensitivity cap, and an agent's search now records its task in the retrieval audit. Measured 2026-11-06 on `8e62945d8`; the commits after it are documentation and a test only | No role in this benchmark sets a cap, so the cap's filtering is covered by the daemon's tests (every search statement, on PostgreSQL) only |
| 14. 2026.11.3 | Recall queries are scanned for secrets before the retrieval audit stores them; the memory secret checkpoint catches short unlabelled tokens; failed embeddings back off longer and a dead-lettered chunk is retried; provenance labels say when a version was pushed and replaced, or redacted. Measured 2026-11-09 on `e0920131c`; the commits after it are a Hermes plugin fix and documentation | The benchmark's items carry no secret-shaped text and no embedding fails, so the scan, the backoff and the dead-letter retry are covered by the daemon's tests only |
| 15. 2026.11.5 | No retrieval, ingest, embedding or scoring path changed. The one memory change is an operator verb that retires a document whose file is gone (`vornikctl memory supersede-missing-documents`, GitHub #701), which the benchmark never calls. Measured 2026-11-14 on `39bb57729`; the commits after it are a settings-form check, the dispatcher's report-back wording and release documents | The verb's retirement is covered by the daemon's tests only |

Rows 2 to 13 carry `retrieval_path_unverified`: the harness trusts the
daemon's reported recall method rather than observing it.

**Per ability** (row 2's three runs):

| Ability | Context recall | Context precision | MRR | Items |
|---|---|---|---|---|
| `knowledge-update` | 1.0000 ±0.0000 | 1.0000 ±0.0000 | 1.0000 ±0.0000 | 20 |
| `multi-session` | 0.9564 ±0.0250 | 0.9667 ±0.0000 | 0.9750 ±0.0000 | 20 |
| `single-session-assistant` | 1.0000 ±0.0000 | 1.0000 ±0.0000 | 1.0000 ±0.0000 | 20 |
| `single-session-preference` | 1.0000 ±0.0000 | 1.0000 ±0.0000 | 1.0000 ±0.0000 | 20 |
| `single-session-user` | 1.0000 ±0.0000 | 1.0000 ±0.0000 | 1.0000 ±0.0000 | 20 |
| `temporal-reasoning` | 0.9833 ±0.0144 | 0.9458 ±0.0000 | 1.0000 ±0.0000 | 20 |

**Where it loses.** `multi-session` is the only ability that misses recall,
and its recall varies between runs (sd 0.0250): evidence spread across
sessions is the weakest case, and which items miss changes from run to run.
`temporal-reasoning` has the lowest precision (0.9458): it finds the right
documents and adds extra ones.

**Reproduce.** Run against a bench daemon, never production: the run
bulk-writes and clears the store.

```bash
# Mint the token against the bench daemon:
#   VORNIK_API_URL=<daemon> vornikctl companion grant \
#       --project bench --client claude-code --memory-all
export VORNIK_URL=<your-bench-daemon>
export VORNIK_COMPANION_TOKEN=<companion key with memory_read + memory_write>
export VORNIK_BENCH_DSN=postgres://<user>:<pass>@<host>:5432/<your-bench-db>?sslmode=disable

curl -sLO https://huggingface.co/datasets/xiaowu0162/longmemeval-cleaned/resolve/main/longmemeval_oracle.json
vornikctl bench memory run --system vornik --dataset longmemeval \
    --dataset-path ./longmemeval_oracle.json \
    --dataset-sha256 821a2034d219ab45846873dd14c14f12cfe7776e73527a483f9dac095d38620c \
    --database <your-bench-db> --i-know-this-wipes <your-bench-db> \
    --tier2-only --accept-unverified-path \
    --our-extraction-model "<the embedder the daemon reports>" \
    --recall-method context-assembly \
    --max-items-per-category 20 --max-tokens 4096 \
    --run-dir ./run-$i          # repeat for i in 1 2 3
vornikctl bench memory aggregate ./run-*
```

- **The harness reads `VORNIK_URL` and `VORNIK_COMPANION_TOKEN`.**
  `vornikctl`'s own `VORNIK_API_URL` and `VORNIK_API_KEY` are a different
  pair; setting only those aims the run at `http://localhost:8080`, which on a
  single host is production. The database guard is the last line of defence.
- **`--our-extraction-model` must match what the daemon embeds with**, or the
  run is refused. Read it from `whoami` (`embedder`); the key uses
  `<provider>/<model>@<dimensions>d`, for example
  `openai/qwen3-embedding:0.6b@1024d`. Leaving it unset is what made the
  2026-09-08 run's key partial.
- **Set `memory.reranker.enabled: false` on the daemon under test.**
  `--tier2-only` stops requesting the reranked path but cannot disable it, and
  refuses the run if a rerank happened. An LLM reranker reorders results
  between identical runs.

**Dataset:** LongMemEval v1-cleaned (`xiaowu0162/longmemeval-cleaned`,
September 2025), `longmemeval_oracle.json`, `sha256
821a2034d219ab45846873dd14c14f12cfe7776e73527a483f9dac095d38620c`.
**Budget:** `max_tokens=4096`. **Judge:** none.

## LongMemEval — judged accuracy (tier 1) and the judge's noise floor

**A judged number needs the judge's own variance first**, or a "win" cannot
be told apart from the judge disagreeing with itself. Ten identical runs,
same items, same models, warm corpus:

| Metric | Mean | sd | Min | Max | Narrowest defensible gate (3σ) |
|---|---|---|---|---|---|
| **judged accuracy** | **0.8241** | **0.0340** | 0.7500 | 0.8750 | **±0.1020** |
| context recall | 0.9764 | 0.0059 | 0.9722 | 0.9861 | ±0.0176 |
| context precision | 0.9410 | 0.0000 | 0.9410 | 0.9410 | exact |
| MRR | 0.9917 | 0.0108 | 0.9792 | 1.0000 | ±0.0323 |

n=10 runs × 24 `temporal-reasoning` items, key `a2fd4d245b21`. **Judge:**
`gemma4:31b`. **Answerer:** `gpt-oss:120b`, a different model family, so the
judge does not grade its own output. Both open-weight, version-pinned and
served by one provider (Ollama Cloud); a multi-provider router would add
variance to the number being measured.

- **Accuracy moved between 0.750 and 0.875 with nothing changed** but the
  models' sampling: 3 of 24 items flipped. A tier-1 gate needs ±10.2 points,
  wider than most real improvements. That is why tier 2 leads: on the same
  runs, context precision was exactly constant.
- **Context recall varies here (sd 0.0059) but not in tier 2.** These runs
  use the system as shipped, with the LLM reranker in the path; tier 2 uses
  the reranker-free path.

```bash
export VORNIK_BENCH_LLM_URL=https://ollama.com/v1
export VORNIK_BENCH_LLM_KEY=<your key>
for i in $(seq 1 10); do
  vornikctl bench memory run --system vornik --dataset longmemeval \
    --dataset-path ./longmemeval_oracle.json \
    --database <your-bench-db> --i-know-this-wipes <your-bench-db> \
    --answer-model gpt-oss:120b --judge-model gemma4:31b \
    --category temporal-reasoning --max-items 24 --max-tokens 4096 \
    --run-dir ./jv-$i
done
vornikctl bench memory aggregate ./jv-*
```

About 4.5 minutes per run on the reference deployment; the first run is
slower because it ingests the corpus.

### Comparison: hindsight 0.9.0, same items, same budget

Same machine, same six `temporal-reasoning` items, each system as it ships
(own reranker on), both cold (store or bank cleared before every run):

| System | Context recall | Context precision | MRR | Runs |
|---|---|---|---|---|
| Vornik | 1.0000 ±0.0000 | **0.9444 ±0.0000** | **0.9444 ±0.0481** | n=3 |
| hindsight 0.9.0 | 1.0000 ±0.0000 | 0.6806 ±0.0833 | 0.7292 ±0.1250 | n=4 |

- **Recall ties at 1.000.** This oracle subset has about three sessions per
  item; the full `longmemeval_s` has 38 to 62, where recall should separate
  the systems.
- **Vornik's retrieved set is tighter and better ordered** (precision 0.944
  against 0.681, MRR 0.944 against 0.729); hindsight reaches the same recall
  by returning more.
- **hindsight varies between runs** (precision sd 0.083, MRR sd 0.125, MRR
  0.667 to 0.917 over four runs) because an LLM consolidates its memories at
  ingest, so its corpus differs each time. A single hindsight run cannot be
  quoted; a gate on it would need ±0.375 MRR. Vornik's ingest is chunk and
  embed, with no model deciding what is stored.
- **Caveats.** n=6, one ability: a smoke-scale baseline, not a ranking. Both
  arms are `retrieval_path_unverified` (rerankers on). hindsight's extraction
  model was pinned to `google/gemma-4-26b-a4b-it`, with its bundled local
  embedder and reranker; Vornik used its configured local embedder. Without
  deleting hindsight's bank between runs its precision fell from 0.806 to
  0.639 on an identical repeat.

## LongMemEval — the six-item warm axis (closed)

**Closed on 2026-08-21.** Until `98037e34` the harness did not clear the store
its flag (`--i-know-this-wipes`) promised to clear, so runs repeated against
an already-ingested, warm corpus. Every run since is cold. The missing clear
moved scores: on three runs of the same 120 items, admitted deposits went
426 → 426 → 209 as the store filled, and judged accuracy moved from 0.692 to
0.750 after a manual wipe. Warm figures understate the system by an amount
nobody has bounded, because a deduplicated item loses the haystack it is
scored against.

| Release | Items | Ability | Context recall | Context precision | MRR | Runs | Key |
|---|---|---|---|---|---|---|---|
| `2026.8.3-13-g13476b8f` (baseline, pre-tracking) | 6 | temporal-reasoning | 1.0000 ±0.0000 | 0.9444 ±0.0000 | 1.0000 ±0.0000 | n=3 | `d0fa4f0f8389` |
| `2026.8.7-48-g0e450f9c` (shipped as **2026.8.8**) | 6 | temporal-reasoning | 1.0000 ±0.0000 | 0.9444 ±0.0000 | 1.0000 ±0.0000 | n=3 | `e865104e9959` |

The same six items measured cold on 2026-08-27, reranker off:

| Release | Items | Ability | Context recall | Context precision | MRR | Runs | Key |
|---|---|---|---|---|---|---|---|
| `2026.8.9-50-g4b343821` | 6 | temporal-reasoning | 1.0000 ±0.0000 | 0.9444 ±0.0000 | 1.0000 ±0.0000 | n=3 | `e865104e9959` |

- **Warm and cold are identical on these six items** once the reranker is
  off, all with sd 0.0000.
- **A cold figure of MRR 0.9444 ±0.0481** (in the hindsight comparison) was
  first attributed to an LLM titler making ingest non-deterministic. That was
  wrong: the reranker reorders results between runs. With it off, the path is
  deterministic warm and cold.
- **The 2026.8.8 row** was taken at `0e450f9c`, four commits before the tag;
  those commits touch only release notes and installer plumbing. Its key is
  PARTIAL (extraction, answer and judge models left empty), so the two warm
  rows are "not provably comparable"; every field that sets what was measured
  matches.
- **The key could not tell warm from cold until 2026-09-03**: the warm 2026.8.8
  row and the cold row share key `e865104e9959`. The key now carries an
  observed `corpus_regime` and `daemon_revision` (harness version 4 → 5).
  These rows keep their version-4 keys as history.

Item ids scored: `gpt4_2655b836`, `gpt4_2487a7cb`, `gpt4_76048e76`,
`gpt4_2312f94c`, `0bb5a684`, `08f4fc43` (the first six `temporal-reasoning`
items in file order). The rows were produced with:

```bash
curl -sLO https://huggingface.co/datasets/xiaowu0162/longmemeval-cleaned/resolve/main/longmemeval_oracle.json
scripts/bench-reproduce.sh --dataset-path ./longmemeval_oracle.json \
    --category temporal-reasoning --items 6 --runs 3 --database <your-bench-db>
```

Today this command produces a cold result: with the reranker off it
reproduces the figures above exactly (measured 2026-08-27, sd 0.0000 on all
three metrics); with it on, MRR varies between runs.

## Releases with no row, and why

The policy in `RELEASE.md` requires a row per release from `2026.8.4`. It was not
followed. Rather than leave the gaps silent — which reads as "nothing regressed" —
each is stated:

| Release | Row | Why |
|---|---|---|
| `2026.8.4` | none | No benchmark run was taken. Not backfillable: it would need the tagged binary rebuilt *and* the bench deployment's models restored to what they were, and those were changed during 2026.8.7. |
| `2026.8.5` | none | As above. |
| `2026.8.6` | none | As above. |
| `2026.8.7` | none | No run. The reason was recorded at the time in the 2026.8.7 release notes: every outward-facing provider was disabled and all roles moved to a locally served model during that cycle, so a run could not be pinned to the baseline row's axes. |
| `2026.8.8` | **yes** | Both axes above. The warm row was measured at `0e450f9c`; the cold row at `2698eb67`. |
| `2026.8.9` | none *at the tag* | No run was taken at `2026.8.9` itself. The tree past it is measured on both memory axes above. |
| `2026.9.0` | **agent row only** | The agent arm above was taken on the release candidate (`2026.8.9-70-g5d247f72`). No LongMemEval row: the memory axes were measured 50 commits earlier in the same cycle and nothing in the intervening work touches the retrieval path. Stated rather than left as a silent gap. |
| `2026.9.1` | none | No run was taken. The cycle is bug fixes and the forge re-review feature; nothing in it touches ingestion, embedding, retrieval or the agent harness, so the axes stand where `2026.8.9-50-g4b343821` (memory, both axes) and `2026.8.9-70-g5d247f72` (agent arm) left them. A row measured on an unchanged path would add a data point without adding evidence, and the cost of a 120-item n=3 run is not free. Stated rather than left as a silent gap. |
| `2026.9.2` | none | No run was taken. The cycle moves the agent loop's eleven filesystem/git tools from bash into a Go helper with behaviour pinned by a 64-case golden, and adds persistence and replay instruments; it does not touch ingestion, embedding, retrieval or the harness's scoring path, so the axes stand where the rows above left them. The agent arm *should* be re-measured on the Go helper before the next loop slice moves more of it — that is filed as work, not claimed here. Stated rather than left as a silent gap. |
| `2026.9.3` | none | No run was taken. The cycle is the update path and the pulled agent image; it does not touch ingestion, embedding, retrieval or the harness. Stated late — this row was added with 2026.9.4's — rather than left as a silent gap. |
| `2026.9.4` | none | No run could be taken: the benchmark arm's model endpoint has been unreachable since 2026-09-04 (it now accepts and immediately resets, which a port check reads as up). The cycle does not touch ingestion, embedding, retrieval or the scoring path; it does change what a multi-system-step workflow's agent receives, which is not measured here. Stated rather than left as a silent gap. |
| `2026.9.5` | none — **a gap, not a waiver** | No run was taken, and this cycle DID change the retrieval path: recall began ranking by recency and letting a recurring series supersede its earlier members. It shipped unmeasured on the axes above. Stated late — this row was added with 2026.9.7's, whose run measures the tree that includes it — rather than left as a silent gap. |
| `2026.9.6` | none | No run was taken. The cycle bounds agent containers and fixes the adoption board; the board reads the retrieval and ingest audit ledgers but changes neither path. Stated late, with 2026.9.7's, rather than left as a silent gap. |
| `2026.9.7` | **yes** | The memory open axis above, on the release build (`2026.9.6-117-gee40525c8`, n=3). The cycle changes ingestion and retrieval, so a waiver was not open to it. The v9 agent-harness arms run on the slow-hardware track and are reported in the release notes, not in this table. |
| `2026.9.8` | none | No run was taken. The patch changes only the companion endpoint's reply to a `GET` stream request, plus test fixtures. It touches no ingestion, embedding, retrieval or scoring path, so the axes stand where 2026.9.7's measured row left them. Stated rather than left as a silent gap. |
| `2026.10.1` | **memory row only** | The memory open axis above, on the release build (n=3). The cycle lowers the minimum size of a deliberate memory deposit from a companion client, and this benchmark ingests through that path, so a waiver was not open to it. **No agent row.** The cycle changes the agent harness substantially (the router step, the retry ladder, the prompt-token budget and the tool-result cap), so this is a gap, not a waiver. The agent arm's model is the self-hosted Qwen3.8-27B on the endpoint that has been unreachable since 2026-09-04, and the comparability key pins the model, so a run on any other model would start a new table that cannot show a regression against the 2026.9.0 row. A substitute arm on a locally served 20B model was considered and not run: one arm is about 20M prompt tokens, which on the reference host's integrated GPU is days, not hours. The changes are covered instead by regression tests that replay each incident, and by an end-to-end lane that drives a front-end agent through the broker on a locally served model. |
| `2026.10.2` | **memory row only** | The memory open axis above, on the release candidate (`2026.10.1-55-gc800c23be`, n=3), identical to 2026.10.1. The cycle did not touch ingestion, embedding, retrieval or scoring (agent administration, approver devices, sealed agent credentials and release packaging), so the row is the no-regression check. Stated late, with 2026.10.5's, rather than left as a silent gap. |
| `2026.10.3` | **memory row only** | The memory open axis above, on the release candidate (`2026.10.1-77-gbb5238eb2`, n=3), identical to 2026.10.2. The cycle touched no ingestion, embedding, retrieval or scoring path (the macOS release check, the Hermes plugin's catalog readiness, EULA version 3). Stated late, with 2026.10.5's, rather than left as a silent gap. |
| `2026.10.4` | none | No run was taken. The cycle changes no daemon-side ingestion, embedding, retrieval or scoring code: its memory changes are the Hermes plugin's forgetting (0.8.0), which calls the existing correction path from the client, plus tests (one measures a companion note as one chunk); the rest is broker grants, agent administration and approval transport. The axes stand where 2026.10.3's row left them, and 2026.10.5's row measures the tree that includes this cycle. Stated late, with 2026.10.5's, rather than left as a silent gap. |
| `2026.10.5` | **memory row only** | The memory open axis above, on the release candidate (`2026.10.4-171-g2213d297b`, n=3), identical to 2026.10.3. The cycle changes the companion note ingest path (memory reinstate after a forget, the refute route) and makes recall record each returned result's use, so a waiver was not open to it. No agent row, for the reason stated under 2026.10.1: the arm's model endpoint is still unreachable. |
| `2026.10.6` | **memory row only** | The memory open axis above, on the release candidate (`2026.10.5-535-g3a228c188`, n=3), identical to 2026.10.5. The cycle makes the Article 18 restriction default-deny on every chunk read and makes recall refuse undeclared arguments, so a waiver was not open to it. This entry records the memory row only; no agent row is published for this release in this table. |
| `2026.10.7` | **memory row only** | The memory open axis above, on the release candidate (`2026.10.6-439-g67cafb8e9`, n=3), identical to 2026.10.6. The cycle changes the recall paths (repository scope on every path, a provenance label on every hit), so a waiver was not open to it. This entry records the memory row only; no agent row is published for this release in this table. |
| `2026.11.1` | **yes, both** | The memory open axis above, measured on `2026.10.7-130-ge8dc9aab6` (n=3), identical to 2026.10.7; the cycle changed the recall path, so a waiver was not open to it. The agent axis: the open-weight arms in [Agent quality](#agent-quality-a-4b-active-open-weight-model-on-one-gpu-2026-10-31), measured on `2026.10.7-94-g3246baad0`, part of this release; they start a new table (different model and hardware from 2026.9.0). |
| `2026.11.2` | **memory row only** | The memory open axis above, measured on `2026.11.1-76-g8e62945d8` (n=3), identical to 2026.11.1; the cycle changed the search SQL (the sensitivity cap) and the retrieval audit, so a waiver was not open to it. **No agent row, and this is a gap, not a waiver.** The cycle adds the `writing-style` guidance block to every agent prompt, which the agent axis would measure; the arm was not re-run for this release. The agent axis therefore stands at 2026.11.1's row, which does not include the block. |
| `2026.11.3` | **memory row only** | The memory open axis above, measured on `2026.11.2-60-ge0920131c` (n=3), identical to 2026.11.2; the cycle changed the memory checkpoint, the recall audit, the embed retry and the provenance labels, so a waiver was not open to it. **No agent row.** An agent arm did run on this release's code (`gemma-4-26b-a4b`, 30 tasks × 3, harness 11 at `35fb8e0b0`), but it was pre-registered as the end-to-end check of GitHub #655, not as a scored arm, so its numbers are not published as a row. Harness 11 counts a run that stops at a checkpoint as not finished, which the harness behind the 2026-10-31 rows counted as finished; the next scored arm starts a new table for that reason. On that check: 70 of 90 runs finished, 19 stopped at a checkpoint, 1 failed, and no task read an earlier task's work from git. |
| `2026.11.4` | none | No run was taken. Its release notes say so: no retrieval, memory or agent-prompt code changed since 2026.11.3 (the memory, chat and benchmark code differed in comments only). Stated late, with 2026.11.5's, rather than left as a silent gap. |
| `2026.11.5` | **memory row only** | The memory open axis above, measured on `2026.11.4-93-g39bb57729` (n=3), identical to 2026.11.3. The cycle's memory change (a verb that retires documents whose file is gone) is off the benchmark's path, so the row is the no-regression check. **No agent row.** The cycle adds no agent-prompt guidance the agent axis measures; the dispatcher prompt changed (no follow-up promise on a channel that cannot deliver one), and the dispatcher is not on the agent axis. |

A missing row stated as missing is honest. An absent row is not — which is the whole
