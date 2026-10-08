# Optional lab: Connect Claude and finish one task

This detailed exercise is outside the short guide’s 10–15 minute reading budget.

**Time:** about 30 minutes, plus execution time. **Outcome:** Claude Code can identify its Vornik project, delegate meeting notes, and retrieve a reviewed brief.

## 1. Understand the loop

Claude is your interactive assistant. Vornik's daemon queues work and runs agents in containers. A **project** groups configuration and memory; its **swarm** supplies agent **roles**. A **workflow** describes the steps; a **task** is one execution. An **artifact** is a file produced by that execution.

You will supply synthetic notes, delegate a summarization workflow, do another small piece of work while it runs, and return for the result. This task needs neither browsing nor RAG.

## 2. Confirm the operator handoff

Ask the operator for the service URL, training project ID, your individual companion credential through a secure channel, and confirmation that `companion-report-summarize` is available. The operator must provision a functioning deployment and summarizer role first.

For an operator on the Vornik host, the repository runbook supplies this project-creation pattern. Replace the config path if the daemon reads another tree:

```bash
vornikctl init project --template companion vornik-101 \
  --config-dir "$HOME/.config/vornik/configs"
vornikctl config reload
vornikctl config reload-status
vornikctl project list
```

Check that `vornik-101` appears and the reload reports no validation errors. Confirm the generated project has `autonomy.enabled: false`. If the project already exists, inspect and use it rather than running creation again.

The operator issues one learner key using their existing operator-authenticated CLI configuration:

```bash
vornikctl companion grant \
  --project=vornik-101 \
  --client=claude-code \
  --label="learner-01/training" \
  --workflows=companion-report-summarize \
  --repo-scope=vornik-101-training \
  --budget-usd=5 \
  --memory-all \
  --skill-read --skill-write
```

The USD 5 cap is an example lifetime cap for the key, not a daily allowance or guaranteed course price. The command displays the secret once; capture it privately, outside assistant output, course files, and git. This key allows future memory/skill exercises but does not approve skills or edit daemon configuration. Later workflow lessons require the operator to authorize the new workflow too.

Check the installed command before using it:

```bash
vornikctl version
vornikctl companion grant --help
```

## 3. Load the Claude Code companion

On the machine where you run Claude, set the endpoint and enter your token without echoing it or writing its value into shell history. This example uses Bash and is session-scoped:

```bash
export VORNIK_URL="http://localhost:8080"
read -r -s -p 'Vornik companion token: ' VORNIK_COMPANION_TOKEN
export VORNIK_COMPANION_TOKEN
```

Use `http://localhost:8080` only when Claude and the daemon run on the same machine. For a remote daemon, replace it with the confirmed HTTPS service URL before entering that service's credential.

Start Claude from this shell. Inside Claude Code, replace `/ABSOLUTE/PATH/TO/vornik` with your local checkout path:

```text
/plugin marketplace add /ABSOLUTE/PATH/TO/vornik
/plugin install vornik-companion@vornik
```

Start a fresh Claude Code session from the same credential-bearing shell after installation. Run `/help` and check for `vornik-companion` commands. The shell environment lasts only for this shell and its child processes; use your own secure credential-loading arrangement for later sessions.

## 4. Verify identity and workflow access

Paste this into Claude:

```text
Use Vornik's whoami tool with repo_scope="vornik-101-training",
then its catalog tool. Report the project ID, client kind, daemon revision,
effective repo scope, and whether companion-report-summarize is available.
Do not delegate anything yet. Do not display credentials.
```

Check that the project is the operator's training project and the effective scope is `vornik-101-training`. The catalog must include `companion-report-summarize`. The plugin command `/vornik-companion:whoami` is another identity check; use `/help` for the installed command names.

If the identity is wrong, stop and correct the connection before submitting work. A scope token does not grant access to another project. Catalog reports the workflows and network capabilities available to this key; do not assume all workflows can browse.

## 5. Delegate inline notes

Paste this prompt into Claude:

```text
Delegate the following synthetic training task to Vornik using
workflow="companion-report-summarize" and
repo_scope="vornik-101-training". Return the task ID immediately.
Use only the inline content. Do not publish or send messages.

Task prompt:
target_audience: operator
Produce a brief with key points and caveats in artifacts/out/summary.md.
Separate confirmed commitments, blockers, and unconfirmed information.
Do not invent owners or dates.

input:
Team Cedar — training day 2026-10-08.
Confirmed: Mira owns the onboarding checklist update, due Friday 2026-10-09.
Confirmed: Jon owns the release notes draft, due Monday 2026-10-12.
Blocked: The demo environment is unavailable; no owner or recovery date
has been assigned.
Decision: Require one peer review before sharing any weekly handoff.
Unconfirmed: A customer demo may happen next week; date and owner are unknown.
```

Save the returned task ID. While Vornik runs, ask Claude to draft a three-item checklist for reviewing the expected brief. This illustrates the background-work loop without paying for another delegation.

The notes are inline deliberately. A daemon container cannot read a file on your laptop merely because you mention its path. When later exercises use files, upload their bytes through the companion upload command or `inputArtifacts`.

## 6. Retrieve and review

Replace `TASK_ID` with the returned ID and paste:

```text
Call Vornik status for TASK_ID. If completed, call result for that same ID
and show the returned summary artifact. If still running, report its state
and poll hint. If failed, report the failure and stop before retrying.
```

Return later if it is running. Initial image pulls or model warmup can take longer than subsequent tasks. `result` can return `complete: false` while execution is still in progress.

Check the actual artifact against the notes:

- Mira and Jon retain their own actions and exact due dates.
- The unavailable demo environment remains a blocker with unknown owner and recovery date.
- The customer demo remains tentative, with no invented date or owner.
- The peer-review decision is preserved.
- A summary file is returned, rather than only a completion message.

Record the task ID, the retrieved brief, and your review verdict in your training notes. Keep credentials out of that record.

## Recovery

| Symptom | Next check |
|---|---|
| Plugin tools absent | Confirm plugin installation, inspect `/help`, and start Claude from the shell that exported the variables |
| Authentication fails | Confirm the endpoint and its own credential; ask the operator to inspect expiry/revocation |
| Wrong project or scope | Correct the credential or explicit scope; rerun `whoami` |
| Workflow absent | Ask the operator to check project workflow configuration and key allowlist; do not guess another workflow ID |
| Task failed | Inspect its failure; the operator can use `vornikctl task explain` and the relevant playbook |
| Completed without usable output | Retrieve the artifact and check its contents; completion alone is not the exercise's success condition |

## Completion check

You can explain which work Claude performed and which work Vornik performed. You have verified identity, an allowed workflow, one task ID, and an actual brief that passes the factual checks. Your reusable artifact is a reviewed daily brief plus a connection checklist.

Return to the [short guide](index.md) for the RAG, workflow, skill, and team-reuse loop.

Sources: [companion setup](../../../../contrib/claude-code-companion/README.md), [companion features](../../features/companion.md), and the executable `configs/workflows/companion-report-summarize.md` template in the targeted checkout. See the [research brief](research.md) for verification limits.
