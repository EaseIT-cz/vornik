# Vornik 101: Your AI Work, Remembered and Reusable

**A 10–15 minute guide to the open-source Community Edition.** You already use Claude Code or Codex. This explains where Vornik fits, how to start using it, and how to reuse your work with a team. Installation and task execution take additional time.

## 1. Where Vornik fits

You ask Claude to prepare a daily brief. It needs yesterday's decisions, today's notes, and your team's review process. Vornik gives this work a persistent home: project memory, reusable workflows, and background execution whose results you can retrieve later.

```text
You <--> Claude Code
              |
              | companion connection (MCP)
              v
       Vornik project
       +--------------------------------+
       | RAG       facts and decisions  |
       | Workflows steps for doing work |
       | Skills    reusable procedures  |
       +--------------------------------+
              |
              v
       Agents run a task --> Result artifact
                                  |
                                  v
                        You + Claude review it
```

A **project** groups work, configuration, and memory. A **swarm** supplies agents with **roles**, such as summarizer or reviewer. A **workflow** describes executable steps. A **task** is one run. An **artifact** is a resulting file, such as your daily brief.

Keep the interaction in Claude: frame the work, retrieve context, delegate a bounded task, and review its answer. Vornik continues the delegated work while you do something else.

## 2. Connect Claude and verify where you are

Start with a working Vornik deployment. Its operator supplies the service URL, a companion project, available workflows, and **your own companion key**. Use a training project with autonomy disabled. A companion key does not automatically grant configuration administration.

On the machine running Claude, load the service URL and its credential into `VORNIK_URL` and `VORNIK_COMPANION_TOKEN` through your secure credential setup. Keep token values out of prompts and git. `localhost` works only when Claude and the daemon are on the same machine.

Install the repository's companion plugin inside Claude Code, replacing the path:

```text
/plugin marketplace add /ABSOLUTE/PATH/TO/vornik
/plugin install vornik-companion@vornik
```

Start a fresh session that inherits those variables. Ask:

```text
Use Vornik whoami with repo_scope="vornik-101-training", then catalog.
Tell me my project, effective scope, allowed workflows, and budget cap.
Do not display credentials or submit work yet.
```

Check the project before continuing. **Identity says where you are; catalog says what this key can run.** The [optional connection lab](lesson-01.md) has operator and shell steps. No deployment yet? Start with [installation](../../getting-started.md).

## 3. RAG remembers facts

RAG retrieves stored project knowledge relevant to your question. Use it for decisions, constraints, and verified facts that should survive your current chat.

For our fictional Team Cedar, ask Claude:

```text
Remember this synthetic decision in Vornik under
repo_scope="vornik-101-training":
"Team Cedar decided on 2026-10-08 that every weekly handoff
requires one peer review before sharing."
Then recall the team's handoff review rule and show its provenance.
```

Try recalling it in a fresh session. Check the source and date before treating a hit as current truth. If a decision changes, use `memory_correct` to refute the old claim and record its correction.

Semantic recall needs a reachable embedding model and PostgreSQL/pgvector. Working chat alone does not prove embeddings work; `whoami` reports embedding readiness. New notes can take time to become searchable. Your key needs memory read/write permissions.

A specific `repo_scope` organizes knowledge for that repository or training context. `*` is cross-cutting memory within the project. Strict recall excludes legacy unscoped notes but still includes `*`. **Repo scope is a retrieval filter; project/key permissions control access.**

## 4. Workflows make work repeatable

A prompt says what you want this time. A workflow stores the steps and their instructions so you can run and improve the process repeatedly.

Start with a summarization workflow already present in your catalog:

```text
Recall Team Cedar's handoff rule under repo_scope="vornik-101-training".
Include the relevant recalled decision in a delegation to
companion-report-summarize using that same scope.

Inline notes:
Mira owns the onboarding checklist, due 2026-10-09.
Jon owns release notes, due 2026-10-12.
The demo environment is unavailable; owner and recovery date are unknown.
A customer demo next week is tentative; date and owner are unknown.

Ask for a brief with commitments, blockers, and uncertainties.
Do not invent owners or dates. Return the task ID immediately.
```

Later, ask Claude to call `status` and then `result` for that ID. Inspect the returned artifact: a `COMPLETED` state does not establish factual correctness.

To create your own daily-brief workflow, have the operator copy a compatible shipped executable template under a new ID. Preserve its steps, role bindings, and output contract; change the step prompt to your required brief format. Save it in the daemon's actual workflow directory and authorize it for your key.

The operator checks and applies the change:

```bash
vornikctl workflow validate /ACTUAL/CONFIGS/workflows/daily-brief.md
vornikctl config reload
vornikctl config reload-status
```

Then run it on the same input and compare the result. Validation checks the file; **reload confirmation and a real run prove it is usable**. Keep the file in version control. See [workflow details](../../guides/workflows-and-llm-controls.md).

The daemon cannot read a laptop file because its path appears in a prompt. Supply inline text or upload its bytes. Check catalog before requesting web research: some companion workflows have no network access.

## 5. Skills preserve procedures

RAG stores “the team decided this.” A knowledge skill stores “when preparing a handoff, follow these steps.”

Once your brief format works, ask Claude:

```text
Search Vornik knowledge skills for a handoff review procedure.
If there is no suitable existing skill, propose a project-scoped draft:
preserve known owners and dates, list unknowns explicitly,
and obtain one peer review before sharing a weekly handoff.
Show me the proposed procedure for human review. Do not approve it.
```

Learners need skill read/write permissions. An operator reviews and approves the draft using companion skill tools with `skill_admin` permission. In Community Edition, use these tools rather than relying on the Enterprise admin inbox. Approval makes the procedure eligible for use; check that the next task actually applies it.

Three different things may be called skills: Claude plugin skills guide your local assistant; Vornik knowledge skills preserve procedures; packaged `SWARM-SKILL.md` capabilities bundle workflow/role functionality. Start with knowledge skills for team know-how.

Keep this procedure project-scoped. A **global** knowledge skill can reach roles across every project on the daemon. See [knowledge skills](../../features/knowledge-skills.md).

## 6. Put it together every day

Use this loop:

```text
Recall relevant facts
        -> run the workflow
        -> review the artifact and apply the procedure
        -> remember a new durable fact
        -> propose a skill if a reusable procedure emerged
```

Give teammates the workflow file, a synthetic input, the expected output checks, and the relevant skill. Each person gets their own key for the appropriate team project. They use the same agreed scope and retrieve context from Vornik, instead of needing your original transcript.

Start manually. Consider scheduling after the routine produces useful results and you have agreed its triggers, costs, and review points. Community Edition is free software; model-provider and infrastructure costs still apply.

You are ready to begin if you can explain where facts, steps, and procedures belong—and can use these prompts to connect, delegate, retrieve, and review a brief. Hands-on setup is in the [optional lab](lesson-01.md); author evidence is in the [research brief](research.md).
