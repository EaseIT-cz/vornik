# Vornik 101 research brief

**Scope update:** the user requested a short 10–15 minute guide after the initial course draft. `index.md` is now the complete main reading path. The longer connection lab is optional. See `requirements.md` and `outline.md`.

Research date: **2026-10-08**. Repository target: commit `39e0414dadd4478eef9765df3ab4cb6ab75265c6` (`2026.8.9-47-g39e0414d`). This is a checkout-specific course draft, not a claim that every released version behaves identically.

Observed tools: installed `vornikctl version` reports **2026.10.3, Community Edition**; the personal EaseIT companion service reports daemon revision **2026.10.5**. Its edition was not independently established. Remote execution evidence therefore checks the companion exercise on that service, not the complete targeted checkout on a fresh Community installation.

## Evidence map

Paths below are relative to the repository root; Markdown source links are relative to this brief.

| Learning outcome | Primary evidence | Edition and prerequisites | Observable exercise |
|---|---|---|---|
| Explain background execution | [concepts](../../concepts/index.md), [companion](../../features/companion.md) | CE; running daemon and agents | Explain session/task/artifact boundaries using one real task |
| Connect Claude and verify access | [plugin README](../../../../contrib/claude-code-companion/README.md), [grant CLI](../../../../internal/cli/companion.go), [MCP handler](../../../../internal/api/companion_mcp.go) | CE; plugin, endpoint-specific key, project and workflow allowlist | `whoami`, `catalog`, matching project/scope |
| Delegate and retrieve results | [summarization workflow](../../../../configs/workflows/companion-report-summarize.md), plugin delegate/upload commands | CE; configured summarizer role and writable output artifacts | Task ID, terminal state, retrieved brief with preserved facts |
| Use persistent RAG | [memory guide](../../features/memory-rag.md), companion scope section, [scope regression tests](../../../../internal/api/companion_mcp_scope_fixes_test.go) | CE; memory grants; PostgreSQL/pgvector and reachable embeddings for semantic search | Deposit, fresh-session recall, provenance and correction |
| Create and adjust workflows | [workflow guide](../../guides/workflows-and-llm-controls.md), [parser](../../../../internal/registry/workflow_md.go), [validator](../../../../internal/registry/workflow_md_validate.go), executable templates | CE; operator config access, deployed role and key authorization | Validate → reload → confirm → run; compare two outputs |
| Reuse knowledge skills | [knowledge guide](../../features/knowledge-skills.md), [skill MCP tests](../../../../internal/api/companion_mcp_skills_test.go), [executor selection](../../../../internal/executor/skills_context.go) | CE tool/CLI path; read/write grants and human operator with admin grant | Draft, approval, retrieved body and demonstrated application |
| Combine daily work and team reuse | Same companion, memory and skill contracts; [edition matrix](../../editions.md) | CE; shared team project, individual credentials and versioned artifacts | Second person reproduces the loop without original transcript |

The daily brief and Team Cedar scenario are instructional proposals, not existing product features. Team reuse means shared configuration and appropriate project access; it does not require Enterprise cross-project orchestration.

## Findings that shape the course

1. **Separate setup from use.** The companion does not provision a usable deployment merely by connecting. An operator must supply the configured daemon, project, roles, keys, models, and later workflow authorization.
2. **Embeddings need their own readiness check.** A chat model working is insufficient evidence that semantic recall works. The memory guide specifies PostgreSQL/pgvector; SQLite has keyword recall instead. Lesson 2 must check `whoami`'s readiness and queue information.
3. **Use executable workflow examples.** The public workflow guide's introductory `name`/description/prompts example omits an executable step graph. The parser binds body headings to declared steps; a heading for an undeclared step is rejected. The metadata validator accepts `name` or legacy `workflowId`, but metadata validation is not runtime readiness. Start from the shipped executable template, preserve its graph/role/output contract, and confirm a real run after reload.
4. **Check reload results.** `config reload` applies state; `config reload-status` exposes errors. Neither a file saved locally nor a successful metadata check proves the daemon reads that directory.
5. **Send bytes or inline context.** File paths on the client are not attachments. File-bearing review workflows require input artifacts. A URL in a no-network workflow does not become research.
6. **Distinguish scopes and access.** Scoped recall also includes `*`; non-strict recall may include legacy NULL chunks. Scope tokens can be explicitly overridden and do not establish authorization isolation. Use separate projects and properly scoped credentials where access boundaries are needed.
7. **Teach three kinds of skills.** Client plugin instructions, daemon-owned knowledge procedures, and packaged swarm capability skills have different lifecycles. Knowledge proposals remain drafts until approved. The CE route uses companion tools/CLI, not the Enterprise admin inbox.
8. **Prove skill application.** In this checkout, execution receives an index of eligible approved skills and fetches bodies on demand. Selection is project/role based across repo scopes. Approval does not prove every task loaded the full body or applied it; use a task requiring the procedure and inspect the resulting evidence.
9. **Keep manual operation first.** Training companion projects disable autonomy. Scheduling is a later optional extension with explicit budgets and review points.

## Published-source check

The published [companion page](https://docs.vornik.io/features/companion/), [knowledge-skills page](https://docs.vornik.io/features/knowledge-skills/), and [workflow guide](https://docs.vornik.io/guides/workflows-and-llm-controls/) were consulted. They support the course's companion, skill, and workflow topics. The retrieved companion page was older than the checkout and lacked some newer material; examples are anchored to the local target and installed help rather than treating the website as a release-specific contract.

## Verification performed

- Executed read-only companion `whoami` and `catalog` on the configured personal EaseIT endpoint. Confirmed memory grants and availability of the required workflows.
- Executed scoped recall before delegation. Results were unsuitable as course evidence; only public source files and synthetic input were used for course artifacts.
- Inspected CLI help for companion grants, workflow validation, and reload status. A version probe using `--version` failed; the supported `version` subcommand was then checked. Lesson commands use the supported spelling.
- Read the workflow parser/validator, template, scope regression tests, skill permission/draft tests, and executor skill-selection implementation. Existing tests were inspected, not executed.
- Ran the Lesson 1 synthetic summarization input remotely: task `task_20261008110836_7e9870fe27c1addd` completed and `result` returned a nonempty summary. It preserved both owners/dates, the blocker, the peer-review decision, and uncertainty about the demo. It also called the notes a held training session, which the input did not establish: human review should remove that framing. The returned artifact was named `summary-20261008-948d.md`; learners should retrieve the actual returned artifact rather than assume its external name matches an internal output path.
- The live exercise used an existing personal companion project with an explicit training repo scope. It was not a freshly provisioned isolated training project; scope is not project isolation. No configuration changes, skill approvals, service restarts, or publication were performed.

## Remaining verification and development

- Run the full setup flow on a fresh CE training deployment and actual Claude Code installation. Plugin installation and operator project/key provisioning were documented, not executed here.
- If deeper practice is requested, develop optional workflow/RAG/skill exercises. Verify the targeted workflow loads in CE and that changing its prompt changes its output. These are not required chapters of the short guide.
- Execute RAG deposit/correction and fresh-session recall in an isolated training project; test the configured embedder rather than infer readiness from chat.
- Demonstrate an approved procedure's application and a second person's credential-based reproduction. Keep approval separate from proposal.
- Learner-test the 10–15 minute reading estimate and accessibility of the setup instructions.

The independent Vornik documentation audit is tracked separately in `verification.md`.
