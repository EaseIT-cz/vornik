# Forge review publication and diff coverage

## As built (2026-11-05)

`forge.fetch_diff` resolves the Forge provider and returns the full or incremental
diff as both `message` and `diff`, with repository, PR number, scope, outcome,
and head SHA metadata. The `no-change` case is an explanatory sentence rather
than an empty diff. A genuinely empty provider response in the full or
incremental case is accepted and sent to the reviewer. The GitHub provider's
shared `do` helper reads at most 1 MiB from a response but ignores both the read
error and whether the body exceeded that limit. A larger diff can therefore be
silently shortened.

`forge.post_review` converts structured reviewer feedback and summary into a
body, or falls back to raw reviewer output. It appends an optional request quote
and an AI disclosure footer. On a failed-CI approval it instead posts a comment
with a withheld-approval notice, request quote, and disclosure footer. Both
publication paths call the Forge provider without checking the final body for
credentials. The scheduler builds the configured shared secrets detector
before it registers this handler, but does not pass the detector to Forge.

The 2026-11-04 TST1 synthetic run observed a fabricated Basic Auth value
repeated and decoded in the public review for PR #63. That value was not a live
credential. The run also observed a false positive on a public KMS ARN in PR
#62. These are production-level observations; the source paths above are
confirmed by inspection at the TST1 build's source revision.

## Change proposal (2026-11-05)

Before either review or comment publication, scan the exact final body with the
configured shared detector. If the detector is absent or reports any strong
credential finding, refuse the provider call with a generic error that contains
no matched text. Supplement it with targeted checks for explicit Basic Auth
material, detached Base64 candidates that decode in memory to printable
username:password text, and short assignments to sensitive keys; do not block
on entropy alone. Keep public identifiers such as ARNs, account IDs, UUIDs, and
commit SHAs publishable.

Reject zero-byte provider diffs for full and incremental review scopes while
preserving the explicit `no-change` explanatory result. For the 1 MiB HTTP
response cap, read at most one byte beyond the limit, propagate read errors, and
refuse oversized responses so truncation cannot look complete.

This change prevents Vornik from amplifying detected credentials into review
comments; it cannot make an already-public credential private or revoke it.
Repository owners must rotate exposed credentials. It also does not claim that
heuristic detection catches every possible secret format. This patch does not
anchor the GitHub review API request to the exact head SHA that produced the
diff, and it does not resolve the model's false-positive verdict on public KMS
ARNs observed in PR #62. Both remain separate production-readiness gaps.

## Verification contract

- On each publication path, a detected credential or missing detector produces
  an error and zero provider calls; safe public identifiers are still posted.
- Exercise structured feedback, structured summary, raw fallback, quoted
  request text, and the appended footer as parts of the final scanned body.
- Empty full/incremental diffs and oversized HTTP responses fail before model
  review; the explicit `no-change` path stays successful and does not trigger a
  review.
- Normal diffs below the cap remain byte-for-byte intact.
