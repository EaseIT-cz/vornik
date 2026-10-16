---
sources:
    - path: internal/forge/forge.go
      sha256: 8fe94fe7e928d7d2e18eaabf8d0b149db4210746cebe491ccb8202ab8e221c88
    - path: internal/forge/github/github.go
      sha256: 44e8cf74915ea21fcbb960f86e9c01e1a492578e4af2d973695ebd37bca39c9e
    - path: internal/forge/issues.go
      sha256: 26e007910abebbabf41b907c81741ae7d53680e9675bce1748785129f35e60c4
    - path: internal/forge/github/issues.go
      sha256: 1bf427bc8c7242fe6c878dfaaac875a145c94292338c861649a569d25fda7fc2
    - path: internal/api/forge_issues.go
      sha256: 4eb63a71b8ef21adf60cbf32b343e8fc7e5fdf27237f4cfa80dc2e62f7d2f311
    - path: internal/registry/project_forge_issues.go
      sha256: 029c58c95573956387d132eafa8f27bba404d90d1c6e4b89614f68285b7c60d9
---
# Forge — GitHub automation

!!! note "Community Edition"

    Included in the free, open-source **Community Edition**. See [Editions](../editions.md).


Forge connects a vornik project to a GitHub repository so that **labelling an
issue opens a pull request, and opening a pull request gets a review** — done by
your swarm, on your infrastructure. The defining property is the credential
model: **the daemon owns the push, and the agent never holds a credential.**

## What it does

Forge classifies inbound GitHub events deterministically (no LLM router decides
what to do) and drives two main flows:

- **Issue labelled → pull request.** Label an issue `bug` and the swarm fixes it
  on a `fix/issue-<n>` branch; label it `enhancement` / `feature` /
  `feature-request` and it takes a first cut on a `feat/issue-<n>` branch. Either
  way the daemon opens the PR (titled `Fix #<n>: …` or `Implement #<n>: …`,
  closing the issue). **Every automated PR is opened as a draft** — a human marks
  it ready.
- **Pull request opened → review.** When a PR is opened, reopened, marked ready
  for review, or **updated with new commits**, the daemon fetches the diff, a
  reviewer agent reads it, and
  the daemon posts the review back through GitHub's review API.

A bare issue with no label is ignored — only a *labelled* issue is actionable.

**Draft pull requests are not reviewed automatically.** A draft is work in
progress; marking it ready for review is what starts the review. An explicit
request (below) still reviews a draft — asking is consent.

### Re-review: new commits, and asking for one

A pull request is reviewed again when you push to it. Two things keep that from
becoming noise:

- **A burst of pushes produces one review, not one per push.** While a review is
  running, further pushes update what it will look at rather than queueing more
  reviews.
- **A re-review looks at what changed** since the last review it posted, not the
  whole pull request again — so it does not repeat findings you have already
  read. If the baseline is unavailable for any reason, including a force-push
  that rewrote it, the review falls back to the complete diff rather than
  reviewing less than it should.

You can also ask directly, by mentioning the bot in a comment on the pull
request. **`<bot>` is your GitHub App's own handle** — its slug, e.g.
`@vornik-companion`. It is configurable — `forge.mention_handle` on the project,
which applies to both ingresses — and it should be a handle you own: a handle that happens to be someone else's account
notifies a stranger every time anyone addresses the bot.

| Comment | Effect |
|---|---|
| `@<bot> review` | review now, covering what changed since the last one |
| `@<bot> full review` | review the whole pull request, ignoring the baseline |
| `@<bot> pause` | stop reviewing this PR automatically; commands still work |
| `@<bot> resume` | resume automatic review of this PR |

