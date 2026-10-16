---
name: develop-with-memory
description: |
  Teaches Claude how to change code in a repository whose project memory is
  in Vornik, for ANY project: recall before reading code, design before
  changing it, keep memory current, and, where memory holds no design for the
  part being changed, write an as-built record of that part, have it
  reviewed, then ingest it. Use when the vornik-companion tools are available
  and you are investigating, fixing or changing code. Never claims a design
  exists: it reads each recall's design_record count.
---

In this harness the tools are `mcp__plugin_vornik-companion_vornik__recall`,
`mcp__plugin_vornik-companion_vornik__remember`,
`mcp__plugin_vornik-companion_vornik__memory_correct` and
`mcp__plugin_vornik-companion_vornik__delegate`; they may arrive deferred, so
load them with ToolSearch before the first call. The repository's scope is the
one the SessionStart directive names. To send files to a workflow, use
`/vornik-companion:upload` (for example
`/vornik-companion:upload companion-doc-review "<what to check>" <record> <files>`);
to ingest a reviewed record, `/vornik-companion:rag-ingest <record>`.
The SessionStart hook prints this text at every session start and after each
compaction; set `VORNIK_DESIGN_FIRST=off` (for one repository, in its
`.claude/settings.local.json` `env`) to leave rules 4 to 8 out.

<!-- vornik-dev-guidance:start -->
# Developing with project memory

These rules are for changing code in a repository whose project memory is
kept in Vornik. They adapt to what that memory holds: many projects have few
written designs or none, and nothing here lets you claim a design exists.

## Investigate with memory first

1. **Recall before reading code** for a design, status, capability or "how
   does X work" question, an error or a surprising behaviour, and before your
   third read into one part of the code. Pass the repository's scope when
   your recall tool takes one. Read code to verify what a hit names, not to
   rediscover what memory holds: memory freezes facts when they are written.
2. **Read the recall's `design_record` before trusting a hit.** It counts the
   design-class hits (spec or decision) that carry this repository's scope
   among the hits returned (with no scope, the whole project's). Above zero,
   those hits are the design record for the question: check that a file,
   flag or function they name still exists. At zero, or when the field is
   absent, no design for this question was found in this repository's
   memory, whatever else ranked: hits from other repositories and notes are
   not its design. Say so, and work from the code. A design counts only when
   it was stored with this repository's scope. Every other hit (research,
   incident and companion notes) is a hint to verify, never a fact.
3. **Keep memory current.** When you change a design, re-ingest it in the
   same change, under the same path, so the old version stops being served.
   When a recalled fact contradicts the code, correct it (`memory_correct`
   with the hit's chunk id, then the corrected fact) instead of working
   around it.

<!-- vornik-design-first:start -->
## Design before changing code

Skip rules 4 to 8 when the user has said this project keeps its designs
elsewhere or does not work design first.

4. **Recall the design of the part you are about to change, before editing
   it.** Then one of three is true:
   - the design is silent on the change: add the change to it;
   - the design contradicts what the code does: correct it, dated, and mark
     the stale text where it stands;
   - there is no design (rule 2 found none): do not invent one, and do not
     skip this rule; offer the user rule 5.
5. **Write the as-built record, one part at a time.** With the user's
   agreement, write a design of what that part does today, from its code,
   its configuration and its observed behaviour, measured where you can (run
   times, sizes, counts), and say which statements you measured and which
   you inferred. Cover only the part the work touches, never the whole
   repository. Put it where the project keeps designs; when it keeps none,
   ask the user where. Head it "as-built, reverse-engineered on <date> at
   commit <sha>" and list the files it describes.
6. **Have it reviewed before anything relies on it.** Send the record and the
   files it describes to review (with `delegate`, the companion-doc-review
   workflow, which checks a document against the code it describes). Fix
   what is real and repeat until the review finds nothing blocking; after
   three rounds, ask the user. Without `delegate`, the user is the reviewer.
7. **Ingest only a reviewed record**, with this repository's scope and the
   file's path in the repository (the companion-rag-ingest workflow, or
   `remember` with class `spec` when it is small), so memory never serves an
   unreviewed draft as a design. Then make the change as a dated section of
   the same record, test first, and amend the record with what shipped.
8. **When you edit a file a record lists, amend that record in the same
   change** and re-ingest it. A record whose files changed after its commit
   is stale until someone checks it.
<!-- vornik-design-first:end -->
<!-- vornik-dev-guidance:end -->
