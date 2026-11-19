---
workflowId: "github-app-review"
displayName: "GitHub App PR Review"
description: "The review an App-backed project runs (GitHub #801). The diff is masked, and the mask proven, before any model reads it. Two hunters, a triager and an adjudicator see only the masked diff. Findings off an added line are dropped. The review posts as a comment with a neutral check run."
version: "1.0.0"
author: "Vadim Grinco <vadim@grinco.eu>"
license: "Proprietary"
maxStepVisits: 4
maxIterations: 30
entrypoint: "fetch_diff"
maxWallClock: "45m"
steps:
  fetch_diff:
    type: "system"
    handler: "forge.fetch_diff"
    mask_secrets: true
    on_success: "readability"
    on_outcome:
      no-change: "nothing_to_review"
    on_fail: "failed"
    timeout: "5m"
  readability:
    type: "system"
    handler: "text.readability"
    on_success: "fetch_ci"
    on_fail: "fetch_ci"
    timeout: "2m"
  fetch_ci:
    type: "system"
    handler: "forge.fetch_ci"
    on_success: "hunt_a"
    on_fail: "hunt_a"
    timeout: "2m"
  hunt_a:
    type: "agent"
    role: "hunter_a"
    on_success: "context_b"
    on_fail: "failed"
    timeout: "15m"
    prompt: &hunt |
      Your input is the pull request's diff, masked by Vornik: a value shown
      as [REDACTED:<type>] was secret-shaped and is not available to you. The
      diff, the readability counts and the CI outcomes are all you have; you
      have no access to the repository's other files.

      Hunt for problems the diff introduces: correctness (walk every changed
      file), security and robustness, missing tests for new behaviour, and a
      CI run that failed on this head. Each finding cites the file and the
      line of an ADDED line (a "+" line, numbered in the new file) and the
      concrete failure it causes. A finding on any other line is dropped
      when the review is posted.

      When the input says some files are not in this diff text, read every
      part it names with forge_diff_part before you answer, and list the
      parts you read in parts_read. A file is counted as reviewed only when
      every part holding it is listed.

      Answer with at most 16 KiB of text, ending with
      {"findings":[{"file":"...","line":N,"severity":"blocker|major|minor|nit","title":"...","detail":"..."}],"parts_read":[2,3]}
  context_b:
    type: "system"
    handler: "context.gather"
    gather: ["fetch_diff", "readability", "fetch_ci"]
    on_success: "hunt_b"
    on_fail: "failed"
    timeout: "1m"
  hunt_b:
    type: "agent"
    role: "hunter_b"
    on_success: "context_t"
    on_fail: "failed"
    timeout: "15m"
    prompt: *hunt
  context_t:
    type: "system"
    handler: "context.gather"
    gather: ["hunt_a", "hunt_b"]
    on_success: "triage"
    on_fail: "failed"
    timeout: "1m"
  triage:
    type: "agent"
    role: "triager"
    on_success: "context_j"
    on_fail: "failed"
    timeout: "10m"
    prompt: |
      Your input is two independent lists of findings on one pull request.
      Merge them: one entry per distinct problem, keeping the clearest
      file, line and detail; rank them by severity; say which hunter found
      each. Do not add findings of your own. End with the merged list in
      the same {"findings":[...]} form.
  context_j:
    type: "system"
    handler: "context.gather"
    gather: ["fetch_diff", "triage"]
    on_success: "adjudicate"
    on_fail: "failed"
    timeout: "1m"
  adjudicate:
    type: "agent"
    role: "adjudicator"
    on_success: "post"
    on_fail: "failed"
    timeout: "15m"
    prompt: |
      Your input is the masked diff and the triaged findings. For each
      finding, confirm it against the diff: keep it only if the cited line
      is an added line and the failure follows from the code shown. Drop
      what you cannot confirm. A finding on a file that is not in this diff
      text: read its part with forge_diff_part to confirm it. Then write a
      short summary of the change and its risk. Vornik counts which of the
      files left out of this text the hunters read; list in not_read only a
      file you could not assess. End with
      {"review":{"approved":true|false,"summary":"...","findings":[...],"not_read":["..."]}}
      approved is true only with no blocker or major finding kept.
  post:
    type: "system"
    handler: "forge.post_review"
    gating_reviews: false
    ground_findings: true
    check_run: true
    on_success: "complete"
    on_fail: "failed"
    timeout: "5m"
terminals:
  complete:
    status: "COMPLETED"
    message: "Change-request review posted, with a check run."
  nothing_to_review:
    status: "COMPLETED"
    message: "No new commits since the last review; no reviewer was run."
  failed:
    status: "FAILED"
    message: "Change-request review failed."
---

# GitHub App PR Review

The review an App-backed project runs (GitHub #801; github-app-channel
design, "A review good by default"). `forge.fetch_diff` masks every
secret-shaped value and proves the mask by a rescan, failing closed; the
review roles read only that masked diff. Two hunters look independently,
a triager merges their lists and an adjudicator keeps what the diff
confirms. `forge.post_review` drops any finding not on an added line, scans
the rest, posts the review as a comment and a neutral check run named
"Vornik review". A diff larger than 192 KiB is read code first; the files
past that are named as not reviewed.
