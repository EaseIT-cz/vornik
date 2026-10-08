# Verification record

## Independent documentation audit

Task: `task_20261008110731_26a02509dfb364e5`.
Workflow: `companion-doc-review`.
Scope: `github.com/EaseIT-cz/vornik`.

The reviewer received public repository source documents as inline file attachments, with instructions to use those documents only, identify Community Edition boundaries and hidden prerequisites, and write an OUTPUT audit. The workflow has no network access; URLs in those documents were incidental references. It was not asked to fetch them or publish anything.

The initial upload was rejected because renamed upload filenames did not match their `path` field. Resubmission omitted the optional path identity; these review attachments are snapshots, not a source-document re-ingestion exercise.

Observed terminal state: `COMPLETED`, finished 2026-10-08 at 09:16:33 UTC. Retrieved the actual `vornik-101-audit-20261008-98c1.md` and `review-20261008-98c1.md` output artifacts. The reviewer audited the original outline and attached source documents, not the final shortened guide.

## Audit findings checked locally

- **Accepted:** the introductory workflow guide omits the executable graph needed for the shipped template. Confirmed against the parser and template. The short guide now says to copy an executable template, preserve role/output contracts, authorize the new ID, and validate/reload/confirm/run.
- **Accepted as a teaching concern:** distinguish plugin instructions, knowledge procedures, and packaged swarm capabilities. The short guide explains all three. This is not evidence of a storage-path bug: the knowledge guide already distinguishes the kinds of skills.
- **Accepted:** avoid implying a built-in scheduled weekly handoff. The guide presents a manually run team routine and defers scheduling until its triggers and controls are agreed.
- **Accepted:** make the client/daemon filesystem split and no-network workflows explicit. Both appear in the short guide.
- **Rejected:** the audit calls the reference architecture and Hermes plugin potentially Enterprise without supporting evidence. The reference architecture exists in public docs; neither is required by this guide. No speculative edition claim was adopted.
- **Not treated as a blocking defect:** six total skills versus five operator skills are different categories. The guide does not depend on a hard-coded skill count. The stale README omission of `vornik-admin` is outside the requested teaching path.
- **Corrected interpretation:** a repo scope is not authorization isolation; explicit overrides are possible. The guide states this directly.

Some returned line references were implausible (for example `32768+`), and recency alone was presented as evidence of correctness. Only claims checked against the actual source were used. The final draft should still receive learner review; an automated source audit does not establish its reading time.

## Lesson 1 live exercise

Task: `task_20261008110836_7e9870fe27c1addd`.
Workflow: `companion-report-summarize`.
Scope: `vornik-101-training`.
Observed terminal state: `COMPLETED`.
Observed result: nonempty `summary-20261008-948d.md` artifact.

The five required factual checks passed. The introductory wording inferred a held training session from a training-day label; a human should remove that unsupported framing. This is an example of why the course assesses actual contents rather than completion state alone.

This execution took place on an existing personal project, not a new CE installation. See `research.md` for the checkout, CLI and daemon revision differences and unexecuted setup steps.

## Final draft checks

The main guide contains approximately 1,190 whitespace-delimited words, including code and diagrams. Its 10–15 minute budget is an estimate allowing time to consider the examples; it has not been timed with learners. All local Markdown link targets in the guide folder were checked and exist. Whitespace checks passed. No application code changed, so runtime test suites were not rerun.