**Only people with standing in the repository can run these** — its owner,
organisation members, and invited collaborators. This is enforced on every
ingress: the check lives with the shared review rules, so both the GitHub App
channel and the generic webhook path apply the same one. A review is model spend, so on
a public repository an ungated command would let any passer-by spend your
budget. Note that a *contributor* — someone who has had a pull request merged —
does not qualify: that describes the past, not permission to trigger spend at
will. Anything the forge does not vouch for is refused, so an unfamiliar payload
fails closed.

A review posted in answer to a comment **quotes the request it is answering**, so
the review says what it was asked. Without that a reader sees a verdict with no
idea which question produced it — and comments can be edited or deleted, taking
the context with them, while the review itself stays.

A mention that isn't one of these commands gets a conversational reply, exactly
as before.

To turn automatic review-on-push off for a whole project rather than one PR, set
`forge.auto_review_on_push: false`. Automatic review of drafts can be turned on
with `forge.review_draft_prs: true`. Both apply on **every ingress** — the
GitHub App channel and the generic `webhooks.sources` relay alike.

```yaml
forge:
  auto_review_on_push: false   # first review only; pushes stay quiet
  review_draft_prs: true       # review drafts too (default: wait for ready_for_review)
```

The push switch suppresses the push trigger *alone*: a pull request being
opened, reopened or marked ready for review is still reviewed, and an explicit
`@<bot> review` always is — asking is asking. If you set these under
`github_app:` before 2026.9.2 they still work, and they now apply to both
ingresses rather than only the App channel; the `forge:` spelling wins where
both are present.

## Backlog-origin pull requests

The two flows above are inbound-event-driven: an issue label or an opened PR
tells Forge what to do and which repo to do it in. A project running
[backlog autonomy](../guides/autonomy.md#backlog-autonomy-and-agent-deposits)
opens draft PRs a third way, with no inbound event at all: an item consumed
from the project's backlog file is dispatched into the `backlog-item`
workflow (analyze → implement, TDD → test → review → publish), and its
`publish` step is the same deterministic `forge.open_change_request` handler
the issue flow uses — pushing the branch and opening the PR daemon-side, no
agent involved.

A few things differ from the issue-driven flow because there's no issue to
key off of:

- **Branch naming.** No `fix/issue-<n>` / `feat/issue-<n>` — the branch is
  named from the backlog item itself (`backlog/<slug>`).
- **No issue to close.** The PR title and body describe the change; there's
  no `Fix #<n>` linkage because no issue triggered it.
- **Always draft, same as the issue flow.** Nothing about backlog origin
  changes that — every automated PR is opened as a draft regardless of how it
  was triggered.
- **The outbound repo comes from config, not the event.** With no webhook
  payload naming a repository, backlog-origin publishing needs `github.repo`
  (`owner/repo`) set on the project so the daemon knows where to push.

Once opened, a backlog-origin PR goes through the exact same **pull request
opened → review** path as any other PR — it isn't special-cased for review,
only for how it came to exist.

## The credential model

This is the part that matters for security review:

- The GitHub App private key lives **only on the daemon's filesystem**. It is
  read once at startup and never leaves the daemon process.
- Outbound calls use a **short-lived installation token** the daemon mints on
  demand and caches in memory for a few minutes.
- The **push** hands that token to `git` through an in-process credential header
  — never on the command line (so it can't be seen in the process list), never
  in the remote URL, and never written to on-disk git config. Pushes are
  non-forced and idempotent: an up-to-date branch is a no-op, a diverged branch
  is rejected.
- The agent only edits files in its workspace. The project's `.git` directory is
  mounted **read-only** into the agent container, so the agent *cannot* commit,
  push, or run `gh` — the entire mutation path is deterministic and daemon-side.

The result: a compromised or confused agent has no path to your repository's
write credentials, because it never has them.

## Setting it up

1. **Create a GitHub App** and download its private key (PEM). Note the App ID.
2. **Grant permissions:** Contents (read & write — verified at daemon startup),
   Issues (read & write), Pull requests (read & write), and Metadata (read).
   Add **Actions (read)** only if you want CI outcomes — see *Reading CI
   outcomes* below. It is a separate, later grant on purpose: adding a
   permission to an existing App requires each repository owner to **re-accept
   the installation**, so Forge does not ask for it unless you turn the feature
   on.
3. **Subscribe to webhook events:** `issues`, `pull_request`, and (for PR review
   commands and `@<bot>` mention replies) `issue_comment`. Add `workflow_run`
   for CI outcomes.
4. **Point the webhook** at your daemon's signed webhook endpoint,
   `POST /api/v1/webhooks/{projectId}/{source}`, and set a webhook secret. Forge
   verifies every delivery's `X-Hub-Signature-256` HMAC; the secret is supplied
   to the daemon by environment-variable *name*, never written in plain YAML.
5. **Install the App** on your repositories and record the installation ID.
6. **Configure the project.** Under the project's `forge` block:

   ```yaml
   forge:
     provider: github
     github:
       app_id: "123456"
       installation_id: "78901234"
       private_key_path: /etc/vornik/secrets/forge-app.pem
       # api_base_url: https://github.example.com/api/v3   # GitHub Enterprise only
   ```

   Webhook sources route by the classified job: change-request (PR) events go to
   the review workflow, everything else to the issue workflow. Set
   `require_forge_event: true` on the source to drop deliveries that aren't
   actionable.

   **Repository scope.** A webhook source acts only on events for the
   project's own repositories: by default, the repositories named by
   `forge.github.repo`, `github.repo`, `github_app.repo_allowlist` and
   `forge.issues.repos` (compared case-insensitively against the
   delivery's `repository.full_name`). This matters when one App is
   installed on a whole organisation: every repository's events reach the
   App's webhook, and without a scope another repository's labelled issue
   would become a task in this project. Name the scope on the source to
   make it explicit or different:

   ```yaml
   webhooks:
     sources:
       - name: github
         repos: [acme/service]   # or ["*"] to accept every repository
   ```

   A delivery for a repository outside the scope is acknowledged, recorded
   as `filtered` with the code `repo_not_in_scope` (see
   `GET /api/v1/projects/{projectId}/webhooks/events`), logged as a warning, and counted in
   `vornik_webhook_repo_scope_total{outcome="dropped"}`; the same counter's
   `allowed`, `no_repository` and `unrestricted` outcomes count everything
   else the check examined. Events with no repository (installation, ping)
   are not judged by the scope; `filter` and `require_forge_event` still
   apply to them. If a project has a forge integration but names no
   repository anywhere, the daemon warns at startup that its source accepts
   every repository.

   *Upgrading:* a source on a project that names a repository but
   deliberately receives events for other repositories needs `repos` set
   (or `["*"]`) after this change.

At startup the daemon verifies it can actually push for each Forge-configured
project and warns you if it can't — so a misconfigured key surfaces immediately,
not on the first issue.

## Review behaviour

The reviewer works through the diff in a fixed order — intent, a design
challenge (is this the right approach, and what's the alternative?), a
correctness pass that cites `file:line` for every finding, test adequacy,
security and robustness, and an explicit "what would break this" attempt —
before reaching a severity-ranked verdict. A problem the reviewer notices
that the diff didn't introduce doesn't block the PR: it gets recorded with
the `backlog_deposit` tool (see
[Backlog autonomy and agent deposits](../guides/autonomy.md#backlog-autonomy-and-agent-deposits))
instead, so the verdict stays scoped to what's actually in the diff.

By default a posted review is a non-gating comment, even when the reviewer
approves. Set `gating_reviews: true` on the review step to turn the reviewer's
verdict into a real GitHub APPROVE / REQUEST_CHANGES. (A review that would
approve the App's *own* PR is automatically downgraded to a comment, since GitHub
won't let an author approve their own pull request.)

## When the target is gone

A review needs the pull request it was asked about. If the forge says that PR is
not there — it was deleted, the number never existed, the repository was renamed
— **the task fails immediately rather than retrying.**

That is deliberate. Retrying cannot make a missing pull request exist, and each
attempt spends real forge API rate limit that your other reviews need. Before
this, a review aimed at a non-existent PR would quietly burn its whole retry
budget in a few seconds and then tell you to "try again".

The failure is reported as `FORGE_TARGET_UNAVAILABLE`, and
`vornikctl playbook show FORGE_TARGET_UNAVAILABLE` says what to check. In
practice it is one of two things:

- **The target really is gone or was never there** — the invocation pointed at
  the wrong number or a renamed repo. Fix the caller; nothing is wrong with
  Vornik.
- **The target exists but Vornik can no longer see it** — the App was uninstalled
  from that repository, or its permissions no longer cover it.

Rate limits and GitHub outages are *not* treated this way: a 429 or a 5xx is
temporary, so those keep their full retry budget exactly as before.

## Conversational replies

A separate GitHub App *channel* handles `@<bot>` mentions in issue comments:
an allowlisted user mentioning the bot routes through vornik and gets a reply
posted as an issue comment. This is the chat surface; the deterministic flows
above are the automation surface.

## Reading CI outcomes

Forge can read what your CI concluded and put it in front of the review, so a
reviewer is not looking at a diff while the pipeline that tested it is invisible.

**What it records.** When a workflow run completes, Forge stores its conclusion,
its per-job conclusions and its timing, against the **commit** the run tested.
That happens for every run it watches — including runs with no pull request,
such as a push to your default branch.

**What it does with it.**

- A **failed** run triggers a review, coalesced through the same machinery that
  collapses a push burst, so six workflows finishing together produce one review
  rather than six.
- A **failed** run whose commit has *already* been reviewed gets a short factual
  **comment** instead — the workflow, the conclusion, the failing jobs and a link
  to the run. This is the common case, because CI usually finishes just after the
  review of the push that triggered it. No model writes that comment, so there is
  nothing in it to be wrong: re-reviewing a commit with no new code is how a bot
  ends up describing the wrong pull request. Turn it off with
  `ci.comment_on_failure: false`.
- A **green** run is recorded quietly. If you set `ci.success_workflow_id` it
  also starts that workflow once per run — useful for depositing a merged build's
  output somewhere, and off unless you configure it. You can also name the
  review workflow here, so a *completed* run is what triggers a review rather
  than the push that started it: a workflow that needs a pull request runs only
  for runs that have one, and a green push to your default branch is recorded
  without starting anything.
- Any review that runs later reads the outcomes for the commit it is reviewing.

**Getting the actual output in.** Conclusions come free. Content — a
`terraform plan`, a test report, a scan result — has to be **uploaded by your
pipeline as an artifact**, and you name it in `ci.artifact_name`. Forge reads
artifacts, never job logs: a log has no format contract and is where secrets get
printed, whereas an upload is a deliberate act by whoever wrote the pipeline.
There is no tool-specific parsing anywhere — Terraform is simply a repository
that uploads a plan, and the same setup works for anything else.

Uploaded content is treated as **untrusted**: anyone who can push a branch can
make CI print anything, so it reaches a model marked as data rather than
instructions, and it is size-capped with any truncation stated rather than
silent.

```yaml
forge:
  ci:
    enabled: false              # master switch; false is today's behaviour exactly
    artifact_name: ""           # opt in to content; empty records conclusions only
    max_artifact_bytes: 1048576 # refuse a download above this
    max_excerpt_bytes: 65536    # store at most this, and say when it truncated
    review_on_failure: true     # a failed run triggers a coalesced review
    success_workflow_id: ""     # a green run fires this workflow; empty is off
    comment_on_failure: true    # say so when a failure cannot produce a review
    block_approval_on_failure: true # withhold an APPROVE while a run is red
    workflow_paths: []          # record only these workflow files; empty means all
    trigger_workflow_paths: []  # of those, only these may TRIGGER; empty means all
```

**A red pipeline withholds the approval.** With `block_approval_on_failure`
(default on), `forge.post_review` will not submit an **APPROVE** while a run it
recorded for the reviewed commit has failed. The review is posted as an ordinary
comment instead, led by a line naming the failing workflow — so the reasoning
still reaches the pull request, but it cannot satisfy branch protection on a
commit its own pipeline rejected.

This is a check on the recorded outcome, not on the reviewer's opinion: it does
not read the review text and cannot be argued with. A `REQUEST_CHANGES` verdict
is unaffected, and a commit with no recorded runs is not treated as a failure —
"nothing ran" and "CI failed" are different facts. Set it to `false` to restore
the previous behaviour.

**Why two path lists.** `workflow_paths` decides what is *recorded*;
`trigger_workflow_paths` decides which of those recorded runs may *start* a
review. On a repository where several workflows run per pull request, one list
cannot express what you want:

- record everything and let everything trigger, and a green push fires the
  success workflow once per run — several reviews for one push;
- record only one workflow, and the review sees only that one. It can then
  report a green pipeline while another workflow is red, which is worse than
  having no CI context at all.

So record everything and nominate one gate:

```yaml
forge:
  ci:
    workflow_paths: []                                   # record all of them
    trigger_workflow_paths:
      - ".github/workflows/integration.yml"              # …this one starts the review
```

Pick the workflow that finishes last, or the one you consider authoritative:
the review renders the outcomes recorded *when it runs*, so a gate that
finishes early shows a partial picture. It never invents one — a run that has
not completed is reported as not completed, never as passing.

Naming a path in `trigger_workflow_paths` that a non-empty `workflow_paths`
excludes is rejected at startup: that run would never be ingested, so the
trigger could never fire.

If your events arrive on the generic signed webhook rather than the App
channel, give the source a `ci_workflow_id` as well — a CI run is not a change
request, so without one it would fall to the source's default (issue) workflow:

```yaml
webhooks:
  sources:
    - name: github
      change_request_workflow_id: github-review
      ci_workflow_id: github-review   # or a workflow of its own
```

**One limitation worth knowing before you rely on it.** GitHub does not tell
Forge which pull request a run belongs to when the PR comes from a **fork** —
the event arrives with no pull request attached. Such a run is still recorded
and still visible to a review of that commit, but a CI failure on a fork PR will
not *start* a review on its own.

## Notes and limits

- Automated PRs are always **drafts** — promotion to ready is a human action.
- Re-running a flow is idempotent: it matches the deterministic branch name and
  only short-circuits on an *open* PR (a closed or merged one yields a fresh PR).
- Provider support is GitHub today; the provider interface is built to be
  provider-neutral. Per-installation rate-limit buckets and PR inline
  (per-line) review comments are not yet implemented — reviews are posted at the
  PR level.

## Issue triage through the App

Task agents can inspect issues through `forge_list_issues`, `forge_get_issue`,
and `forge_list_labels`. The daemon calls GitHub with the App's installation
token; the agent receives bounded, marked untrusted text. Pull requests are
excluded. The calling role must explicitly list each tool in `allowedTools`.

Start with a report that proposes labels and duplicate references. With a
configured `forge:` block, scope reads to your configured repo:

```yaml
forge:
  provider: github
  github:
    app_id: 123456
    installation_id: 78901234
    private_key_path: /etc/vornik/secrets/forge-app.pem
    repo: acme/service
  issues:
    repos: [acme/service]       # additional explicitly allowed repos
    labels_enabled: false      # report first; no label changes
    allowed_labels: [bug, enhancement, P1, P2, P3]
    close_enabled: false
    writes_without_approval: false  # see "Issue writes are unattended" below
```

Configured `forge.github.repo`, legacy `github.repo`, and
`github_app.repo_allowlist` also grant issue repo scope. An empty set grants
nothing; clone remotes and task-provided repo names never expand access.

After reviewing reports, enable `labels_enabled` and grant the writer role
`forge_add_labels` and `forge_set_labels`. Every requested label must match
`allowed_labels` exactly. Adding labels preserves all existing labels. Setting
labels replaces the managed allowed labels and retains other labels observed
at the time of the read. A human or another replica changing labels during the
read/write interval can still race GitHub's replacement operation; coordinate
label writers when retaining concurrently edited labels matters.

Closing duplicates requires a separate `close_enabled: true` and the role tool
`forge_close_issue`. Supply the source issue number, a different positive
`duplicate_of` number in the same allowed repo, and an explanation. The daemon
verifies both are issues, posts an explanation marked as automated triage and
ending with the AI-disclosure notice (`ai_disclosure.publication_text`), and
closes the source as not planned. Retries recognize only markers posted through
this same GitHub App. Closing refuses when the full comment set cannot be
checked within the 100-comment cap. Both write opt-ins default to false.

### Issue writes are unattended

Anyone who can open an issue or comment on it writes the text the agent
reads. Vornik marks that text as untrusted, but the model still reads it, and
crafted text can steer the agent ("this is a duplicate of #1, close it",
"remove the P1 label"). Vornik does not yet ask you to approve each label
change or close on your phone. So label and close writes are refused until
you also set:

```yaml
forge:
  issues:
    writes_without_approval: true
```

This states that you accept writes decided by an agent reading untrusted
text, with no human in the loop. It is set only in the project file; the
project form keeps it but does not show it, and an assistant connected
through the admin tools cannot set it. Reads never need it. If a project has
`labels_enabled` or `close_enabled` without it, the daemon logs a warning at
boot and refuses each write with a reason that names the key.

**A close stays in one repo for the whole task.** A close posts the
agent's explanation as a comment on the issue, and issue reads can span
every allowed repo, private ones included. So `forge_close_issue` on a repo
is refused if the same task has called any `forge_*` tool on a different
repo, or one with no repo, whether that call succeeded or not. So text in
an issue that makes the agent try another repo stops that task from closing
anything; it can block closes but never widen them. To triage several repos
and close duplicates, run one task per repo. This rule covers the forge
tools only. It does not see text that reaches the agent another way
(another task's output, memory, `query_api` or an MCP server), so a close
could still quote that text: do not give a closing role those routes to
private repos. `writes_without_approval` is the real control. Label writes
are not restricted by repo; their values come from `allowed_labels` and
carry no free text.

**Duplicate closing requires one active API instance.** Concurrent replicas
can post duplicate explanations because GitHub comment creation has no
idempotency key. Route closing requests to one instance in an HA deployment.
Within that instance, close calls are serialized and retries/restarts reconcile
the deterministic App-authored comment marker. This does not claim distributed
exactly-once posting.

Issue lists default to page 1 with 30 entries and accept at most page 10 and
100 entries per page. Labels scan at most ten pages of 100. Issue and comment
text is clipped to 16 KiB on a UTF-8 boundary; comments are capped at 100.
Results report truncation and next-page information. Raw upstream pages over
1 MiB are rejected instead of returning broken or silently partial JSON.

Each write requires a durable pending tool-audit record before contacting
GitHub, followed by a success or error record. Missing audit storage blocks the
write. Once GitHub has accepted a write, the reply says it was applied, even if
the daemon then cannot encode the result or write the result record; in the
latter case the pending record is kept and the daemon logs the issue at error
level so you can reconcile it. The agent is told not to repeat the write. Audit metadata includes task, project, execution, operation, repo,
issue, labels and an explanation hash; it does not store the explanation text.
The App needs Issues read for reports and Issues write for changes. Boot checks
warn independently of Contents permissions; grant the required permission and
reaccept the installation if the warning appears.
