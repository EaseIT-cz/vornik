---
sources:
    - path: docs/release-notes
      sha256: bacfcc077d08762e0cfb46ceddbda561375d845b591c67e4605d04cdf281813a
---
# Release Notes

A curated, reverse-chronological summary of user-facing changes in vornik.
Each entry highlights what changed for you as an operator — new features,
behavior changes, and notable fixes. Internal-only changes are omitted.

!!! tip "Upgrading"
    New configuration keys are additive and default to the previous behavior,
    so upgrades generally require no config changes. Always take a backup
    before upgrading. A few releases ask you to restart the daemon to pick up
    new behavior; those are called out below.

---

## 2026.11.8

!!! warning "Upgrade requirements"
    - **Tailnet addresses are now private**. Vornik treats
      carrier-grade NAT, 100.64.0.0/10, where Tailscale assigns addresses,
      like a LAN range. An agent project's API or MCP server on a tailnet
      peer stops working until its host is on
      `agent_admin.private_api_hosts`, and its URL must be https. Before
      upgrading, add to that list the host of each agent connection on a
      tailnet (one whose host is a `*.ts.net` name or resolves into
      100.64.0.0/10). For a connection whose URL is a 100.x address, list
      the address. After
      upgrading, `vornikctl doctor` reports `agent_connections_cgnat` as an
      ERROR naming each such connection still unlisted; list its host or
      remove the connection.
    - **Other daemon clients lose tailnet reach with no list to restore
      it:** an OIDC issuer, a broker URL or a Bedrock image URL on a
      100.64.0.0/10 address is refused. A hub probe target there needs
      `integrations.allowed_hosts`. Self-hosted model endpoints keep their
      tailnet reach, and a model on a tailnet peer still needs its approval
      as a remote one. On a managed instance, a customer's own model
      endpoint on that range is refused, as a LAN one already was.
    - **An approval waiting at the upgrade can be refused once**. An agent's request to connect an API or MCP server now
      records whether `agent_admin.private_api_hosts` gave it intranet
      reach, and the apply refuses it if that changed after filing. A
      request filed before the upgrade records none, so if its host is on
      the list it is refused when approved, with "the operator's list of
      intranet hosts changed after you were asked"; the agent asks again
      and the new approval names the host.
    - **A quickstart install keeps its old service unit until you replace
      it.** The shipped unit is now `Type=notify`, so systemd waits for the
      daemon to report ready and a boot that hangs is restarted after 300 s
      instead of showing as running forever. The updater never rewrites
      your unit: an install made by an older quickstart keeps
      `Type=simple`, and the update warns and names the command. To get the
      bounded start, install the shipped unit:
      `install -m 0644 deployments/podman/systemd/vornik.service ~/.config/systemd/user/vornik.service && systemctl --user daemon-reload`
      (from your checkout).

### New features

- **An agent's jobs can share a memory, and a job can start the next one**.
  - **Before:** each project an agent such as Hermes built for a job kept
    its own memory, which nothing else could read. The venue-scouting job's
    results never reached the enrichment job unless Hermes copied them by
    hand.
  - **Shared memory, by opt-in:** a project created or updated with
    `shared_memory` reads and writes the namespace's shared memory
    (`<namespace>--home`); you approve joining on your phone. Every other
    project's memory stays sealed, so a job that reads private data never
    feeds one that reaches the web.
  - **Handover:** a workflow can declare `hand_over` in `define_workflow`.
    When it completes, it starts another workflow of the namespace and hands
    it chosen output files as document inputs. You approve the hand-over on
    your phone. A loop is refused; a chain stops after three hand-overs from
    one delegation. Each attempt is counted on
    `vornik_agent_handovers_total`.
  - **For the agent:** `describe_installation` now lists the five ways to
    lay out projects (`patterns`), and the admin guidance and tool
    descriptions say the same.
  - The agent's own memory is unchanged: it stays where its memory key is.
    Existing job memory stays in each project; to keep it after a project
    joins the shared memory, move it with `vornikctl memory reassign`.

- **Start the daemon held, then release it**. A
  `start-held` marker file in the data directory makes the next start
  serve its listeners without starting any work: no scheduler, autonomy,
  channel polling or task leases. `/readyz` answers 503 `"status":"held"`
  with its checks, so an upgrade can verify the new version before it
  touches anything. `vornikctl daemon release` reloads the config files,
  removes the marker and starts work; a release that fails stays held or
  exits, and the marker decides. A hold past its deadline (30 minutes by
  default) stays held, reports `overdue` and alerts once. While held,
  webhooks get 503. After a release the daemon redelivers the GitHub App
  deliveries that failed while it was held, up to 200, and
  `GET /api/v1/daemon/hold` reports how many it could not. On a managed
  instance, `managed-upgrade` now starts the new version held, checks
  `/readyz` and doctor, and releases it; a held version that fails those
  checks is rolled back before it has done any work.

- **Intranet hosts per project, by address, port or pinned addresses**. An `agent_admin.private_api_hosts` item may now be an IP
  address (`10.0.0.5`, a 100.x tailnet address, IPv6) or carry a port
  (`nas.lan:8443`), which limits it to that port. An item may also be a
  mapping with `host`, `addresses` (the addresses or CIDRs the host may
  resolve to: anything else is refused, public or private) and `projects`
  (the agent projects it serves). Plain names keep serving every agent
  project; name projects when more than one exists. The approval names
  the host and any pins and records the grant; a restart that changes
  the grant before the approval applies makes the apply refuse.
  `describe_installation` lists the hosts an agent's projects may reach,
  and `vornikctl doctor` (`agent_private_hosts_unused`) warns about an
  entry no approved connection uses. A listed host's private reach now
  holds on the URL's own port only, and its client follows redirects to
  its own origin only: a server that redirected to another port or to
  plain http on itself is refused there.

- **Shipped roles name a model tier, not a model**. The
  swarms and project templates Vornik ships no longer pin Bedrock or Ollama
  models. Each role names a tier instead (`complex`, `coding`, `standard`,
  `trivial` or `vision`), and every tier runs on your setup model until you
  map it. Map tiers in `runtime.agent_llm.tiers`, or apply a shipped
  provider profile with `vornikctl models profile apply bedrock` or
  `ollama-cloud`. `vornikctl models tiers` shows what each role runs on.
  Setup names the profile that matches your endpoint, and a managed
  instance sets tiers from its catalogue through `setup/managed`
  (operations API 1.6.0). An installed instance keeps its own swarm files,
  models included. Guide: [Choose the models your roles run on](../guides/model-tiers.md).

- **opencode connects to Vornik in one command**. `vornikctl
  companion connect opencode` checks a companion key and stores it in a file
  only you can read. It then writes into opencode's config directory:
  - an MCP entry that runs Vornik's bridge (no opencode config holds the key);
  - a plugin that adds Vornik's working rules and a session digest to the
    system prompt;
  - the companion skills, as `skills/vornik-*`.
  `doctor --client opencode` checks the install, and `disconnect opencode`
  removes it. Tested live with opencode 1.14.37. The plugin uses an opencode
  hook marked experimental, and `doctor` warns when your opencode version
  differs from the tested one. Guide: [opencode](../guides/opencode.md).
- **The App review measures itself**. A canary run gives the
  review a generated diff of planted fake secrets and decoys, then scores
  what it caught instead of posting. It posts nothing and calls GitHub for
  nothing. Schedule it weekly with autonomy's cron mode, and set floors after
  four measured runs; with no floors set it only reports. See
  [Forge](../features/forge.md#measuring-the-review-the-canary).
- **PR reviews read scanner results as SARIF**. When your
  pipeline uploads a `.sarif` file as its CI artifact, a review shows one
  line per finding: level, rule, file, line and message. The App review's
  triager merges these with the reviewers' findings, and a finding still
  posts only on a line the pull request adds. Code snippets and fingerprints,
  which can hold the secret a scanner found, are never shown.
- **Cursor connects to Vornik in one command**. `vornikctl
  companion connect cursor` checks a companion key, stores it in a file only
  you can read, and installs a Cursor plugin: an MCP entry that runs
  Vornik's bridge (no Cursor config holds the key), an always-applied rule
  with Vornik's working rules, the companion skills, and a session hook that
  adds your delegations and recent memory to a new chat. Tested against
  Vornik's companion endpoint as installed; not yet in a live Cursor
  session. Guide: [Use Vornik from Cursor](../guides/cursor.md).
- **Autonomy can observe before it acts**. With
  `autonomy.observe: true`, a project's autonomy loop runs as usual, records
  what it would have started (`WOULD_START`, with the prompt) and starts
  nothing. The project page counts the last 7 days' proposals and shows the
  latest five; `vornikctl autonomy evaluations --outcome WOULD_START` lists
  them all. New projects start in observe mode; existing projects keep
  acting, because an absent key means `false`. Guide:
  [Observe first](../guides/autonomy.md#observe-first).
- **Each agent role can set how much its model reasons**.
  `reasoning: off | low | medium | high` on a swarm role, or on a model's
  `runtime.agent_llm.model_limits` entry, is sent to the provider in its
  own form. A model that refuses the setting is retried once without it;
  a Bedrock model that refuses only `off` (GLM 5.3) gets `low` instead.
  `vornik_chat_reasoning_requests_total{model, requested, applied}`
  counts the calls on the OpenAI-compatible routes. Measured on Ollama
  Cloud `glm-5.3-flash`: `off` took a small answer from 41 to 6 completion
  tokens.
- **Each companion key has a request limit**. A companion key
  may make 5 requests a second, with bursts of up to 50; past that the
  companion endpoint answers 429 with `Retry-After`. A leaked key or a
  client stuck in a loop no longer calls as fast as the daemon answers.
  Change or turn off the default with `api.rate_limit.companion_key`
  (`rps`, `burst`, `off`). A key whose own row sets a limit keeps it.
- **A GitHub Actions job can call the companion without a stored secret**. Bind a companion key to one workflow with `vornikctl
  companion trust-github`; the job then authenticates with the ID token
  GitHub issues to its run. Every claim must match exactly (repository id
  and name, ref, workflow file, event, audience), and a run on another
  branch, a fork or a pull request gets a 401. `vornikctl companion grant
  --oidc-only` mints a key whose secret is never printed.
- **Secret findings in stored artifacts are counted**.
  `vornik_artifact_secret_findings_total{type, outcome}` counts each one as
  `rewritten`, `stored` (detect mode) or `dropped` (a heuristic rule, which
  never rewrites an artifact), so you can see how often each rule fires.
- **Any agent that speaks MCP can use Vornik**. `vornikctl
  companion connect generic --print` checks a companion key, stores it in a
  file only you can read, and prints the MCP entry to paste, never the key.
  `vornikctl companion instructions --write AGENTS.md` gives the agent
  Vornik's working rules. Measured on 2026-11-20: Gemini CLI and opencode
  each stored, recalled and refuted a note and delegated a task, in about
  100 seconds; neither gets a session digest. Grant such a key with
  `--repo-scope`: without one, an agent with no Vornik hooks stores its
  notes unscoped. Guide: [Use Vornik from any agent](../guides/any-agent.md).
- **Task, execution and lease IDs are shown short in the console**. Every page shows `T-fee9`, `X-a1b2` and `L-9c4d` instead
  of the full ID, which took a line of its own on a phone. Hover over a
  short ID, or open its link, for the full one. Instincts (`I-fee9`) and
  control-plane proposals (`P-fee9`) are short too, and an approver device
  shows `dev_…` and its last 6 characters, as on the phone.
  IDs you chose (projects, workflows, swarms), a detail page's own ID, and
  data subject request and incident IDs stay whole.
- **`setup/status` reports a restart generation**.
  `pending_generation` counts the settings saved that need a restart, and
  `loaded_generation` the count the running process started with. When
  they differ, a restart is still owed, even if one already happened while
  the change was being saved. Operations API 1.5.0.

### Fixes

- **A re-run of an incomplete erasure, rectification or restriction keeps
  every run's report.** A re-run used to overwrite the first run's stored
  report, so after it nothing recorded what the first run had erased or
  corrected. The stored report now keeps the latest run at the top, plus
  the run number and every earlier run's report, oldest first. A report
  stored before this release reads as run 1.

- **`vornikctl memory recheck-urls` never probes a private or metadata
  address.** It kept its own list of private addresses, which missed a
  cloud metadata address and several encoded forms of blocked addresses,
  so it could learn whether hosts at those addresses were alive. It now
  uses Vornik's one list of private addresses, checked before the request,
  at each redirect and at connect time.

- **An oversized OIDC key set is named in the log, not cut off silently.**
  A key set over 1 MiB used to be truncated, so every GitHub Actions token
  was refused with only a JSON parse error as the logged cause. The log
  now says the key set is over the 1 MiB cap. Tokens are still refused,
  as before; only the reason changed.

- **The skill distiller treats recalled memory as untrusted.** A step that
  only searched memory was accepted as safe, so web text that had reached
  project memory could feed a distilled skill. Such a step is now judged
  by what its searches returned: fetched web content, or anything the
  distiller cannot check, keeps it out of a skill.

- **A model is checked before it is saved.** Saving a role's model or a
  tier mapping now makes one live test call through the instance's router
  (15 s timeout). A model that does not answer is refused with the
  provider's reason and nothing is written; one that answers without
  tools is saved with a warning. On a managed instance, a model outside
  the instance's catalogue is refused everywhere it can be saved, and a
  step that resolves to one fails before its container starts.

- **The doctor reports a retired model at once.** A model that answers
  HTTP 410 used to show only through a failure rate, after enough calls,
  and the message did not say it was gone. `model_calls_live` now lists
  gone models first, with no minimum number of calls, and names each role
  that still points at one.

- **The adaptive router sees the project's workflows, and one "none" is
  not final.** The router was not shown the candidate workflows with
  their descriptions, so it could misjudge, and a first "none" failed the
  task. Its prompt now lists every candidate with its description, and a
  first "none" gets one corrective retry; a second "none" still fails as
  `ROUTE_NO_FIT`.

- **Autonomy's first evaluation after a restart waits for the leader
  election.** It used to run before the election had started, so a due
  evaluation could be refused after its model call was already paid for,
  and a replica without the lease ran it too. It now runs after the
  election, on the leader only.

- **A `run_workers: false` node runs no leader-only work.** Such a node
  used to act as if it were always the leader, so a UI node could run the
  autonomy loop and the retention sweeper beside the worker node.
  Retention now deletes only from worker nodes. Single-node installs are
  unchanged.

- **Autonomy proposals follow a retention window.** Rows recording
  autonomy's decisions were never pruned, and a proposed task's prompt
  can name a person. They now expire with the tool-audit window
  (`tool_audit_days`), and `vornikctl retention` shows them as
  `autonomy_evals`. The record of processing lists them as autonomy
  proposals.

- **The doctor's retention check states the defaults that apply.** With
  retention on and no window set, it said the sweeper prunes nothing. In
  fact the compiled defaults apply: tasks, executions and artifacts after
  60 days, LLM usage and chat audit after 90, tool audit after 180. The
  check now lists them, names the data with no default (memory chunks and
  task messages), and still warns, because the defaults were not chosen
  for your purposes.

- **Healing can repair a shared workflow, and a one-model install is told
  why it gets no candidates.** A healing request could not read a
  workflow outside the project's own files, so it answered "the file does
  not exist". It can now read and propose an edit to the workflow its
  trigger names; the edit still needs a passing trial and your
  promotion. When the config assistant and its judge share a model, every
  candidate is refused; a new doctor row, `healing_judge_family`, and a
  start-up warning now say so and name `config_assistant.judge_model`.

- **A healing replay keeps its link to the task it replays.** The link
  was written before the replay's execution existed, so it was lost on
  every replay and a warning fired each time. It is now written when the
  execution starts.

- **Instincts learn from real tool errors, and work on SQLite.** A tool
  call was counted as failed whenever its output contained a word like
  "error", so reading source code counted as a failure and tool-swap
  instincts were learned with no counter-evidence. Only an error the
  daemon observed now counts, and a swap in a failed step counts against
  the instinct; the instincts learned by the old rule are retired. On
  SQLite, budget instincts could not be stored at all; the table is
  rebuilt once to accept every kind.

- **An agent that rewrites a file with the same content is told nothing
  changed.** Such a write used to answer "wrote N bytes", and the model
  wrote again until the loop detector stopped it. It now writes nothing
  and says the file already holds exactly that content.

- **A failure caused by the output cap is no longer reported as a missing
  role.** The failure explainer matched the word "role" in the output-cap
  message's advice and showed the missing-role remedy. It now matches only
  the messages a missing role produces.

- **`vornikctl` treats a command run from automation as non-interactive.**
  Four commands mistook `/dev/null` for a terminal: `auth login` printed a
  typing hint, `agent connect --api-key-stdin` refused an empty key,
  `init project` printed prompts, and `daemon operations-credentials`
  refused to write to `/dev/null`. Each now checks for a real terminal.


- **A failed web chat reply no longer shows the error text**.
  When a reply failed before any words appeared, the chat page showed
  "Chat dispatch failed:" and the raw error, to every viewer, including
  project members who are not operators. That error can carry the model
  provider's response, its address or part of a key. The page now shows
  the same notice Telegram does ("Sorry, I could not answer: the model call
  failed. Please try again."), and the error goes to the daemon log.

- **Slack says when it could not answer**. A Slack message
  whose reply failed got no answer at all; the error went only to the
  daemon log. Slack now posts the same notice as Telegram and the web chat,
  where the answer would have gone, at most once per conversation per hour.
  It never shows the error text. A failed `/vornik` command gets the notice
  at channel level, where its answer would have posted. Notices are counted
  on `vornik_channel_failure_notices_total{channel="slack"}`.

- **A tool result the output guard cannot scan is withheld visibly**. If the scanner failed, the model already got an empty
  result instead of the unscanned text, but nothing was logged and the
  code said the opposite. The model is now told the result was withheld,
  the turn carries a `scan_failed` guard warning, and the failure is
  counted on `vornik_dispatcher_output_guard_scan_panics_total`.

- **A hand-over finds the file the first job wrote**. Vornik
  stores each output under a dated name (`enrich-20261125-a8f2.md`), and
  the hand-over looked for the plain name (`enrich.md`), so every
  hand-over was refused with "the job finished without" the file. It now
  matches the name the job wrote.

- **An assistant's hand-over request no longer fails after you approve it**. An assistant could name a hand-over file by its bare name
  (`enrich.md`), the form a workflow's output list uses. Vornik showed the
  request for approval and then refused it while applying it, so the phone
  said "Could not be applied". A bare name is now completed to
  `artifacts/out/enrich.md`, and any other path that cannot be applied is
  refused when the assistant asks, before it reaches your phone.

- **A benchmark batch that could not be verified is re-run, not skipped**. A step that failed before its container started was read
  as a run with no image identity, so its batch could not be verified and
  the pass stopped at its end. Such a step is no longer counted as a run,
  and a batch whose journal cannot be verified is re-run on resume.

- **A rejected healing candidate no longer blocks the next one**. A workflow can have one pending proposal at a time.
  Rejecting a candidate left its proposal pending, so every later
  candidate for that workflow failed with "workflow already has a pending
  proposal". Rejecting a candidate now rejects its pending proposal too.
  The benchmark autopilot rejects a candidate whose trial failed.

- **The config assistant retries once when its reply is cut off**. A reasoning model can spend its whole output budget
  thinking and never answer. The assistant now asks again once, one
  reasoning level lower (high to medium, medium to low, low to off), and
  refuses `ASSISTANT_REPLY_TRUNCATED` only if the retry is cut off too.

- **Every workflow step sees every earlier step's files**.
  A step was handed only the previous step's `artifacts/out/` files, while
  workflows built through the admin tools tell each step that earlier
  steps' files are there. In a four-step workflow, step 4 could not read
  what step 1 or step 2 wrote. Every later step now receives them; a file
  a later step rewrites is handed on in its newest version. An automatic
  resume restages the files of every step it carries over.

- **A workflow resumed after a pause still has its earlier steps' files**. After an approval, a wait for child tasks or a daemon
  restart, the next step started without the files the steps before the
  pause had written. It now finds them, as an uninterrupted run does.

- **A git command stopped at its time limit takes its child processes with
  it**. Vornik killed only `git`, so a transport or
  `index-pack` it had started could keep running, hold the command open and
  keep writing in the repository. Vornik now stops the whole process group.
  Git's own background maintenance is left alone, as before.

- **Telegram task control**:
  - `/inbox` points each task to `/status <id>`. Before, it pointed to
    `/task`, which does not exist.
  - The assistant's own retry re-queues a task the way `/retry` does:
    atomically, failed, cancelled or completed alike.
  - The assistant can no longer cancel a task that is waiting for your
    approval; answer the approval instead.
  - A database error is no longer quoted in a reply; the reply says the
    action could not be done right now.

- **An agent's API call says when the API answered with an error**.
  - **Before:** `query_api` handed a workflow role only the API's response
    body. A redirect or error with an empty body reached the role as an
    empty result, so a role reported "no results" for a call that had
    failed. `list_apis` showed only the API's host, so a role could not
    tell that the base URL already included a path such as `/res/v1`, and
    repeated it.
  - **Now:** an answer that is not a 2xx starts with `HTTP <status> from
    provider "<name>".`. A redirect also says that `query_api` does not
    follow redirects and that paths are relative to the base URL.
    `list_apis` shows an agent API's full base URL, path included.
  - **What you might see:** error answers in a role's tool results now
    carry this first line. Successful answers are unchanged.

- **A model endpoint counts as local only when Vornik can reach it** (security).
  - **Before:** an agent model endpoint whose address was an IPv6 form
    carrying a private IPv4 address (NAT64, 6to4, IPv4-compatible), or a
    link-local address, counted as local. It skipped the device approval
    a remote model needs, and every call to it was then refused.
  - **Now:** an endpoint is local only when the model guard admits it:
    loopback or a private LAN address. Those shapes now count as remote
    and need an approval; calls to them are still refused.

- **The configuration assistant reports a cut-off reply instead of "no
  change"**.
  - **Before:** a reasoning model that spent its whole output budget
    thinking returned an empty reply. The assistant read that as its
    final answer and refused with NO_CHANGE and no reason. Self-healing
    counted that as a producer failure, the same as a timeout.
  - **Now:** a reply cut off at the output-token limit is refused as
    `ASSISTANT_REPLY_TRUNCATED`, naming the tokens spent and the remedy.
    A NO_CHANGE carries the assistant's reason, or says it gave none, and
    self-healing answers it with 422 `HEALING_NO_CHANGE`: the assistant
    found nothing to fix. The new `config_assistant.reasoning` (`off`,
    `low`, `medium`, `high`) sets the reasoning level asked of the
    assistant's model on every call.

- **`vornikctl doctor` exits 0 when the only non-OK checks are SKIPPED**.
  - **Before:** a SKIPPED check, such as a feature that is not wired,
    made the command exit 1, although its summary listed no issue.
    Scripts read that as a failure.
  - **Now:** it exits 0 when every check is OK or SKIPPED, and 1 when any
    check is WARNING, ERROR or an unknown status. `--json` still exits 0
    and `--offline` still exits 1 on a failed check.

- **Every address guard refuses what the daemon's guard refuses** (security).
  - **Before:** A2A webhooks, companion push and the scraper kept their own
    lists of private addresses. Each admitted some address the daemon's
    dial guard refuses: 0.0.0.0/8, 255.255.255.255, multicast (the
    scraper's image and calendar fetches), and IPv6 forms that carry a
    private IPv4 address, such as 64:ff9b::a9fe:a9fe (169.254.169.254
    through a NAT64 translator). A companion push range opened with
    `companion_push.allowed_cidrs` also reached the cloud metadata address
    inside it (100.100.100.200, fd00:ec2::254, fd20:ce::254).
  - **Now:** each guard decides by the daemon's one list, and an
    `allowed_cidrs` range never opens a metadata address. The scraper's
    `encode_image` and `ical_events`, and the `web_fetch` check before a
    page loads, refuse the same ranges as `web_submit`: also documentation,
    benchmarking and reserved addresses, and every NAT64, 6to4 and Teredo
    address.
  - **What you might see:** a push, webhook or scraper fetch to such an
    address now fails with a refusal. No supported target uses one.

- **A config change that fails to apply no longer hides a bad reload**.
  - **Before:** when an approved change failed, Vornik reverted the files
    and reloaded, but it ignored that reload's result, and no reload had a
    time limit.
  - **Now:** every reload the control plane makes is bounded at 10 seconds.
    One that fails, times out or is stuck is logged and shown by
    `vornikctl doctor` (`control_plane_restore_reload`), with the remedy:
    fix the file and reload, or restart.
- **A reminder that cannot be delivered retries, then says so**.
  - **Before:** a reminder whose send failed stayed "firing" forever. It was
    never delivered, never retried and never reported.
  - **Now, a text reminder** is tried three times: at its time, one minute
    later, and five minutes after that. Then a one-shot reminder becomes
    `expired` with its last error, and a recurring one moves on to its next
    occurrence.
  - **A scheduled-task reminder** is not retried, because a retry could
    start the task twice.
  - **To find them:** `vornikctl doctor` lists reminders that expired
    undelivered in the last seven days (`reminders_undelivered`).

- **CI artifact content reaches a review as text, masked**.
  GitHub serves an artifact as a ZIP. Forge had stored the ZIP's bytes as
  the "excerpt", so a review saw binary instead of your plan or report. Forge
  now unpacks the archive, and it masks secret-shaped values with the
  daemon's secret detector before storing anything. With `secrets.enabled`
  off, the content is withheld and the review says so. A row stored before
  this release is shown as withheld.

- **The daemon names config keys it does not read**. At
  start it logs each key in `config.yaml` that no setting declares, and
  `vornikctl doctor` reports them (`config_undeclared_keys`). The removed
  `chat.thinking_enabled` and `chat.thinking_budget` had been accepted
  silently, so a config that set them read as "thinking off" while it did
  nothing.
- **`vornikctl restore --clean` no longer empties the database when the
  restore fails**. The clean and the replay now run in one
  transaction, so a failed restore leaves the database as it was, and the
  vector extension is kept rather than dropped.
- **A managed upgrade no longer fails at its install step**.
  `--accept-eula` no longer prompts on a non-terminal standard input, and a
  failed acceptance's last error line is now in the upgrade's reason
  instead of being discarded.
- **A Community release no longer stops when an upstream source archive
  disappears**. The release rebuilds the agent image's
  third-party source archive from the previous release's copy, checking
  every file against its pinned checksum; 2026.11.7's run had failed when
  `microsoft/onnxruntime` stopped answering.
- **An agent that writes a file with the bytes it already holds is told
  so**. `file_write` writes nothing and answers that the file
  already holds exactly this content, so the agent goes on instead of
  writing again. In the 2026-11 agent benchmark, 18 of 196 runs were
  stopped by the loop guard for repeating that write. Takes effect with
  this release's agent image.
- **A step that runs out of time says so**. Its error now
  reads "step X ran out of its 30m0s time budget (the container was
  stopped)", or names the execution's wall-clock cap, instead of "failed
  waiting for container: podman wait timed out", which read as a host
  fault. The failure class does not change.

---

## 2026.11.7

!!! warning "Upgrade requirements"
    - **GitHub tokens now reach only the repositories a project names**. Each token the daemon mints for a GitHub App is scoped
      to the project's `repo` (or a `github_app` block's `repo_allowlist`).
      A project naming a repository the App installation does not cover
      used to get an installation-wide token; it now gets none, and its
      reviews, pushes and replies fail. Before upgrading, check that every
      repository your projects name is listed under the App installation's
      *Repository access* on GitHub.
    - **`vornikctl github-token` needs `--repo` or `--unscoped`.** A script
      that calls it with neither now stops with an error. Pass
      `--repo owner/name` (repeatable) for the repositories the token is
      for.
    - **A hand-made GitHub block may not use the instance's App key**. A `github`, `forge.github` or `github_app` block whose
      key file is the key of the App created from the console is refused
      when the configuration loads. Move such a project to
      `use_registration: true`, which takes the App and installation from
      the console's registration.

### Features

- **Use Vornik from AWS DevOps Agent**. An AWS DevOps Agent
  investigation can recall your team's incident memory and the procedures
  an operator approved in Vornik. With a wider key, it can also store its
  finding and hand follow-up work to a sandboxed workflow. Register your
  Vornik's companion endpoint in the agent's console, and allowlist the
  tools your key lists. The
  [AWS DevOps Agent guide](../guides/aws-devops-agent.md) has the two key
  profiles and how to reach Vornik privately or over HTTPS.
- **A companion key is offered only the tools it may call**.
  `tools/list` used to show every memory and skill tool to every key, and
  refused the ones outside the key's scopes only when they were called. It
  now lists only what the key's scopes allow. A key granted `--memory-all
  --skill-all` sees the same tools as before.
- **A read-only key for AWS DevOps Agent on a Vornik-hosted instance**. **Connect your companion** gains *Connect AWS DevOps
  Agent*, which mints a key that reads memory and approved skills only.
  The key table now shows what each key can do.
- **HTTPS from the installer**. On a host with a public DNS
  name and Caddy installed, `VORNIK_PUBLIC_HOSTNAME=<name>` makes the
  quickstart put Caddy in front of the daemon, with a certificate from
  Let's Encrypt over port 443 only. Caddy adds no credential: people sign
  in with their own keys, and the login lockout sees each person's real
  address. Thanks to Veaceslav Mindru, whose community pull request this
  builds on.
- **Vornik 101**. A short guide to where Vornik fits, with an
  optional lab that connects Claude Code and finishes one task. Written by
  Veaceslav Mindru.
- **A companion review opens with its verdict**. The review
  workflows lead with GREEN or NOT GREEN and a numbered list of actions.
- **Community pull requests are credited where they land**.
  The public repository is written by an export, not by merges. A ported
  community change now carries its author as co-author on the public sync
  commit, and the export closes the pull request with a link.
- **Create the GitHub App from the console**. Integrations →
  *Review pull requests with GitHub* creates the instance's GitHub App
  through GitHub's App-manifest flow: GitHub fills in the permissions and
  events, and Vornik keeps the key and webhook secret. You never paste a
  key. It needs the instance's public `https` address
  (`server.public_base_url` or `auth.external_base_url`).
- **Installing the App sets up review**. Each repository you
  install the App on gets a review project of its own, with no YAML. The
  GitHub page lists the repositories and can pause one.
- **The admin tools set concurrency and role limits**.
  `update_project` sets how many of a project's tasks run at once, and
  `define_swarm` sets a role's output tokens and tool steps. Each value is
  capped by a new `agent_admin` key, and a raise needs approval on your
  phone.

- **A pull-request review that is good by default**. For a project set up by installing the App, secrets are masked
  before any model sees the diff, findings must quote the diff they are
  about, a review team reads it, and the result posts as a neutral check
  run with inline comments on the lines found. A large diff is read in
  parts, and the review says which files it did not reach.
- **Manage approver phones from the console**. Manage →
  *Approver phones* lists the phones that approve sensitive actions, pairs
  a new one with a code, and revokes or removes one. It needs a signed-in
  browser session; an owner may pair a further phone but not revoke the
  last one.
- **A managed instance's owner sees more, and changes nothing they should
  not**. The owner reads the instance's swarms and
  workflows, and sees and disconnects a project's MCP sign-in on its MCP
  tools page.
- **Measure what learning adds, on the agent benchmark**.
  A learning study runs one model in four arms: alone, with instincts,
  with self-healing workflows, and with both. Each arm learns on a
  warm-up part of a task set and is measured on a held-out part, with
  learning frozen in between and the learned state recorded. `bench agent
  compare` accepts the arms when the pre-registration names the new
  `learning` axis. The bench-only switches (`bench.learning`) are refused
  on any daemon that is not a benchmark instance. The benchmark's harness
  version is now 13, so its results do not compare with version 12's.

### Fixed

- **A pull-request review never publishes a secret it can recognise** (from a community contribution by Veaceslav Mindru). The
  posted text is scanned, the quoted request and any CI notice included, and
  each credential-shaped value, HTTP Basic credentials among them, is
  replaced by `[REDACTED:<type>]`. An empty diff, or one over 1 MiB, is no
  longer reviewed as if it were the whole change: that review step fails
  with the reason, and nothing is posted. Reviews are pinned to the commit they reviewed.
- **An agent's task key reads only its own task's messages**.
  It could read a sibling task's messages in the same project.
- **A repeated run is told what the earlier one did**. When a
  task runs again after a restart, an interrupted run or a failure, its
  first agent step now lists the tools the earlier run called and is asked
  to check before repeating anything outside the workspace (a push, a pull
  request, a comment, a message). Tool names are listed, never their
  arguments.
- **Agents no longer repeat an identical edit until the run is stopped**. A `file_edit` that changes nothing is refused, and a miss
  says why the text did not match (whitespace, or where it differs).
- **Reviews and dev runs fail less often for the wrong reason**. The analyst must name its pinned test cases, so a missing
  list is retried instead of failing the run; a write that carries the end
  of a tool call is refused, so a review file never keeps the model's next
  turn.
- **Phone approvals**. An approved change outlives the
  phone's request, a written change is never rejected afterwards, and a
  failed apply can be dismissed or retried on the phone and in the console.
- **Memory**. A knowledge-graph entity stored without a
  vector during an embedder outage is embedded once the embedder is back.
- **Managed hosting**. An owner who brings their own key can
  enable memory, and types one endpoint for chat and embeddings.
- **Community Edition**. Six Community features that answered
  only on Enterprise now answer on Community Edition.
- **Regression detection**. The baseline is the seven days
  before the day compared, so a one-day regression is no longer diluted.
- **Codex pricing**. The shipped price table has the Codex
  models' cached-input rates.
- **An upgrade prices new models**. Your `pricing.yaml` was
  copied once at install and never took a model added to the shipped
  table, so that model's spend was charged at the default rate. Now an
  unedited `pricing.yaml` follows the release, and an edited one gains the
  shipped rows it lacks under a `# Added by config-deploy` line. No row you
  have is changed, and the upgrade lists the models whose prices you set
  differently. The previous file is kept as `.pricing.yaml.pre-merge`.
  `vornikctl doctor` names any shipped model your file still lacks.
- **The setup guide finishes on Community Edition**. With
  `api.auth_enabled: true`, step 2 (*Choose what it is for*) answered
  every option with "Could not create it: This is an Enterprise Edition
  feature". The project-template routes now work in both editions, for
  the same operator callers as before.


## 2026.11.6

!!! warning "Upgrade requirements"
    - **Codex subscription users: add the new price rows by hand.** This
      release adds prices for `gpt-6-luna`, `gpt-5.6-terra` and
      `gpt-5.6-luna` to the shipped `configs/pricing.yaml`. An upgrade does
      not yet add new shipped rows to your own `pricing.yaml`, so until you
      copy them in, those models are charged at the `default` rate. Copy the
      three rows into `~/.config/vornik/configs/pricing.yaml`, then run
      `vornikctl config reload`; `vornikctl models list --provider
      codex-subscription` then shows them priced.

### What's new

- **Bring your own model on a managed instance.** An owner who signs up
  without a provided model connects their own OpenAI-compatible provider on
  a new Model provider page. Vornik checks the provider before saving,
  stores the key encrypted and never shows it again, and records each change.
  A fresh managed instance now opens on this page instead of a setup page
  the owner could not use.
- **More of the console for a managed instance's owner.** The owner can grant
  an MCP server's tools to their assistants, and read the operator console's
  observation pages (never change anything there), with those pages in their
  menu.
- **The agent benchmark is open.** Its task sets, answer keys,
  pre-registrations and reproduce script now ship in the Community edition,
  so anyone can re-run a published result. New: 69 Aider Polyglot exercises
  (Go and Python) and `vornikctl bench agent bare`, which measures the same
  model working alone, so the gain from Vornik's pipeline can be measured.
- **Docs at a glance.** The docs open with an architecture picture of what
  Vornik is and does, plus new figures for the life of a task, where each
  credential lives, trust boundaries and access control.

### Security

- **An approved agent MCP server may reach an intranet host**, only one the
  operator lists in `agent_admin.private_api_hosts`, only over https, and
  never loopback, link-local or metadata addresses; each server is still
  approved on your phone. Managed instances refuse the setting.
- **Sign-in works with strict OpenID providers** that accept the client id
  only in the request form.
- **Revoked keys and unpaired devices leave the lists**, and can be removed.

### Fixes

- A provisioned managed instance's agents now reach the daemon, and a fresh
  instance's doctor reports no false errors.
- A managed upgrade checks the new package's dependencies before it stops
  anything.
- Home opens as fast as the other pages; every menu item has its own icon.
- Web chat: a task the chat started reports its result back into the chat,
  replies sit in bubbles, and the chat fits a phone without zooming.
- A request card in Needs you leads with what was asked.
- A chat settings change now says it needs a restart, instead of claiming it
  applied live.
- `/var/lib/vornik` belongs to the service user, including after a restore.
- The Codex subscription's default model, prices and model list follow the
  current Codex models.

---

## 2026.11.5

### What's new

- **A redesigned web console.** The console opens in a calmer light look by
  default (dark is still an option), with larger, clearer type and cleaner
  charts. Navigation is now organised around what you came to do: **Home**,
  **Ask**, **Needs you**, and **Results & cost**, with Manage and Diagnostics one
  layer down.
- **A new Home page.** Home shows a greeting, an Ask box, what needs your
  attention, today's done count, what you spent in the last 24 hours, your next
  scheduled run, and recently finished work — instead of eleven engineering
  views.
- **Results & cost.** A new page shows what got done and what it cost over the
  last 14 days, in one line, one chart, and a table by assistant.
- **A phone-friendly bar.** On a phone the bottom bar is now five tabs — Home,
  Ask, Needs you, Results, More — with Manage, Diagnostics, theme, account and
  sign-out in a More sheet. The theme switch is now reachable on a phone.
- **Chat that works like chat.** Sending a message shows it at once, runs the
  turn in the background, and streams the reply in as it is written. A dropped
  connection no longer cancels the turn, and Stop keeps what was written.
- **Mint your own project keys.** On a managed instance the owner can now mint
  and revoke project API keys from the console, with no shell.
- **First run from the browser.** A new self-hosted install can get to a first
  answer from the setup page without running a shell command.

### Security

- **Secrets are encrypted at rest.** Project integration secrets (passwords,
  signing secrets, webhook secrets) and configuration placeholders are now stored
  in the encrypted credential store instead of plaintext files.
- **Operator credentials from the CLI.** `vornikctl credential list | set NAME |
  remove NAME` manages operator credentials; `set` reads from stdin only. A
  credential value is never logged or echoed.
- **Slack allowlists apply without a restart.** Adding or removing an allowed
  sender or channel now takes effect on a config reload; removing Slack from a
  project closes the channel immediately.
- **Dependency update.** OpenTelemetry was updated to 1.47.0, fixing two reported
  vulnerabilities in the SDK and trace packages.

### Fixes

- **The project settings form refuses a duration with no unit.** A poll
  interval saved as `20` (instead of `20m`) used to fall back silently to five
  minutes; the form now refuses it and says what it expects, such as `20m` or
  `1h`.
- **Web chat no longer promises to report back.** The assistant used to say
  "I'll report back" in the web chat, which cannot yet deliver a later message.
  It now waits for a short task within the reply, or points you to the task's
  page.
- **`doctor fallback_rungs` no longer hides a live fault.** A fallback model that
  failed before reaching the model, or whose last-failure time was unknown, could
  be cleared as fixed when the model worked elsewhere. It now stays at ERROR
  unless the failure was genuinely on the model path.
- **A broker fill is retried while the broker runs.** A fill the daemon failed to
  save used to wait until the broker restarted; it is now retried periodically.
- **Containers recover after a network outage.** The scraper and gateway
  containers now detect when they have no route out and restart themselves.
- **The adoption board counts chat turns and tool calls**, so a chat-first
  project is no longer invisible in the activity totals.
- **Companion hints: no slow match on long commands.** The rule that spots a
  `git commit` after variable assignments now matches in linear time in the
  Hermes plugin (it could take minutes on a crafted long command). The Claude
  Code and Codex hooks get the same rule. Plugins 0.23.3 (Hermes), 0.50.10
  (Claude Code) and 0.45.8 (Codex).
- **Hermes in Docker: mount your project outside the home.** The Hermes plugin
  refuses files in its home directory; in the Docker image the home is
  `/opt/data`. The README now tells Docker users to mount the project as its own
  volume. Hermes 0.23.4.

## 2026.11.4

### Security

- **Go 1.26.11.** The daemon, the CLI and the images are built with Go
  1.26.11, which fixes GO-2026-6859 in `net/http`, reported by govulncheck
  against 1.26.10.

### Fixes

- **The Enterprise packages ship the registry and project templates**. The RPM and deb installed no `configs/` tree, so a fresh
  package install had no registry and the daemon stopped at start ("no
  registry config directory found"). The package now carries the shipped
  swarms, workflows, role library and project templates, and installs them
  into `/etc/vornik/configs` on install and on upgrade, as the daemon's
  user, keeping files you edited. A registry you keep directly in
  `/etc/vornik` is left alone.
- **A managed instance can rewrite its own configuration**.
  Managed setup patches the configuration file as the daemon's user, which
  could not write beside the root-owned file. Provisioning now gives the
  daemon its configuration file and lets it write in `/etc/vornik`, while
  the files root owns there stay out of its reach.
- **`vornikctl playbook show` resolves the codes doctor prints**. `doctor` names error codes such as `UPSTREAM_RATE_LIMITED`;
  `playbook show` answered "Unrecognised failure class" for them. It now
  shows the entry that covers the code and says it resolved it.
- **`doctor fallback_rungs` stops alarming about fixed problems**. A fallback model removed from the configuration, or one that has
  worked since its last failure, kept the check at ERROR for up to 30 days.
  The check now compares its 30-day history against the current
  configuration and the model's own recent successes.

- **Companion hint hooks hold their claims on every host**.
  The Claude Code and Codex hint hooks and the recall nudges claimed a slot
  with `mkdir` and relied on one concurrent caller winning. On Ubuntu 26.04,
  whose `mkdir` is uutils coreutils 0.10.0, more than one caller won in 13
  of 200 measured rounds. A claim is now a file opened with `O_EXCL` (bash
  noclobber), atomic whatever `mkdir` the host has. Plugins 0.50.7 (Claude
  Code) and 0.45.5+codex.20261109.
- **Hermes companion 0.23.1: delegations never upload credential files**
  (Hermes catalogue review). `vornik_companion_delegate` read any regular
  file in the task's workspace, so a model naming `.env`, or `auth.json`
  in the Hermes home, sent it to Vornik, where an ingest could store it.
  It now refuses `.env` and its variants, `.envrc`, and anything under the
  Hermes home, as Hermes's own `read_file` does. Where Hermes does not say
  where the task's workspace is, it refuses instead of reading the process
  directory.
- **Companion hint hooks: no slow match on long commands.** The rule that
  spots a `git commit` after variable assignments could take minutes on a
  crafted ~400-character command in the Hermes plugin; it now matches in
  linear time. The Claude Code and Codex hooks get the same rule (they were
  never slow). Plugins 0.23.1 (Hermes), 0.50.8 (Claude Code) and
  0.45.6+codex.20261111.

## 2026.11.3

### Security

- **The Ollama-compatible API no longer forwards a provider's error text**. `/api/chat` and `/api/generate` used to answer a provider
  error with the provider's own text, which could hold the upstream account
  name and links. They now answer as the OpenAI-compatible proxy does: a
  class code and a fixed or daemon-built message. The provider's text goes
  to the daemon log. A buffered error now carries its class status (400,
  429 or 503) where it was always 502.
- **The companion's recall nudge no longer keeps state in a shared temp
  path**. It uses the plugin's data directory, else your own
  runtime directory, else it stays silent, and refuses a symlinked or foreign
  state directory. Claude Code plugin 0.50.4, Codex plugin 0.45.2.
- **Mirrored files re-marked `never` leave the corpus**. A
  file mirrored into a project's memory and later marked `disclosure: never`
  (or given excluded sections, or broken front matter) now has its chunks
  retired in the next pass. A corpus rollback does not restore them.
- **Agent read tools cannot be raced out of the workspace**.
  `file_read` and `read_many_files` open through the workspace root and check
  the opened file, as the write tools do. A symlink that stays inside the
  workspace still works. Ships in the agent image.
- **The config assistant refuses tokenised calendar URLs**.
  A Google Calendar secret address, an Outlook published calendar or an
  iCloud public calendar URL anywhere in a project's files, prose included,
  is now a secret-hygiene finding. Other providers' feeds are not
  recognised. Move such URLs into a secret.
- **Memory catches a short unlabeled credential**. A random
  24 to 39 character token with mixed case and digits and no label is now
  redacted on its way into memory; a Google Places ID is left alone.

### Improvements

- **Vornik guides how Hermes works**. With the memory key,
  the Hermes plugin (0.23.0) carries Vornik's standing rules in each new
  session's prompt, hints at the moment of need (review after a commit,
  ingest after a design, recall after a failed test), and one check before
  finishing, as Claude Code and Codex have. The Hermes memory key is now
  granted the review and ingest workflows; see the Hermes setup guide.
- **A guide to publishing your own A2A expert**: publishing,
  caller keys, the answer policy, the retrieval policy, and why the
  expert's memory is the boundary.
- **Claude Desktop and Cowork connect with one command**.
  `vornikctl companion connect claude-desktop` stores the key in a file only
  you can read and adds the built-in bridge to Claude Desktop's config: no
  Node, no token in that file. `vornikctl companion doctor --client claude-desktop`
  names each failure and its fix.
- **The overview says what needs you first**. Needs you (the
  inbox's first rows with their actions, approver-device requests, the last
  doctor report's errors), then health problems only, or one "All clear"
  line. The statistics stay below, as At a glance.

### Fixes

- **Only the leader runs the watchdog's startup scan**, so a
  daemon joining a multi-daemon install no longer repeats the leader's sweep.
- **A chunk the embed DLQ could not take is retried, not lost**. It keeps its queue row instead of being released, whether
  the embed call failed or its vector could not be stored or came back
  empty or the wrong size.
- **A cancelled migration run no longer leaves its lock held**, so the next daemon to start does not wait on it.
- **A task message and its counters are written together on SQLite**, as on PostgreSQL.
- **Two knowledge-graph updates of one edge keep both sources on SQLite**.
- **A reset to origin keeps the clone's uncommitted changes**. They are saved to a `refs/vornik/discarded/*-dirty-*` ref first;
  recover them with `git stash apply <ref>`.
- **Workflow proposals are applied and rolled back by the config writer
  only**. A refusal writes nothing and answers 409
  (`APPLY_IN_PROGRESS` or `WRITER_FENCED`).
- **The memory recall audit has a retention horizon**: the
  tool-call audit's window, at least 180 days unless that floor is
  overridden, once retention is enabled (it is off by default).
- **A retry the model circuit rejected keeps its attempt**,
  so tasks released together after an outage no longer run out of attempts
  on rejects.
- **Tool grants and API audits name the right role in every workflow**, not the default workflow's.
- **The web scraper says when its network is down**, instead
  of reporting an SSRF-guard refusal.
- **Admin UI edits wait for other configuration changes**,
  so a UI save and a control-plane apply cannot overwrite each other.
- **The backlog refresh keeps the clone's local work**:
  local-only commits and uncommitted changes are saved to
  `refs/vornik/discarded/*` refs before the reset, as a task-start reset
  does.
- **Deleting a project on some S3-compatible stores deletes every object**, not only the first 1,000; a delete that cannot finish is
  reported as incomplete.
- **Secret checks no longer reveal a secret's length**: the
  `/metrics` scrape token and the broker's internal key are compared as
  fixed-length digests.
- **A parent waiting on its children no longer counts as needing you**: the row stays in the inbox, uncounted, and the overview
  can say "All clear".
- **Doctor counts calls made with a model override**, so its
  per-model failure counts include them.
- **Failed embeddings back off longer each time**, up to 24
  hours, instead of every 10 minutes.
- **Placeholder text is easier to read on the light theme**:
  it meets 4.5:1 contrast in both themes.
- **The benchmark grader and the scraper login browser have a memory
  limit**: half the host's memory, no swap.
- **Memory recall queries are scanned for secrets before they are stored**, like chat transcripts and task messages.
- **The design-edit hint sees DESIGN.md**, whatever the case of
  the name (companion plugins 0.50.5 and 0.45.3).
- **Config changes are refused on a daemon that is not the config writer**: on a multi-daemon install they answer 409 and write
  nothing; a single daemon is not affected.
- **A config reload no longer drops a project whose file it cannot read**: the project keeps its previous configuration, the
  other changes apply, and `vornikctl config reload` exits non-zero with the
  warning.
- **A restart no longer refills the knowledge graph's retry budget**: the count of a chunk's uncounted provider-error retries is
  stored with the chunk.
- **The canary guard runs where it can roll back**: on the
  daemon that holds the config-writer lease.
- **The lead reads a long thread's newest messages**. On a
  thread over 50 messages the lead now sees the latest ones, the operator's
  newest directive included.
- **An approved change that cannot apply no longer deadlocks its project**. A verified revert frees the namespace; an approval whose
  effect keeps failing is recorded failed after 60 attempts (about an hour).
- **A closed approval says which task above it was cancelled**. The inbox shows it for 24 hours, and a stale Telegram button
  answers with the reason instead of "already handled".
- **Agent benchmark tasks no longer see earlier tasks' work in git**. Each repeat now starts on a one-commit history, and the
  earlier history moves to an archive no agent can read. Harness version 11;
  journals from harness 10 and 11 do not compare.
- **A hand-configured rootless install runs its first agent task**. With `userns_mode` empty, rootless Podman now starts agents with
  keep-id; `server.address: ":8080"` gives agents a reachable callback URL;
  and the new doctor check `agent_daemon_reach` flags a loopback listen
  address that agents cannot reach.
- **An empty git repository is bootstrapped**. A project
  directory made with `git init` and no commit gets a seed commit, instead of
  failing every task at merge.
- **The agent benchmark runs unattended past a failed task**. A failed task's leftover files are archived instead of stopping the
  next task, and the reset commit no longer needs a git identity on the host.
- **A pipeline failure is no longer graded as a compile failure**. A benchmark task that failed before its code reached the workspace
  is graded `pipeline_failed`, not `does_not_compile`. Pass rates do not
  change.
- **A memory version pushed and then replaced is labelled out of date**. It was labelled as a draft awaiting its push for good. A
  document redacted on upload now says so, instead of the same false label.
- **Doctor's fallback advice no longer depends on read order**. `model_health` lists every fallback the roles pinning a model name,
  with the number of roles for each. It also counts roles with no fallback,
  and roles whose fallback is the model itself. Before, it kept the first
  role it read, so two runs could disagree.
- **Doctor names a provider's usage limit instead of blaming the model**. A failed call no longer counts as "degenerate output" in
  `model_health`: the token median reads only calls that reached the model.
  An upstream usage or rate limit now reaches the agent as
  `429 UPSTREAM_RATE_LIMITED` instead of a generic `502 PROVIDER_ERROR`.
  The message is fixed; the provider's text is never forwarded. Doctor counts
  these failures and names the account's limit as the cause. External
  OpenAI-compatible callers see a 429 where they saw a 502.

---

## 2026.11.2

!!! warning "Upgrade requirements"
    - **A database API key is bound by its flags on every route.** A key
      minted with `vornikctl key create` reads project memory only with
      `memory_read` and writes it only with `memory_write`; with a workflow
      list it reaches only the A2A routes of those workflows; and it can no
      longer create, list or change keys. Use a static key, an admin key or
      the console for key management. Agent keys, companion keys, static keys
      and console sessions are unchanged.
    - **A workflow output reference that can never work is refused when the
      workflow loads.** `${outputs.<step>.<field>}` naming a `gate`,
      `approval` or `parallel` step, the step itself, or (in a
      payload) a step that does not exist used to load and then fail, or turn
      into an empty string, on every run; so did one in a non-agent step's
      prompt. A daemon holding such a workflow would not start, so the
      updater's preflight now refuses the upgrade and names the file. Check
      with `vornikctl workflow validate <dir>`.
    - **An A2A caller key is limited by default.** A key with a workflow list
      and no rate limit of its own now gets 1 request per second, burst 10, on
      its A2A routes (429 over it). A key's own `rate_limit_rps` and
      `rate_limit_burst` replace the default. Each task such a key submits
      also counts against its `budget_cap_usd` (402 once reached) and the
      workflow's `a2a.dailyQuotaPerKey`, 50 per key per UTC day unless set
      (429 once reached).
    - **Read these boxes yourself for this upgrade.** From 2026.11.2 on,
      `vornik-update.sh` prints the upgrade requirements of every release you
      are upgrading across and will not run unattended until you pass
      `--ack-upgrade-requirements=<target>` (with `--yes`, an acknowledgement
      alone is not enough). The upgrade TO 2026.11.2 still runs the older
      updater, which neither shows them nor knows the flag.

- **One access decision for every operator route** (security). The config
  journal, in-flight, MCP OAuth and agent-grant routes now use the same
  operator check as the rest: a project-scoped key gets 404 instead of 403,
  and a caller with no key gets 403 `OPERATOR_CAPABILITY_REQUIRED`. The two
  memory-firewall policy routes, which answered 400 to everyone, now work.
- **A project-scoped key no longer reads global instincts** (security). A
  scoped caller sees only its project's instincts; a global one is 404.
- **An A2A caller key is bounded** (security). Its tasks are now
  attributed to it, so its budget cap and the daily quota see them; spend
  before this release was not attributed and does not count. A submit whose
  spend or count cannot be read is refused (503). The doctor check
  `a2a_corpus_walk` names a caller key whose A2A tasks retrieved more than
  500 distinct memory chunks in the last 24 hours. It reports the key and the
  count, never the chunks, and blocks nothing.
- **A role can be capped at a sensitivity tier** (security).
  `memorySearch.maxSensitivity: public` keeps the role's search to two kinds
  of chunk: chunks an operator set public, and files under
  `memory.public_source_prefixes` (default `docs/public/`) that the daemon
  checked against the trusted branch. A chunk with no tier counts as
  confidential. The doctor check `memory_sensitivity_cap` states what each
  capped role can retrieve.
- **The A2A consult hop limit is enforced** (security).
  `a2a.consult.max_hops` was read but not applied; a consult chain now stops
  at it.
- **Cancelling a reminder from the chat needs your confirmation**
  (security), as cancelling a task already did.
- **Chat: a running turn can be stopped and steered.** In Telegram, `/stop`
  ends what the assistant is doing in that chat and lists the tasks it had
  started; `/stop tasks` cancels those too. A message you send while your
  own turn runs now joins it at its next step ("Got it, I'll take that into
  account."), instead of waiting until the turn ends; in a group, another
  member's message still waits and runs as their own turn.
- **Chat: one status line instead of a line per tool call.** A turn shows
  one line, edited in place above the reply, with the current activity,
  the time so far and a count per activity, and removes it when the answer
  lands. Slack's placeholder shows the same line.
- **Chat: long work is scheduled as a task.** The assistant now treats
  research across several sources, multi-step work or a written document as
  a task in a project that can run it, and answers in the chat only what one
  or two lookups settle. It makes at most 4 web or consult lookups in one
  turn, and no longer polls task status in a loop. The project list it sees
  names what each project can run, so it picks a project by capability, not
  by a name that matches the topic.
- **Chat: smaller prompts on long conversations.** Large tool results the
  model has already used (from earlier turns, or older than the turn's two
  newest tool steps) are sent shortened to their first 1,600 characters. A
  typical five-call turn sends about 40% less to the model; saved
  conversations keep the full results.
- **Chat fails over when the model is out of quota** (fix). `chat.model`
  now routes by its model prefix like every other model call, so setting a
  Bedrock model as `chat.model` reaches Bedrock even when
  `chat.router.default` names another provider. A reply answered by the
  backup model says so in one line. The new doctor check
  `chat_model_fallback` warns when `chat.model` has no backup in
  `chat.router.model_fallbacks`, or one on the same provider.
- **A routed task that fits no workflow says so** (fix). The adaptive router
  may now answer that none of the project's workflows fits; the task fails
  at once with `ROUTE_NO_FIT`, naming the project's workflows and what the
  task needs, instead of being forced into the closest misfit. Take the
  `route` prompt change into a deployed `configs/workflows/adaptive.md`
  (`config_template_drift` lists it); the router works without it.
- **The doctor no longer reads the routing model as always failing** (fix).
  A route step that handed its task to a workflow was never recorded as a
  success, so `vornikctl doctor` reported the adaptive router's model at a
  100% failure rate and advised switching it. A step that delegates is now
  recorded `ok`; the warning clears within a day of upgrading.
- **A task whose subtasks were all cancelled ends CANCELLED** (fix), and its
  completion notice reports that, instead of reporting success.
- **Output a person can review** (new). Every agent is told how to write
  for a person: answer first, short sentences, exact references, no
  unstated hedges. A swarm or a single role can switch this off with
  `suppressedGuidanceBlocks` (`writing-style`). The doc, architectural and
  GitHub review workflows report readability findings, each with the quoted
  passage, a problem class and a rewrite that keeps every fact. A review
  can withhold approval when its subject cannot be reviewed in reasonable
  time. A new system step, `text.readability`, measures words, sentence
  length, hedges and vague references. Its word lists are English only.
  On one deployment, 20 architecture reviews before and 20 after: mean
  sentence length fell from 17.1 to 10.0 words.
- **Approval notifications on the phone** (new). A paired approver device
  can turn on browser notifications at `/ui/approve/push`, so an install
  with no chat or e-mail alerts still hears of a waiting request. A
  notification says only that a request is waiting, never what it asks,
  and is encrypted to the device. It needs an https public origin. On iPhone
  and iPad it works only from a Home Screen web app (iOS 16.4 or later).
- **Companion hints** (new). `GET /api/v1/companion/hints` serves the
  project's hint catalogue to companion keys, and two project settings turn
  hints off or mute single ones (`companion.hints`, `companion.mute`). The
  daemon also records, per project, whether each hint was followed, and
  turns off for 14 days a hint followed in fewer than 2 of its last 10
  firings. `vornikctl companion hints -p <project>` lists each hint's state
  and follow rate. The Claude Code (0.50.3) and Codex (0.45.1) plugins show
  the hints: after a commit, after a design edit, and after a failed test
  run, each at most once per session. Codex recognises a failed test run
  from its output, since it gives hooks no exit status.
- **Repository paths can be mirrored into project memory** (new). A project
  can list repository paths whose documents are ingested into its memory
  and kept current; a document is mirrored only if its disclosure mark
  allows it, and an unreadable or partial mark fails closed.
- **A project key given to an A2A caller can no longer read the project's
  memory or keys** (security). Before, a key narrowed to one workflow was
  still a full key on the REST API of its project.
- **Add MCP tokens without a shell.** A `secret://NAME` reference now
  resolves from the daemon's environment, then from credentials you enter on
  **Admin → Keys & access → Credentials** (`/ui/credentials`) or type in the control plane's MCP form. Values are
  stored encrypted, never shown again, and every change is audited; saving
  registers a server that was waiting for its token, with no restart.
- **The updater shows what to do before an upgrade** (fix). `vornik-update.sh`
  prints the "Upgrade requirements" of every release between the installed one
  and the target, read from the target's notes, before it changes anything.
  It continues only after you type the target's name, or, with `--yes`, after
  `--ack-upgrade-requirements=<target>`. `--check` prints them too.
- **Companion plugins work behind Cloudflare** (fix). The Claude Code plugin
  (0.48.1) and the Hermes plugin (0.22.2) name themselves on every call, as
  `vornik-companion/<version>`. Python's default signature was refused by
  Cloudflare with `error code: 1010`, so `/upload` failed for a daemon behind
  it. A proxy or firewall in front of the daemon should allow that name.
- **Every step that produces a result is readable by later steps** (fix).
  `${outputs.<step>.<field>}` now resolves for `plan`, `spawn_project` and
  `a2a_call` steps, not only `agent` and `system`. A step that runs again and
  fails no longer leaves its previous result readable.
- **A later agent step can read a system step's output** (fix). A step prompt
  can now reference a system step's result with `${outputs.<step>.<field>}`,
  for example `${outputs.fetch_diff.diff}` in a review workflow's second agent
  step. Before, such a reference passed validation and failed every run with
  `prompt reference unresolved`, so only the agent step directly after the
  system step saw the diff. A value longer than 256 KiB is cut, and the prompt
  says so. The value is otherwise inserted as-is: wrap it in your own
  delimiters and tell the model it is data, because a pull request's author
  controls a diff's text.

---

## 2026.11.1

!!! warning "Upgrade requirements"
    - **Back up `~/.config/vornik/secrets/store.key` beside the database,
      never inside its backup.** Every MCP OAuth token and every credential
      a tool hands to a task is now sealed with it; on first start the
      daemon seals the existing rows once. Without the key, sealed secrets
      must be entered again. Daemons that share one database must share the
      key file and upgrade together. **An older release cannot read the
      upgraded tables**: the way back is the database backup taken before
      upgrading.
    - **Model clients ignore `HTTP_PROXY` and `HTTPS_PROXY`.** A chat or
      embedding provider reachable only through a proxy is unreachable after
      this upgrade; the daemon logs each proxy variable it ignores. Bedrock
      is unaffected.
    - **Spoken replies are off by default.** An install with text-to-speech
      answers voice messages in text. To keep spoken replies, set
      `voice.synthetic_replies: true`, `voice.synthetic_replies_ack: true`
      and a `voice.synthetic_replies_ack_basis` naming who accepted that the
      audio carries no embedded watermark, and why.
    - **A swarm role can no longer set the daemon's agent environment keys**
      (`VORNIK_API_URL`, `VORNIK_LLM_ENDPOINT`, `VORNIK_MEM_URL`,
      `VORNIK_API_KEY`, `VORNIK_LLM_API_KEY`, `VORNIK_PROJECT_ID`,
      `VORNIK_TASK_ID`, `VORNIK_EXECUTION_ID`), in `runtime.envVars` or
      `named_secrets`. Such a swarm file fails to load, naming the role and
      key. No shipped swarm sets one.
    - **`vornikctl subject erase` may ask for a residual assessment**: a plan
      that leaves a person's data inside other participants' messages needs
      `--residual-assessment` and `--assessed-by`.
    - **A sign-in started before the upgrade fails once** (every login now
      carries a PKCE cookie); signing in again works.

EU AI Act marking, a stricter console and stricter outbound connections.

- **AI-generated output is marked, and anyone can check it** (EU AI Act
  Art 50(2), due 2 December 2026). On by default on an install with a
  secret store key.
  - A signed mark in every channel that can carry one: API and web chat
    response headers, email headers, a hidden comment on GitHub texts,
    Slack message metadata, document metadata. A ledger record of every
    mark, which is the only mark on Telegram, agent files and voice.
  - Anyone can look up a content hash, verify a mark and fetch the public
    keys, without an account; answers carry no content, task, project or
    channel. How it works, its limits and how to check:
    [AI transparency](../ai-transparency.md#how-ai-generated-output-is-marked).
  - Visible changes: GitHub texts end with a hidden HTML comment, and mail
    carries `Auto-Submitted: auto-generated`, so mail systems send no
    automatic replies to it.
- **Security.**
  - The console runs only its own scripts: its Content-Security-Policy
    admits an inline script only by the hash of its exact text, with no
    `'unsafe-inline'` or `'unsafe-eval'` for scripts. Inline styles are
    still allowed.
  - The chat and embedding clients dial through the SSRF guard: a model
    endpoint on, or redirecting to, a link-local or cloud-metadata address
    is refused; local and LAN model servers keep working. Every guard now
    refuses the cloud metadata addresses, IPv6 ones included.
  - Chat notices (Telegram, Slack, email) show a task's error and status
    text scrubbed to everyone except an admin-linked user in a private chat.
  - A credential a tool issues during a task reaches private Telegram chats
    only, and is kept out of the tool audit.
  - A task whose role cannot be resolved gets no MCP tools (403, counted on
    `vornik_mcp_gate_refused_total`).
  - Removing an MCP connection deletes its stored sign-in.
  - Agent containers can run with a read-only root filesystem and a PID
    limit (`runtime.agent_read_only_root`, `runtime.agent_pids_limit`, off
    by default). On the 90-task agent benchmark this showed no measurable
    effect on pass rate or energy.
- **Compliance.**
  - Data-subject request deadlines count one calendar month from receipt
    (GDPR Art 12(3)), not 30 days from when the command ran.
  - An erased chat session stays erased: a later save can no longer write
    it back, on any node.
- **Memory.** Companion documents verify on self-hosted forges (GitLab,
  Gitea, Forgejo, GitHub Enterprise, plain git) listed under
  `memory.provenance.forges`; a repository too large to clone is checked
  through the forge's API. On upgrade each automatic clone is made once more.
- **Benchmarks.** An open-weight model on one GPU is published:
  `gemma-4-26b-a4b` on one RTX 3090 passed 89 to 90 of 90 agent tasks, for
  €1.34 to €1.40 of electricity per run. [Results](../benchmarks/results.md).
- **Reliability.** A schema-constrained retry no longer fails on llama.cpp
  (HTTP 400); all configuration writes share one lock, so two writes can no
  longer lose an update.
- **Console.** Standing explanations on the reporting pages moved into an ⓘ
  tooltip that opens on hover, focus or tap.
- **Companions.** The Claude Code plugin (0.48.0) takes its key from Claude
  Code's options dialog, which keeps it in the system credential store.
- The web scraper identifies itself as `vornik-agent/<release>`.

## 2026.10.7

!!! warning "Upgrade requirements"
    - **Existing installs: give the database its own password.** A fresh
      quickstart generates a per-install database password. An install that
      kept a volume with no known password now stops and writes nothing,
      instead of continuing on the shipped default, and the bundled compose
      files refuse to start without `POSTGRES_PASSWORD`. Three doctor
      checks report a weak, reused or network-exposed database credential:
      `db_credential_strength`, `db_listen_exposure` and `credential_reuse`.
    - **The operator surface needs an admin-class key** (403
      `OPERATOR_CAPABILITY_REQUIRED`). This covers config reads and reload,
      the memory firewall, operator profiles, the operator audit,
      control-plane changes, setup and the project wizard. Add any
      `api.api_keys` entry that needs them to `admin.allowed_keys`. Auth-off
      installs and the quickstart key are already admin-class.
    - **Logs and error text follow the key's scope.** A project-scoped key
      reads a scrubbed view of its own project's logs, explain output and
      error fields. An unscoped key that is not admin-class reads none
      (403 `PROJECT_SCOPE_REQUIRED`).
    - **Agent keys reach only the routes the agent image calls,** and get
      403 `AGENT_KEY_NOT_OPERATOR` on every other route.
    - **Operator-typed URLs dial through the SSRF guard,** redirects refused:
      the MCP Test probe, MCP OAuth, the GitHub App API base and broker
      fetches. Add a LAN or loopback server's hostname to
      `integrations.allowed_hosts`.
    - **With auth off, the daemon answers only its own names.** When
      `api.auth_enabled` is false (including `api.auth_dry_run`), the HTTP
      listener refuses, with 403, a request whose `Host` is not an IP
      address, `localhost`, `host.containers.internal`, the host of
      `server.public_base_url` or `auth.external_base_url`, or a name listed
      in the new `server.accepted_hosts`. This stops a web page from reading
      daemon answers through DNS rebinding. A cross-site POST is refused
      too, and `Sec-Fetch-Site: none` no longer counts as same-origin.
      If you reach an auth-off daemon by a LAN name, for example the
      machine's hostname or a router DNS name, add that name to
      `server.accepted_hosts`. Installs with auth on are unchanged.
    - **Generic webhook sources accept only the project's own repositories.**
      Set `webhooks.sources[].repos` to choose; `["*"]` accepts every
      repository. An event from another repository is answered 200,
      filtered, and recorded as `repo_not_in_scope`.
    - **A tool the MCP classifier calls mutating is never approved as a
      read,** and an approved read tool whose server later marks it as
      changing things is suspended until you approve it again on the device.
      Agent read servers are listed again every
      `mcp.agent_read_relist_interval` (default 15m).
    - **Agent containers get no swap** (`--memory-swap` equals `--memory`).
      Raise `runtime.agent_memory_limit` if a role needs more memory.
      **Pull or rebuild the `vornik-agent` image:** its local MCP fallback is
      removed, and MCP tools are discovered only through the daemon.
    - **Companion notes no longer verify themselves.** A companion's
      `remember` of a decision or spec is stored unverified, with a finite
      lifetime. A companion-delegated ingest is verified when its bytes match
      the committed file at the same path on the repository's trusted branch
      (its default branch unless the project names another). List the
      hash ids of your own admin keys in `admin.operator_key_ids` so the
      tasks they start count as yours.
    - **Derived copies of transcripts expire:** black-box traces after 1 day,
      project gists after 7 days, and of `vornik-update.sh` database dumps
      the newest 2 are kept (keys under `retention.*`). Copy any dump you
      want to keep before upgrading.
    - **The console no longer loads a third-party CDN,** and it sends a
      Content-Security-Policy that allows no source outside its own origin.
    - **`vornikctl task list` shows 20 tasks by default and says so;** use
      `--limit`, `--all`, `--offset` or `--active`. `/ui/tasks` pages past
      100 rows.
    - **Building from source needs Go 1.26.10.**

A security release, and memory you can trust across repositories.

- **Security.**
  - Agent keys are confined to the agent routes, and an agent can no longer
    post as the operator or cancel and retry other tasks.
  - Logs, error text, step output and chat status tools serve a scrubbed view
    to non-admin callers. The scrubber sees through layered encodings.
  - The post-mortem explainer sends only the scrubbed log to its model
    provider.
  - Only a running task's own agent may summarize its thread, and a summary
    never hides an operator message. A conversation's author is the signed-in
    caller, never a client-supplied id.
  - The untrusted-content wrapper escapes in a form the model sees, and agent
    memory search wraps every writer-controlled field.
  - A memory correction from a companion can no longer refute the operator's
    own correction.
  - Control-plane crash recovery no longer mistakes an apply's own writes for
    drift, and every rollback holds the config-writer lock.
  - Every transcript write is scanned for secrets before it is stored, and
    `vornikctl secrets scan-history` covers task messages and step signals.
- **Memory.**
  - Provenance verification works with no configuration for every
    repository a project ingests from on github.com, gitlab.com,
    bitbucket.org or codeberg.org. The daemon keeps its own blob-less clone
    of each one, and a repository it cannot clone keeps its earlier trust,
    labelled unavailable. A file ingested before it is pushed is checked again
    over the following hour.
  - Each companion recall hit carries a `provenance` label: verified,
    unavailable, or why it is unverified.
  - `repo_scope` holds on every recall path.
  - Damaged text never becomes a memory chunk, and the doctor's
    `memory_text_damage` check finds any already stored.
  - Knowledge-graph extraction no longer quarantines a chunk for a split
    character, an unknown resolver id or a transient provider error.
    `vornikctl memory regraph --quarantined` re-queues the quarantined ones.
- **Compliance.**
  - The GDPR Art 15 transcript export covers every text column, withholds
    third parties' fields and redacts secrets.
  - An Art 17 erasure also evicts the black-box trace cache.
  - The doctor watches the Art 12(3) response deadline.
- **Reliability.**
  - A retry after `MODEL_UNHEALTHY` waits until the model's circuit can
    admit a call.
  - Cancelling a parent ends every unfinished descendant, paused and
    awaiting ones included, and a resume that races a cancel does nothing.
  - A model provider that never answers no longer hangs its caller, and a
    CLI provider's timeout kills its whole process group.
  - Silent-failure sites in fifteen subsystems now log and count what they
    drop.
  - Deleting a project now removes its rows from every project table.
- **Scheduled work can start from the latest code.** A project that sets
  `workspace.refresh_before_work: true` has its workspace reset to
  `origin/<branch>` before each task from a task-kind reminder, a broker
  schedule or autonomy, the same reset a forge task already gets. Commits
  that exist only in the workspace are kept in a `refs/vornik/discarded/*`
  ref. `workspace.refresh_branch` picks the branch (default: the
  workspace's current branch). Every attempt is counted in
  `vornik_executor_workspace_refresh_total{project, source, outcome}`. Off
  by default, and refused together with `autonomy.mode: backlog`, which
  refreshes on its own. See [Autonomy](../guides/autonomy.md#refreshing-the-workspace-before-scheduled-work).
- **Companions.** Claude Code 0.47.4, Codex 0.43.2 and Hermes 0.22.1. The
  plugins ship develop-with-memory guidance that adapts to projects with
  few written designs. Outside a git repository they send no repository
  scope.
- **Operator commands and console.** `vornikctl task list` pages, with a
  PARENT column. `/ui/tasks` pages past 100 rows. The task page shows
  Pause on desktop and Cancel on mobile. The footer links to these release
  notes. `vornik-update.sh` runs the doctor, as it said it did.

## 2026.10.6

!!! warning "Upgrade requirements"
    - **`vornikctl subject resolve-kg` needs `--request <request-id>`** naming
      a verified request for the same subject.
    - **Task input is staged only from the task's own project's artifacts.** A
      deployment with no artifact store stages no task input.
    - **A failed per-task key mint refuses the step** instead of starting the
      agent with the static key.
    - **Media extraction accepts 15 container formats** (mov, matroska, ogg,
      mp3, wav, flac, aac, avi, asf, mpegts, mpeg, flv, aiff, amr, caf); any
      other format fails extraction.
    - **Building from source needs Go 1.26.9** (`go 1.26.0`, `toolchain go1.26.9`).
    - **`retention.graph_quarantine_days` (including `-1`, a legal hold) and
      `memory_eviction_audit_days` are now honoured.** Before this release
      quarantined knowledge-graph rows were swept at 30 days and
      `vornikctl retention --apply` used 365 days for eviction-audit rows,
      whatever was configured.
    - **Tool audit rows are kept at least 180 days.** A lower
      `retention.tool_audit_days` behaves as 180 unless you set
      `tool_audit_floor_override` with `tool_audit_floor_override_basis`.
    - **The skill distiller drafts a skill only from a task it can prove read
      no third-party content**, so expect few or no automatic drafts.
    - **New bounds:** stdio MCP calls stop at the server's `timeout_seconds`
      (default 120 s); podman run and pull stop at `runtime.image_timeout`
      (default 15 m) and other podman verbs at 60 s; email and GitHub chat
      turns stop at 10 m, and the email is then marked seen.
    - **`scheduler.execution_timeout`** is now read as the ceiling for a
      workflow with no `maxWallClock` (empty means none).
    - **The unix socket serves only the agent routes**, `POST /api/v1/doctor`
      and daemon-scope fix-it sessions need an admin-class key, and A2A peers
      receive only the lifecycle view of live events.
    - **Forge issue close and label writes** need
      `forge.issues.writes_without_approval: true`.
    - **Metrics:** `vornik_chat_requests_total{status="error"}` no longer
      includes timeouts and cancels (now `timeout` and `cancelled`), and
      `vornik_chat_model_health_trips_total` gains a `reason` label.
    - **Failure classes:** `MERGE_FAILED` now comes only from the merge-back;
      new classes `PROMPT_TOKEN_BUDGET`, `OUTPUT_CAP` and `wall_clock_exceeded`.
    - **Migrations 219–230** are applied automatically; SQLite gains the GDPR
      subject tables on the next start.
    - **Rebuild the agent image** for the sandboxed `document_render` helper,
      byte-correct tool-audit and shell-output caps, and the piper bundle's
      licence texts and source offer.
    - **The agent image's piper bundle carries espeak-ng (GPL-3.0-or-later).**
      Each release attaches its corresponding source archive,
      `vornik-agent-third-party-source-piper-2023.11.14-2.tar` with its
      `.sha256`; keep it available if you redistribute the image.
      The same archive is the corresponding source for the agent images of
      releases 2026.9.7 to 2026.10.5 and for images built from `main` since
      2026-09-26, which shipped the same piper bundle without licence texts
      or a source offer.
    - **Client cancels** (companion, Hermes, REST, Telegram `/cancel`) now
      cancel tasks waiting for children, input or an external event, and REST
      also cancels PAUSED tasks; a task awaiting approval is still refused.
    - **Bedrock clamps a requested `max_tokens` to the route cap**, like every
      other provider.
    - **SQLite:** deletes now remove child rows as Postgres does, and the
      retention sweeper now deletes there, so the first sweep can remove a
      backlog of expired rows. Rows orphaned before this release are not
      cleaned up.
    - **The skill distiller also checks where a task's goal was created;**
      tasks created before the upgrade are never distilled.
    - **Deleting a project now also deletes its OAuth tokens, memory audit
      rows, standing broker grants, tool grants, and workflow and integration
      approvals.** Export any of these you must keep first. A project
      re-created under a deleted project's ID no longer inherits its grants or
      approvals.
    - **SQLite: archived projects past their grace period are now wiped.**
      Before this release the wipe failed and deleted nothing, so the first
      sweep after the upgrade deletes every such project.
    - **Project delete-now waits for the outcome:** the API answers
      `deleted`, `deleted_with_errors` (with a warning and `residue_steps`
      naming what remains: artifact files, or a registry reload the next
      sweep completes), `DELETE_FAILED` (500) or `deletion_pending` (202),
      and `vornikctl project delete-now` exits 0, 3 (removed with residue)
      or 1 (not deleted).
    - **Companion tool calls that repeat an argument key are refused**
      (compared case-insensitively). Two argument names a 2026.10.5
      `vornikctl bench` sends are still accepted and counted in
      `vornik_companion_legacy_argument_total`; upgrade `vornikctl`.
    - **A `chat.timeout` of `0s` or less no longer leaves model calls
      unbounded;** the 300 s default applies.
    - **Metrics and outcomes:** `vornik_judge_evaluations_total` no longer
      counts a failed judge call as `ok`, and a timed-out autonomy evaluation
      is the new outcome `LLM_TIMEOUT` instead of `LLM_ERROR`.
    - **Companion bundles:** Claude Code 0.46.1, Codex 0.42.1, Hermes 0.21.6.
    - **The Hermes plugin reads its settings through Hermes,** so a Hermes
      gateway serving several profiles gives each profile its own Vornik URL
      and keys.
    - **The Hermes plugin no longer coaches a model to re-store a
      duplicate:** a note refused as already stored now says so and that it
      must not be stored again, so a forgotten fact is not re-stored
      reworded; and a note carrying leaked tool-call syntax (a garbled
      call meant for another tool) is refused before it reaches Vornik.
    - **Hermes's unattended sessions never reach the phone.** Cron jobs,
      `hermes chat -q` and webhook or `api_server` sessions with no client
      that can answer follow Hermes's own `approvals.*_mode` settings, and
      `hermes -z` runs in YOLO mode, so `hermes vornik approvals on` does not
      reach them. This is the limit for this release; lifting it needs a
      change in Hermes.
    - **With a small local model, "forget" can still be stored instead.**
      A model that sends Hermes's memory removal to Vornik as a new note,
      with garbled arguments, is refused for the shapes the plugin knows,
      but not for every shape. After asking Hermes to forget something,
      check `/vornik-memory` and forget any stray note there. Newer
      tool-calling models make this mistake far less often.

A security, compliance and "no silent hang" release.

- **Security.**
  - Task input staging is confined to the task's own project.
  - Step-output harvesting reads every file through one pinned directory.
  - Document renderers cannot read local files, fetch, or run script.
  - ffmpeg and ffprobe read only the uploaded file.
  - Live tool-call events carry the redacted audit row.
  - Broker artifact downloads are scanned byte for byte, and cached broker
    answers are tied to the account that produced them.
  - Gateway redirects stay on the same origin and are capped.
  - The skill distiller refuses goals that came from third-party surfaces.
  - A per-request `max_tokens` can no longer exceed the Bedrock route cap.
  - A project re-created under a deleted project's ID inherits none of its
    grants or approvals, and a deleted project's OAuth tokens are deleted.
  - Approver-device choice cards can be answered only with a choice.
  - A companion tool call that repeats an argument key is refused, so the
    audit row always shows the arguments that ran.
- **Compliance.**
  - Every outbound send of agent-authored content carries the AI Act Art 50
    notice; on Slack the notice and content go in one post.
  - GDPR Art 18 restriction (`vornikctl subject restrict`,
    `lift-restriction`) and Art 16 rectification run, and restriction also
    covers knowledge-graph reads.
  - A console page resolves a verified subject request, and the CLI applies the
    same gates.
  - The GDPR subject store and rights ledger work on SQLite, and the
    retention sweeper runs correctly there.
  - The agent image ships the licence texts and a source offer for its piper
    speech bundle.
- **Reliability.** Podman verbs, stdio MCP calls, system steps, repository
  commands and chat turns are bounded, and a bound that fires is recorded with
  its cause. A config-blocked task is re-queued, failed merge work is kept on a
  preserved ref, and approval pauses say what releases them. A CLI model
  provider call returns at its timeout even if a child process holds its
  output, a failed or timed-out judge is no longer counted as a success, and
  project deletion reports a partial or failed wipe instead of claiming
  success.
- **Hermes and companions.** Memory the user can see and forget, several
  accounts per provider, progress and result reuse, file reads from completed
  runs, and daemon-side GitHub issue triage. The paired approver device can
  install a read-only connector the operator has vetted, resolve an
  interrupted config apply, and shows requests the daemon withdrew; an
  assistant is told why its project is paused. The Claude Code bundle now
  names its tools as a plugin install exposes them, and the Claude Code and
  Codex bundles add an offline recall nudge (`VORNIK_NUDGE=off` disables it).
  A backlog deposit can record the level a bug was found at (`found_at`,
  `--found-at`).
- **Agent administration.** Refused agent-admin calls are recorded without
  their inputs, counted, and shown on the assistants page. A daemon-side
  failure to record a change is no longer counted as the assistant's refusal,
  and a failed refusal read degrades the page instead of breaking it.
- **Operator commands.** `vornikctl daemon drain`, `undrain` and `inflight`,
  and `vornikctl control-plane journal list` and `resolve`; `vornikctl
  retention` works on SQLite.
- **Console.** Form controls meet WCAG 1.4.11 contrast in both themes. The
  cache-savings figures name their cache and window: the spend page's
  provider prompt-cache savings cover the page window and every role/model
  pair, and the response cache served by Vornik covers all cached entries.
- **Correction.** Four items earlier listed under 2026.10.5 ship in this
  release; that section is marked. Five project keys carried a wrong
  "since" release: `broker` and `companion_push` shipped in 2026.10.1, and
  `retention.memory_require_approval` and `retention.memory_idle_days` in
  2026.10.5.

## 2026.10.5

!!! warning "Upgrade requirements"
    - **Pairing and approving need HTTPS.** `/ui/pair` and `/ui/approve/`
      refuse plain http to a non-loopback host. Serve TLS, use a TLS proxy, or
      browse to `http://localhost`.
    - **`X-Forwarded-Proto` counts only from a trusted proxy.** Enable
      `server.real_ip` and list the proxy host's own addresses in
      `server.real_ip.trusted_proxies` as single addresses (`/32`, `/128`,
      `127.0.0.1/32`), never a subnet that carries containers. The proxy must
      replace the header, not append to it. Behind a TLS proxy that is not
      listed, pairing and approving answer "HTTPS required". The daemon warns at
      startup when its public origin is https and `server.real_ip` is not
      configured.
    - **Idle memory retention can require phone approval (opt-in).** Set
      `retention.memory_require_approval: true` on a project and unused
      memory is no longer deleted automatically: after the idle window
      (`retention.memory_idle_days`, default 365) the daemon asks on the paired
      phone. Rejected or unanswered requests keep the data. Deploy this release
      before adding the key, then `vornikctl config reload`. Turning it off
      restores TTL and age-based deletion; back up first.
    - **Skill review can name the revision.** `skill_approve` and
      `skill_reject` accept `expected_version`; a superseded revision is refused.
    - **The updater refuses a downgrade** unless you pass `--allow-downgrade`,
      and `--ref main` uses the fetched `origin/main`.
    - **Rebuild the agent image** for the new `document_render` helper.
    - **Migrations 216–218** are additive and applied automatically.
    - **Hermes companion plugin 0.11.3** (from 0.11.1). *(Corrected 2026-10-13: an
      earlier edit of this page named 0.20.0, which ships in 2026.10.6.)*
    - **`vornikctl agent connect` exits 1 with one refusal line.** A script
      that matched "not connected" must match the new line or the exit code.

A brand, Hermes, approvals, memory and agent-admin release.

- **New brand.** The console, app icons, favicons and the docs site move to the
  new Vornik mark and colour scheme. The new console colours meet WCAG AA
  contrast in both themes, and the console and docs site serve their fonts
  themselves instead of calling Google Fonts. The docs site gains a brand page.

- **Hermes.**
  - Turning approvals on replaces a stale pin on a disconnected namespace.
  - Replacing a note with identical text no longer forgets it.
  - A note you forgot and add again is recallable again, when the forget came
    from Hermes's own forget path.
  - The admin skill reads the active definition of an owned workflow before
    editing it. Broker results accept only the declared output from an answer
    step in the latest execution. Older completed runs may need a fresh run;
    their files remain available through the artifact tools. *(Ships in
    2026.10.6, not 2026.10.5; corrected 2026-10-13.)*
- **Agent admin.**
  - `list_recipes` files the cover request every read owes.
  - `agent connect` states its refusal once.
  - Verbs accept a project's full id.
  - A credential already requested for a namespace serves the next project.
  - `update_project` changes a project's description or display name.
  - A role granted the `document_render` helper can convert staged Markdown to
    a PDF or HTML file.
  - Google document links are no longer redacted as high-entropy strings.
  - Adding an MCP server with an existing API name is refused. *(Ships
    in 2026.10.6; corrected 2026-10-13.)*
- **Approvals and devices.**
  - A lost pairing-poll response no longer unpairs the browser.
  - Paired devices get distinct default names, and the devices page shows the
    id, the last use and "never used".
  - `vornikctl pair-device` prints an https address and warns on plain http.
- **Standing grants.** A live grant is never hidden behind dead ones, and dead
  grants no longer starve live grants' digests.
- **Fixes.**
  - The updater prefers the fetched remote branch and refuses a downgrade.
  - Project removal is verified by the file's absence.
  - `vornikctl backup` explains a PostgreSQL client and server version mismatch.
  - The doctor checks agent models against your catalogue.
  - Guarded redirects stop after 10 hops.
  - Live task pages close dead websocket connections.
  - Skill reviews are bound to the revision they were shown.
  - Memory consolidation skips refuted and superseded notes. *(Ships in
    2026.10.6; corrected 2026-10-13.)*
- **Redirect discovery.** A new skill investigates an unexpected public
  redirect and reports only what independent evidence supports. See
  [Discover and validate redirects](../guides/redirect-discovery.md).
- **Runtime.** A Codex response cut off at the output cap reports
  `finish_reason: length`.

## 2026.10.4

!!! warning "Upgrade requirements"
    - **Rebuild the agent image.** It now contains the output-cap guard and
      the `glob` fix for a trailing `**`.
    - **Broker workflow schemas may no longer use property names containing
      `.`, `[` or `]`** in their input or proposal schemas. Rename such
      fields before you upgrade, or the workflow will not load.
    - **Project APIs no longer follow redirects.** Point `base_url` at the
      final endpoint.
    - **The updater now replaces config files you never edited** in canonical
      config directories, and lists each one it replaces. Files you edited are
      kept.
    - **Migrations 212–215** are additive and applied automatically.
    - Plugins: Hermes 0.11.1, Claude Code companion 0.32.0, Codex companion
      0.29.0.

A reliability and agent-admin release.

- **Paired phones stay paired.**
  - A double tap, two open tabs, or a slow decision could sign an approver
    phone out while other devices still listed it as active. Requests already
    in flight now share one renewed sign-in, so concurrency no longer signs a
    phone out.
  - A phone that does sign out, because its last answer never arrived, is told
    why. A `vornikctl pair-device` code restores the same device.
  - Per-approval renewal is kept, so a copied sign-in still dies at the
    phone's next approval. A browser riding along on the phone's approvals
    raises an alert.
- **Recipes, document input, per-role models.**
  - Agents can install tested recipe workflows: inbox digest, agenda and
    morning brief.
  - A workflow can take a text document as input, read-only.
  - An agent role may choose a model from the operator's catalogue. A remote
    destination needs one approval on the phone.
- **Setup clients see when a phone decision completes.** `list_my_setup`
  carries content-free notifications for setup approvals.
- **Credential entry refuses blank secrets.** Whitespace-only and
  invisible-only values are rejected; accepted values are stored exactly as
  entered.
- **Approvals.**
  - Plain-language summaries and risk levels.
  - Batch review of a workflow's proposed writes.
  - Bounded standing grants for repeated broker writes.
  - Hermes's own safety prompts can be answered on the paired phone.
  - An approval link opened in an unpaired browser leads back to the approval
    after pairing.
- **Agent-built workflows.**
  - Roles receive their instructions again.
  - A reply cut off at the output cap is never accepted as an answer.
  - A step that does not produce its handoff file fails instead of silently
    succeeding.
  - Failures keep their precise class for `task explain`.
- **Project APIs** support `auth.query_param`.
- **Older uid-agnostic agent launches are fixed.** With `keep-id` and no
  `run_as_user`, uid-agnostic agent images run as the daemon's UID and GID,
  and doctor reports the real shape.
- **Dependencies.** The telemetry server uses `undici 7.29.1`; the docs use
  `mkdocs-material 9.7.7`.

## 2026.10.3

A patch release.

- **`vornikctl` for macOS (arm64, amd64) is attached to the release.**
  2026.10.2 withheld it: the check that runs it on a Mac before attaching it
  could never pass. The check is fixed and now also runs on every push.
- **Hermes companion plugin 0.4.0.** `hermes vornik connect` connects Hermes to
  Vornik in one command (it runs `vornikctl agent connect hermes` and installs
  nothing itself); `hermes vornik status` shows what is configured, never a
  token's value. `VORNIK_URL` and the tokens are optional: an admin setup needs
  none of them. The plugin is ready for Hermes's plugin catalog.

## 2026.10.2

!!! warning "Upgrade requirements"
    - **Migrations 205–211** add the tables for agent credentials, approver
      devices and approval requests, and agent key columns. All additive.
    - **Back up `~/.config/vornik/secrets/store.key` once it exists.** Vornik
      creates it the first time an assistant's credential is stored, and the
      stored credentials cannot be opened without it.
    - **Deploy the new config assets once after this update.** This update
      runs the previous release's `vornik-update.sh`, which does not yet
      deploy them:
      `VORNIK_DEPLOY_REVISION=$(git -C ~/vornik rev-parse HEAD) ~/vornik/scripts/config-deploy.sh ~/.config/vornik`.
      From the next update on, the updater does it. Vornik picks them up
      without a restart.
    - No agent image rebuild is needed for this release.

**Vornik now lives at [github.com/EaseIT-cz/vornik](https://github.com/EaseIT-cz/vornik).**
The repository moved from `grinco/vornik`. Issues, releases and history came
with it, and the old address redirects, so an existing checkout keeps
updating.

- The agent image is now `ghcr.io/easeit-cz/vornik-agent`, with every
  earlier tag copied unchanged.
- If your configs still name `ghcr.io/grinco/vornik-agent`, they keep
  working: Vornik maps the old name. `vornikctl doctor` lists the files and
  the one-line command that updates them.
  *Correction (2026-10-02): in 2026.10.2 and 2026.10.3 that doctor check
  does not run in `vornikctl doctor` (it was wired into the wrong report);
  it does from the next release. Until then, find the files with
  `grep -rl ghcr.io/grinco/vornik-agent ~/.config/vornik/configs` and update
  them with `sed -i 's#ghcr.io/grinco/vornik-agent#ghcr.io/easeit-cz/vornik-agent#g'`.*

**Let your assistant set up Vornik, with Vornik as its safety layer.** Hermes,
Claude Code (including the Claude desktop app's Code-tab routines), Claude
Desktop and Codex connect with `vornikctl agent connect`. The assistant then
sets up its own projects, workflows (with schedules), REST APIs and MCP servers
through Vornik's MCP tools, and runs them. It never sees a credential:
credentials are entered on your paired phone. Every new connection, every
credential and every write the assistant wants to make is approved there, and
it receives only results shaped by the schema you approved. See
[Let your assistant set up Vornik](../guides/assistant-setup.md).

- `vornikctl pair-device` pairs your phone. Do it yourself, never through the
  assistant.
- `--url https://…` connects an assistant on another machine, for example a
  Mac, to your Vornik. `vornikctl` for macOS is now part of each release.
- The console's **Assistants** page shows what each assistant has set up.

**Fixes to `vornik-update.sh`:**

- After pulling the agent image, the update now tags it. Before, the image
  arrived untagged, so `ghcr.io/easeit-cz/vornik-agent:latest` named nothing, or
  the previous image, after an update that reported success. This fix applies
  on the update to this release.
- The rollback record (`STATE.txt`) now names the commit that was installed,
  not the checkout's, and prints no rollback commit when it cannot tell. An
  install whose checkout already sits at the target is updated without
  `--force`. A backup written by an earlier release can name the wrong commit
  here; use `<backup>/vornik.prev -version` instead.
- A missing `skopeo` is now named, rather than reported as "the registry could
  not be reached".
- An interrupted image build removes the build containers it left; older
  leftovers are reported with the command that removes them.
- Updates and re-run quickstarts now deploy config files a release adds,
  without changing any you have edited.

**Releases carry binaries.** Each Community release now attaches `vornik` and
`vornikctl` for Linux (amd64, arm64) and `vornikctl` for macOS (arm64, amd64),
with checksums, an SBOM and build provenance attestations. The macOS binary is
not signed: check it against `checksums.txt`, then run
`xattr -d com.apple.quarantine vornikctl`.

*Correction (2026-10-02):* 2026.10.2 itself shipped **without** the macOS
`vornikctl`. The check that runs it on a Mac before attaching it had a defect
and could never pass, so the release withheld both macOS binaries, as
designed when that check fails. The Linux binaries are unaffected. The macOS
`vornikctl` ships from 2026.10.3.

**Also fixed:** the task link in Slack and chat completion notices opened a
"project not found" page; it opens the task.

---

## 2026.10.1

!!! warning "Upgrade requirements"
    - **Rebuild or pull the agent image with this release.** The agent-side
      fixes below (the router, the prompt-token budget, the tool-result cap,
      request sizing and the memory-peak report) live in the image; with an
      older image none of them takes effect.
    - **SQLite: the first start rewrites stored timestamps** into one
      fixed-width form, once, one column at a time. Take a backup first.
      Postgres is not affected.
    - **A workflow whose `retry.on` names a deterministic error class no
      longer loads.** `retry.on` may name only `unclassified`,
      `llm_call_failed`, `container_start_failed`, `container_wait_failed`,
      `container_killed` and `context_timeout`; the error lists them. The
      shipped workflows already name only these.
    - **Fresh quickstart installs require an API key** (see Security). An
      existing install is not changed; re-running the quickstart over a
      config with `api.auth_enabled: false` prints a warning and leaves the
      file alone. If your install is reachable beyond your own machine with
      auth off, turn auth on.
    - Set `broker: true` on a project only after upgrading the daemon; an
      older daemon rejects a project file that carries it.
    - The `config_template_drift` doctor check can report findings it did not
      report before (a template value your deployment never took).
    - An MCP subscription by name only, with no matching server in
      `config.yaml`, is now withheld, and the doctor's
      `mcp_project_connections` row reports it as a warning.

**Security fixes from an external scan, and safer quickstart defaults.** An
external scan of a deployment found three real problems:

- `GET /api/v1/config` returned `admin.allowed_keys` and the S3
  `access_key_id` values in plaintext to an operator-scope caller. The dump
  redacts by key name and these names matched none of its rules. They are
  redacted now, and a test checks every credential-shaped config field.
  What it does not cover: a secret stored under a name that contains none of
  `key`, `pass`, `secret`, `token`, `cred` or `private`.
- The configuration assistant could pass a secret-named list (`api_keys:`,
  `allowed_keys:` written as a literal list in a project file) to the model.
  Such lists are now redacted whole, including lists of lists and lists of
  maps; a list of `${VAR}` references stays editable. A secret reached
  through a YAML alias (`api_key: *tok`, with the value defined under a
  harmless key) also passed; the assistant now refuses to read such a file
  until the value is inlined or moved to `${VAR}`. It also refuses a YAML
  file with more than one `---` document, whose later documents were never
  screened.
- `/ui/static/`, which is public so the login page can load, served a list of
  its files. Directory requests now return 404; the files themselves stay
  public.

The quickstart seed (also used by the Lima and macOS installers) listened on
all interfaces with auth off. A fresh install now generates an API key into
`~/.config/vornik/secrets/api.env` (readable only by you) and requires it;
log in to the UI with it, and source `~/.config/vornik/shell-env.sh` for
`vornikctl`. A fresh install also gets its own database password, but only
when no database exists yet, so a re-run never locks you out.

**Browser-login sessions can use the console again.** Since 2026.9.6 every
action that changed something (pausing a task, rechecking a channel) was
refused with `403` for a session logged in through the browser, because the
console never sent its CSRF token. It does now, and the `403` names the real
cause instead of blaming Basic Auth.

**Vornik as the broker for a front-end agent.** A chat agent such as Hermes
Agent can now hold no credentials at all: it delegates to workflows you write
in a project marked `broker: true`, passes typed inputs, and reads back only
one declared, schema-checked result document. Raw task content stays inside
Vornik. A broker workflow can also propose a write (a mail reply, say); you
approve it in `/inbox`, and Vornik executes exactly the arguments you
approved, once, with no model in between. Writes are off until you set
`broker.writes: on`. `result` can wait up to 25 seconds for a task to finish,
and a harness with a webhook can be pushed task and action states instead of
polling. `vornikctl companion grant --no-delegate` mints a memory-only key
that cannot start tasks. The Hermes plugin in `contrib/hermes-companion/` is
at 0.2.2; earlier READMEs named the memory provider `vornik`, which selects
nothing — it is `vornik-companion`. See the companion guide.

**Agents stop spending tokens on work that cannot change the result.**

- The adaptive router step gets no tools and a fixed answer format. It used
  to be offered tools, and a correct first answer was re-prompted into
  exploring the workspace.
- The retry ladder re-runs only transient failures. A schema violation used
  to be retried with identical inputs.
- The prompt-token budget leaves room for the final answer instead of
  stopping a step after its research and losing the work.
- A tool result enters the conversation capped at 32 KiB, with the full
  output kept for `tool_result_read`, which can now page through it.

**MCP servers recover on their own.** A project MCP server whose first
connection failed (for example, its container was restarting) was dropped
until the next restart, and every call to it failed with "not connected". It
is now retried in the background, you are alerted if it stays down for 10
minutes, and the doctor's new `mcp_project_connections` row lists such
servers. An unreachable server also no longer holds the API closed at
startup, which closes the 2026.9.8 known issue; a step whose server is still
starting waits up to 10 seconds for its tools.

**New.** `vornikctl package upgrade` upgrades an installed extension package
file by file and refuses, with every reason, if you edited or deleted
anything it contributed. Each step that runs in a fresh (non-warm) agent
container now records that container's memory peak.

**Fixes.**

- Opening a live task page no longer makes other open pages show finished
  steps as running again.
- A config edit made while another reload was running is applied instead of
  silently skipped.
- SQLite lease expiry and task update-time comparisons are exact; mixed
  timestamp formats made lease expiry wrong by up to a second and task update
  times compare wrongly by up to a day.
- Chat replies no longer start with invented markers such as "[from your
  notes: not applicable.]"; the model had been copying them from its own
  earlier replies.
- A fact you deliberately save to memory from chat or a companion needs three
  words, not 64 characters.
- Secret findings in artifacts, webhooks, backlog deposits and container logs
  now appear in the task's scan history.
- The spend page labels memory reranking instead of showing a raw value.

**Known issues.**

- Ingesting HTML or EPUB files through the companion's RAG-ingest workflow
  stores markup and archive bytes rather than the extracted text.
- Deleting or retiring memory does not yet remove the knowledge-graph
  entities and edges that only that memory supported.

---

## 2026.9.8

**Claude Code no longer loses the vornik companion when the daemon
restarts.** Since Claude Code 2.1.282, every daemon restart dropped the
companion from an open session, and its tools stayed gone until you ran
`/mcp`.

The companion endpoint answered the MCP server-notification stream request
with the wrong status, so Claude Code kept re-opening the stream and marked
the server failed when a re-open landed during a restart. The endpoint now
answers that request the way the MCP spec requires (`405`), Claude Code holds
no stream, and a restart goes unnoticed between calls. Upgrade the daemon;
nothing changes on the client side.

**Fixes.** Tests added in 2026.9.7 that failed only in the Community Edition
tree now pass there.

**Known issue.** If one of your MCP servers is unreachable, a daemon restart
can keep the API unavailable for about a minute while startup waits on it.
The companion no longer disconnects over it; a fix is planned.

---

## 2026.9.7

!!! warning "Upgrade requirements"
    - **Rebuild or pull the agent image with this release.** Document, image,
      video and audio extraction and voice transcription and speech now run
      inside the agent image, never on your host. With an older image, PDF,
      OCR, video, audio and voice all report **"not available"** — there is
      no fallback to host programs. The image is about 1.8 GB; models are not
      included.
    - **Audio extraction changed engine.** It no longer runs the Python
      `whisper` command; it runs whisper.cpp with the model at
      `extractors.audio.model_path`, falling back to `voice.stt.model`. **With
      neither set, audio extraction is not available.**
    - `voice.stt.binary_path`, `voice.stt.ffmpeg_path`,
      `voice.tts.binary_path` and `voice.tts.ffmpeg_path` are now ignored. The
      daemon warns at startup when they are set; you can delete them.
    - **Host checks moved to `vornikctl doctor`.** The doctor in the UI and
      REST API now shows a single `host_checks` row telling you to run
      `vornikctl doctor` for the container and image checks.
    - **Stdio MCP servers can be added only by editing your config files on
      the host.** The admin UI, proposals and the project editors refuse them.
      HTTP MCP servers are unaffected.
    - Workflow changes applied **before** this release can no longer be rolled
      back from the UI; rollback refuses them and names the file. Changes
      applied from now on roll back as before.
    - If a project declares `dependencies:`, run
      `vornikctl deps install <project>` before its tasks run; until then its
      steps fail and name that command.
    - **Git:** a globally installed git-lfs (or any global filter driver) is
      no longer used by the daemon's git; run `git lfs install --local` in
      the project's workspace instead. The new `git_config_composition`
      doctor row lists anything dropped.
    - If a project's workspace cannot get an isolated worktree for a task,
      the task now fails with `WORKSPACE_UNAVAILABLE` instead of running
      unisolated. A git hook that breaks `git worktree add` is the usual
      cause.
    - Roles with `runtimePolicy: warm` no longer see the project
      workspace; use `ephemeral` for any role that needs it. Swarms
      scaffolded from the `basic`, `dev` or `research` presets had a warm
      lead; the daemon warns about them on load.
    - After upgrading, re-run the config asset install from the same release
      so the new `config_template_drift` doctor check has a current baseline.

**No request can make Vornik run a program on your host.** Only `vornikctl`,
run by you, starts host processes. Nothing that arrives through chat, the REST
API, MCP, A2A or a webhook can make the daemon run one — not even a fixed
command. Before this release, several paths could:

- An admin form, the "add MCP server" form, proposals and the project editors
  could register a command-line MCP server the daemon would launch. They now
  refuse to. Stdio servers are added by editing the config files on the host.
- The doctor ran container and system commands for its host checks, the
  support report included `podman info`, and diagnose read the system
  journal. These host checks now run only in `vornikctl doctor`, which
  merges them into the same report (pretty and `--json`). The support report
  no longer includes `podman info`, and diagnose no longer shows a journal
  tail.
- Applying or rolling back a workflow change, and deleting an artifact, made
  git commits. They no longer do: the change history is in the database.
  Apply now records the file it replaces and refuses if it cannot, so every
  apply can be rolled back; rollback restores that file to both config trees.
  The control-plane mirror still writes your source checkout — you commit it.
- `render_document` ran pandoc on the host. It now renders only in the agent
  image, with no network and no fallback, and gains a `docx` format.
- Uploaded PDFs, images, video and recordings, and every voice note, were
  processed by programs on the host. They now run in the agent image.
- Only one internal component may start a process at all, and it runs a
  closed set of five kinds: the agent container, control verbs on the
  daemon's own containers, git in a workspace, the git-over-HTTPS backend,
  and programs you named in your config files. Anything outside that shape
  is refused before it starts.
- **The daemon's own git no longer reads anything an agent can write.** Git
  runs programs named by the repository and its config: hooks, file
  monitors, and filter drivers named in `.gitattributes`. Several paths let
  an agent's workspace choose them. The daemon now refuses to run a task
  unisolated, pins the repository it works in, and reads a global git
  config composed from yours that keeps your identity and credentials and
  drops every driver. Every git flow (pull-request review, issue fixes,
  pushes, merge-back, autonomy refresh) was tested against real git before
  and after the change, and on a live repository before release.

**Every sandboxed tool run is bounded.** Each run has no network, a read-only
root filesystem, dropped capabilities, and a memory limit, CPU share, process
limit, timeout and input size per feature (render, PDF, OCR, video, audio,
speech-to-text, text-to-speech). Oversized input is refused before anything
starts. Output is capped per feature, and enforced while the tool runs: no
single file can exceed the cap, and the output directory is checked once a
second. That is a sampled bound, not a disk quota, so a run can overshoot by
about one second of writes before it is stopped. At most
`sandbox_tools.max_concurrent` runs (default 2) happen at once, with one slot
kept for voice so a batch of extractions cannot stall a voice note. Tune
per feature with `sandbox_tools.limits.<feature>`,
`sandbox_tools.timeouts.<feature>` and `sandbox_tools.cpus.<feature>`;
`sandbox_tools.max_input_bytes` overrides every feature's input bound. These
settings are read at startup, not on config reload. The startup memory check
counts the sandbox pool; only limits you set yourself can refuse a boot.
A new `sandbox_tools` doctor check runs each tool on a generated sample, and
four metrics cover it: `vornik_sandbox_tool_runs_total`,
`vornik_sandbox_tool_duration_seconds`, `vornik_sandbox_tool_wait_seconds` and
`vornik_sandbox_tool_remove_failures_total`.

**Re-ingesting a document now replaces its earlier versions.** Every re-ingest
used to add its chunks beside all earlier ones, all still live, so recall
could serve a months-old section of a document you had since rewritten. A
document is now identified by its path in its repository; the newest upload
wins and older versions are retired. Each version is stored whole, so an
unchanged section is never lost with the old version. Your agents' own output
is never affected.

- `/rag-ingest` in the companion plugin takes its scope from the files' own
  repository, not the directory you ran it in, and refuses an upload that
  mixes repositories.
- `vornikctl memory supersede-legacy-documents` retires the name-only versions
  ingested before this release, once a document has been re-ingested under
  its path. It is a dry run unless you pass `--apply`, and leaves alone any
  name two files share. On our reference deployment it retired 2,980 stale
  chunks across 69 documents.
- Documents over about 125 KB could not be ingested in 2026.9.6 — a size
  guard meant to protect an agent's context also blocked ingest, where no
  agent reads the file. Fixed.

**Companion plugins 0.26.0 (Claude Code) and 0.23.0 (Codex)** ship with this
release: repository-scoped `/rag-ingest` and `/upload`, and backlog deposits
that land in their own priority group.

**The agent-quality benchmark measured the wrong thing, and v9 fixes it.**
Tasks ask the swarm to write new files, but the workspace kept earlier runs'
output, so from the second run onward agents re-validated code that was
already written. Every task now starts from a clean workspace, and four tasks
are graded by acceptance tests the agents never see (reported alongside the
score, not used as a gate). Harness v9 results are **not comparable** with
earlier dev-swarm figures, including earlier baselines.
The first graded v9 arm ran on a local Ollama host: a 35B mixture-of-experts
model (about 3B parameters active) at 4-bit passed all four hidden acceptance
suites. That is one run per task, on small single-file fixes, at 23 to over 90
minutes per task. It shows a locally served model can land correct fixes on
this harness; it is not a claim about harder work or a statistical result.

**On a slow backend.** When `scheduler.speed_aware_timeouts` is enabled, the
speed factor now stretches step timeouts, not only the task lease, which never
limited anything. `chat.optional_work.disabled` and
`chat.optional_work.disabled_models` switch off optional LLM work (narration,
memory titles and summaries, reranking) for a model too slow to serve it
beside tasks. Optional work hitting its own short deadline no longer marks the
model unhealthy and fails your next tasks.

**New operator tools.**

- A **regulatory record** page lists every data-subject request and security
  incident, with those near or past their GDPR deadlines first, and AI Act
  disclosure counts per channel. It shows identifiers, deadlines and hashes,
  never personal data. Available in both editions.
- **`vornikctl leader-lock release`** clears an orphaned leader-lock row the
  doctor reports, and refuses a lock that is still held. The doctor no longer
  reports a lock nothing in your build would renew as an error.
- **`vornikctl deps install` and `deps status`** install a project's declared
  dependencies inside the agent image, from pre-built packages only.
- The **`config_template_drift`** doctor check tells you when a deployed
  config lacks a fix its shipped template has; `vornikctl doctor ack` declines
  one you do not want, and records who did.

**Chat can no longer cancel or retry a task on its own.** The model could
cancel or retry a task without asking. Now it proposes, and you confirm in
your own next message.

**Fixes.** The doctor's model routing check asks the live router, so it no
longer calls working models "unrouted". A model is no longer blamed for
failures caused by containers or mounts. `orphan_fk_rows` keeps your LLM cost
history instead of offering to delete it. `vornikctl doctor --json` no longer
drops fields. A step timeout above the workflow's `maxWallClock` now warns,
since it can never take effect. A cancelled task no longer overwrites a task
that finished at the same moment, and holds no lease. An explicit empty
`database.driver` now means Postgres, which turns on features that were
silently off under that setting.

---

## 2026.9.6

**One agent can no longer take the host down.** Agent containers ran with no
memory limit and no CPU quota at all, so a single runaway step was a
machine-level event rather than a task-level one. They are bounded now. The
limit is **derived from your host** rather than shipped as a fixed number: a
constant default beside the shipped `max_concurrent_tasks` would have refused
to boot on any host under 8GiB, which would have turned a load-dependent risk
into a certain outage caused by upgrading. Set `runtime.agent_memory_limit`
to override, or to `"none"` to keep the old unbounded behavior. A limit you set
that cannot arithmetically fit is refused at startup with the arithmetic in the
message; one that merely exceeds currently-free memory warns and boots.

**The adoption board stops reporting numbers it did not measure.** Two
customer-reported defects ([#14](https://github.com/grinco/vornik/issues/14)):

- Credentials were listed only if they had rows in three specific activity
  ledgers, so a key used for the REST API, the UI, chat or A2A was **absent
  from the board however heavily it was used** — and the board read as "nobody
  is using this". Rows now come from your credential list, with authentication
  itself as evidence of life. The same cause stalled the "last seen" column.
  The board also silently truncated to 20 rows; it now says how many it is not
  showing.
- A row could show a credential's LLM call count beside **"0 of 30 active
  days"**. The active-days column read one ledger while the counts beside it
  read another. It is now the union of all three: a credential that queried
  memory and spent on a model the same afternoon counts as one active day, not
  two, and the day boundary is UTC regardless of your daemon's timezone.

What is still not counted is now stated on the page rather than left to be
inferred: a credential whose only traffic is REST or the UI shows a recent
"last seen" beside zero active days, because a key's last-used timestamp is one
instant, not a history.

**A config tree your next restart would refuse is caught before it flaps.** A
deployed workflow naming a step error class the installed binary rejects takes
the daemon down on restart — and restarts it, forever, under
`Restart=on-failure`. `vornikctl doctor` now reports it, and the same check runs
against the **new** binary during `vornik-update.sh`, before the swap. Two other
doctor checks that could never have passed are fixed: `image_freshness`
compared a Community label against an Enterprise daemon, and `agent_image_uid`
outlived the constraint it tested.

**Community edition gains browser login** — a credential exchanged for a
bounded session, not a password. Revoking a key ends the browser sessions that
key minted.

**Our release pipeline was publishing inconsistently, and that reached you.**
Three repositories sat at three different versions on the same day, and the
Community edition had a `2026.9.5` tag with no release attached to it. The
causes were all the same shape — a step that worked because someone remembered
to do it. Creating the Community release is now automatic on a version tag, the
installers are checked against the version being released (they had been
pinned two releases behind for five days, so fresh installs quietly took an
older tree), and a weekly audit verifies each release actually produced the
versioned agent image it promised. Every release from 2026.7.2 onward now has a
matching tag and release entry.

**Fixes.** A step prompt of 128 KiB or more is no longer silently dropped.
`pricing.yaml` now reloads instead of reporting success and continuing to bill
the old rates — this one was called out in 2026.9.5 as restart-only. A registry
reload no longer drops a project the daemon is serving. A Slack command the
daemon does not answer is no longer invisible. The agent image runs under any
uid. Five dependency alerts on the Community repository are closed.

---

## 2026.9.5

**Your memory knows what is current.** Retrieval now ranks by freshness, and a
recurring series supersedes its own older members — so a daily digest answers
today's question instead of yesterday's. The decay half-life is read per row,
which lets one store hold fast-moving news beside reference material that never
goes stale. Nothing is filtered out; freshness is a ranking signal. Existing
rows are untouched: absent series membership means "not part of a series",
which is what everything written before this release is.

**Self-hosted inference stops being billed at a made-up rate.** If you run a
model through vLLM or a LiteLLM proxy, its cost metrics were almost certainly
wrong — an unpriced model falls through to a default rate, and the table
carried no prompt-cache tier at all. On our own benchmark that was a 7.3×
overstatement: $40.51 recorded against $7.12 actually billed. Both the proxy
and direct spellings of a model are priced now, because lookup is exact-match.

**A new doctor check, `pricing_drift`,** reports prices that disagree with a
pinned snapshot of LiteLLM's published table. It reports and never applies —
your operator-verified rate is often the better one — and it tells you how many
prices it could actually compare rather than implying it checked them all.
`vornikctl pricing sync-upstream` refreshes the snapshot and prints the diff.
Nothing fetches at runtime: prices drive budget enforcement.

**Fixes you may have hit.** Telegram commands did nothing in group chats
(`/cmd@botname` never dispatched), and everyone in a group was treated as the
same speaker. An agent path that lost its leading slash resolved nowhere. A
backtick in an `/upload` prompt aborted the upload silently, with no error and
no task id.

**Restart required for pricing changes.** `vornikctl config reload` reports
success on a `pricing.yaml` change it does not apply — the table is read once
at startup. Restart the daemon after editing rates.

---

## 2026.9.4

**Forge can read your CI, and it will not approve a red commit.** Turn on
`forge.ci.enabled` and Forge records what each Actions run concluded — the
conclusion, the jobs, the timing — against the commit it tested, and puts it in
front of the reviewer. A failed run triggers a review (six workflows finishing
together still produce one); a failure on a commit already reviewed gets a
short factual comment instead, written by no model. With
`block_approval_on_failure` (default on) `forge.post_review` will not submit an
**APPROVE** while a run it recorded for that commit is red — the review posts
as a comment led by the failing workflow's name. A commit with no recorded runs
is not treated as a failure. To get content in — a `terraform plan`, a test
report — have your pipeline upload it as an artifact and name it in
`ci.artifact_name`; Forge reads artifacts, never job logs. Your GitHub App
needs **Actions (read)** and the `workflow_run` event, asked for only when the
feature is on. You may name the review workflow as the success workflow, so a
*completed* run — not the push — triggers the review: a workflow that needs a
pull request runs only for runs that have one, and a green push to your
default branch is recorded without starting anything. See the Forge guide,
*Reading CI outcomes*.

**A reviewer no longer approves code it never read.** When a pull request's
head had not moved since its last review, the reviewer still ran — on a
one-sentence diff — and could post a confident review of a *different* pull
request as a real approval. `forge.post_review` now refuses to post a review of
an already-reviewed head, and a workflow can skip the model call entirely
because a system step can route on its own result. Two related fixes: an agent
now receives **every** preceding system step's output rather than only the
last (a second system step had been overwriting the diff), and a CI-triggered
review checks out the pull request's head rather than the base branch.

**Rate what an automated change produced, and see it where you approve.**
Thumbs up or down on any execution — `vornikctl execution rate <id> up|down
[--reason ...]`, or the control on the task page. Ratings roll up per skill
(`vornikctl knowledge rollup`, `/api/v1/skills/{id}/rating-rollup`, a column in
the skills admin) and appear on the Telegram card, the Slack blocks and the MCP
`skill_get`/`skill_list` an approver reads, so re-approving an edited skill is
informed by ratings on *that* body. Kept for `retention.execution_ratings_days`
(default 400).

**Boot no longer races the database.** The systemd unit waited only for the
network, so a reboot could start the daemon before the containerized Postgres
accepted connections and spin it through hundreds of restarts. The unit now
waits for `podman-restart` and for the database port (bounded, 120s), and an
absent database fails once with a message naming the cause. **The updater does
not rewrite unit files** — copy the new `deployments/podman/systemd/vornik.service`
into place and `systemctl --user daemon-reload`.

**`vornikctl doctor` reads better.** `fallback_rungs` now says whether a dead
fallback rung was *refused* by an open circuit (the breaker doing its job —
look at why the model's circuit opened) or *called and failed upstream* (the
model or its provider); the two were reported as one and pointed at
credentials for a model that worked elsewhere. And `doctor --offline` no longer
migrates the SQLite database it is diagnosing.

**Also fixed:** pausing a task read a stale status and could leave a container
running behind a `PAUSED` row — pause now asks the executor first and never
flips a running task it could not stop; deleting a project now evicts the
cached embeddings computed from its text; on SQLite, the entities view lists a
mention's chunks newest-first, as on Postgres; verifier refusals ("claimed a
file that is not on disk", "cited a commit that does not exist") are classified
instead of landing in `unclassified`; `install-enterprise` can replace a
running daemon (`Text file busy`); the public release tag is created by the
pipeline, so every release ships a version-tagged agent image; and the
Community repository's CI, red since the 2026-09-07 export on two tests that
only pass against the Enterprise tree, is green again.

**Upgrading:** migrations run on start; new keys are additive and default to
the previous behaviour, except that with `forge.ci` enabled a red pipeline
withholds approvals by default. Re-install the systemd unit as above.

## 2026.9.3

**If `vornik-update.sh` has ever reported "Checkout already at target commit.
Nothing to do." after a failed run, read this first.** The updater checked out
the new release while bash was still reading the script file, so on any upgrade
where the script changed size the run died just after the checkout — with a
confusing error like `line 200: F: command not found` — and everything after it
silently never ran: the binary build, the image refresh, the sidecar recreate,
the restart. Because the checkout had already moved, running it again reported
"Nothing to do" and exited successfully. An install that was never updated,
reporting success.

Check whether yours is affected:

```bash
vornik --version                    # the installed binary
git -C ~/vornik describe --tags     # what the checkout claims
```

If they disagree, or `vornikctl doctor` reports `image_freshness` drift, run
`./vornik-update.sh --force`. `--force` is needed precisely because the checkout
already matches, which is what made the failure invisible.

**Note that the fix cannot repair the update that delivers it** — your host runs
its own copy of the old script. Expect the first update to 2026.9.3 to fail on an
affected host, then run it again; the second run works. From then on the script
runs from a private copy, so the file it reads cannot change underneath it.

**The agent image is now pulled instead of built.** It is published to
`ghcr.io/grinco/vornik-agent`, and the updater fetches it **by digest** rather
than building a container on every host, so an ordinary update no longer spends
several minutes on it. Images that are not published — the cluster pair, and the
sidecar images — are still built locally, exactly as before.

**Air-gapped and offline installs are unaffected.** With no reachable registry
the updater builds locally as it always did. And a host that already pulled an
image and later loses its network **keeps that image** rather than rebuilding
over it: it cannot know the target, so it changes nothing and says so, and
`vornikctl doctor` continues to report the real state.

**`vornikctl doctor` now says how you obtained each image** — for example
`Obtained: 1 pulled, 6 built locally`. Neither an image's digest nor its build
label can tell you this after the fact, so the update path records it at the
moment it knows.

**Also fixed:** `vornik-update.sh --no-build` failed with
`VERSION: unbound variable` whenever an image needed refreshing.

## 2026.9.2

**The agent's file and git tools moved from bash into a small Go helper inside
the agent image.** Behaviour was recorded before anything moved and the port was
checked against that recording, so what a tool returns is unchanged for valid
UTF-8 content. You need the rebuilt agent image for this; the updater builds it.

**Four tool-boundary escapes found by a code audit are closed.** A dangling
symlink let `file_write` create a file outside the workspace, a planted
temporary-file symlink let `file_edit` overwrite one, recursive `grep` could
read one through a leaf symlink, and a git revision beginning with `-` was taken
as an option. All four are fixed, each with the reproduction as a test.
Exploiting any of them needed a role granted the tool plus a crafted argument or
a planted link; none is a container escape. Alongside: reads no longer allocate
the whole file before truncating, non-ASCII glob patterns match, `grep`'s
`head_limit` must be at least 1, and `read_many_files` counts characters again
and never cuts a multibyte character in half.

**If `vornik-update.sh` has been failing since late August, this fixes it.** The
updater could not build the agent image (`image build failed for
ghcr.io/grinco/vornik-agent:latest`) because of how it read the image list;
nothing was half-applied, but nothing updated either. Rerun it. It now also
recreates the long-running sidecar containers (the scraper, and any other
compose- or unit-run service built from a manifest image) when their image was
rebuilt — previously they kept running the old image after every update, and
nothing reported it. `--no-recreate-sidecars` opts out. The public agent image also gets
a version tag on GHCR from this release on (`ghcr.io/grinco/vornik-agent:2026.9.2`);
earlier releases only ever published `:latest`.

**You can read what a step was told and what it handed back.**
`vornikctl execution prompt <exec> <step>` shows the system prompt, user content
and tools the step's model saw at its first request; `execution input` and
`execution result` show the `task.json` the daemon wrote for the container and
the `result.json` it wrote back. All three are stored after secret redaction and
pruned with the step outcome that references them. A project can also opt in to
recording every model exchange of its steps (`recording: {llm_exchanges: true}`,
off by default because it costs storage) and export them with
`vornikctl execution exchanges <exec> <step> --export` — the recording a replay
test consumes.

**`vornikctl config show --provenance`** says where every resolved value came
from — file, environment, default or reload — and `--trees` indexes the registry
the daemon is actually running.

**Also in this release:** the Community `vornikctl support-report` collects the
bundle locally and states the edition it really is; `vornikctl doctor` runs every
check on SQLite; four SQLite-only persistence defects found by new shared
backend tests are fixed; a model fallback gets one attempt and a slow model can
trip its own breaker; pull-request review commands are gated on the author's
standing in the repository, and a push during a review is no longer lost;
`grpc` 1.83.1 closes a HIGH advisory.

**Upgrade notes.** Migrations 174–178 apply on start, all additive. Rebuild the
agent image (the updater does). The updater now recreates sidecar containers
before restarting the daemon; a tool call in flight against one fails at that
moment, inside the same window the restart already interrupts.

---

## 2026.9.1

**A workflow's system step now hands its output to the next agent.** This is a
customer-reported bug and it is the reason for this release. When a workflow ran
a system step (for example `forge.fetch_diff`) and the next step was an agent,
the agent received *empty* previous-step context — in every workflow, for every
system step that succeeded. The failure path already reported itself; only
success was silent. If you have an agent prompt that says "the previous step
provided X", it now actually has X. Injected output is capped at 256 KiB, and a
truncation says so inside the text the agent reads.

**Pull requests get reviewed again after you push.** Previously only
`pull_request.opened` triggered a review, so there was no way to get a fresh one
after addressing feedback. Reviews now also fire on **new commits, reopen and
ready-for-review**, and you can ask for one directly:

| Comment | Effect |
|---|---|
| `@<bot> review` | review now, covering what changed since the last review |
| `@<bot> full review` | review the whole pull request |
| `@<bot> pause` | stop reviewing this pull request automatically |
| `@<bot> resume` | resume automatic review |

A comment that mentions the bot without matching a command still gets an ordinary
conversational reply.

Three things keep the cost sane: a push burst **coalesces into one review** of
the final state, a re-review reads only **what changed since the last one**, and
drafts are not auto-reviewed. Every uncertainty resolves toward *more* review —
a missing baseline, a force-push that rewrote it, or an unreadable state row all
fall back to the complete diff, because a wrongly narrowed review silently omits
code nobody will look at again.

**Two things to set before you upgrade if you use the forge reviewer:**

- **Set your mention handle.** `mention_handle` defaults to this project's own
  App slug. You install your **own** GitHub App under your own name, so set it to
  *your* App's slug or commands will not match. An empty value matches nothing,
  deliberately — a handle that happens to be somebody's real account notifies a
  stranger every time anyone addresses the bot, which is a bug this release
  fixes rather than a naming preference.
- **Review volume goes up on active repositories.** On a project configured with
  `github_app:`, `github_app.auto_review_on_push: false` restores the previous
  first-review-only behaviour. On the generic `webhooks.sources` ingress that
  key did not exist in this release; use the per-pull-request `pause` command.
  *(Closed since: `forge.auto_review_on_push` applies on both ingresses, and
  the `github_app:` spelling now reaches both too. See the Forge feature page.)*

**Only people with standing in the repository can run review commands** — the
owner, organisation members, and invited collaborators. A review is real model
spend, so on a public repository an unguarded command is a way for a stranger to
run up your bill. Outside contributors are deliberately excluded.

**A permanent forge failure no longer burns the retry budget.** A pull request
that does not exist, a deleted one, a renamed repository or a revoked
installation used to consume all three attempts on identical 404s and then report
`UNKNOWN`, whose guidance is "try again". Those now fail immediately with a
class that says what happened. Rate limits and outages are unchanged — a 429 or a
5xx keeps its full retry budget.

**A review quotes the request it is answering.** Comments get deleted; reviews
are durable. A comment-triggered review now opens with an attributed quote of what
was asked.

**`vornikctl doctor` tells you more than it could before.**

- `pricing_coverage` used to report "all N models have pricing entries" while
  examining only swarm role models. It now also reads role fallbacks, project
  judge models, chat and router models, the embedding model, the narrator, wizard
  and fixit — and it reports **what it examined** instead of "all models". It may
  now surface gaps it previously could not see; an unpriced model was always
  silently inheriting the default rate.
- Model-circuit failures are classified as `MODEL_UNHEALTHY` instead of landing
  in the unclassified bucket, where they were the large majority of it. The
  `unclassified_step_failures` threshold is re-calibrated from 15% to 5% as a
  result — the old value could not fire once those failures were named. A
  migration reclassifies historical rows so the check is not stuck warning for a
  month while they age out.

**A sustained latency breach now proposes a change you can apply**, instead of an
observation telling you to go run Diagnose yourself. The proposal names a
specific model for a specific role and validates it against your configured model
universe before offering it. It deliberately does **not** pick the model from
latency data alone: the fastest model on a fleet is usually the smallest, and
that trade would swap output quality nobody is measuring for a number everybody
is. The rationale says so, so you read the trade-off before approving.

**Stored artifacts are no longer rewritten by heuristic secret rules.** The two
heuristic rules (entropy and generic key/value) could replace ordinary text —
test identifiers, hashed filenames — with `[REDACTED:…]` markers, and redaction
is one-way, so the original bytes were gone. Across two production surfaces those
rules produced roughly 7,000 findings and zero true positives. Detection is
unchanged; every finding is still found, counted and logged. Only the rewrite
narrows, and the log now distinguishes "found but not rewritten" from "nothing
found".

**Output-guard findings name the rule that produced them**, not just the
category — two different patterns can produce the same category, which was enough
to decide redaction but not enough to triage a false positive. The metric gains a
`rule` label.

**The agent container image is published for arm64.** It was being built for the
release runner's architecture only, while arm64 packages were shipping — so an
arm64 install named an image it could not pull and discovered it at the first
agent run.

*Correction, 2026-09-03:* this section originally also said packaged installs
carry an image provenance record again. The record's schema shipped, but the
packaging step that writes it aborted every build, so **2026.9.1 produced no
packages and no record**. Fixed after the release; the next release carries it.

**MCP audience checks accept equivalent spellings of the same URL.** Host case, a
longhand default port (`:443` on https), a trailing dot, and the Unicode form of
an internationalised host all used to mismatch, so a correctly configured
endpoint could be refused with "needs reconnect". Genuinely different endpoints —
a non-default port, the other scheme's default port, a different host — are still
different, and credentials embedded in a URL never count as the host.

---

## 2026.9.0

**`vornikctl doctor` no longer reports OK for a check it never ran.** Thirty
checks returned `OK` from paths that meant *not evaluated* — no database, no
config directory, metrics not wired. They now report `SKIPPED`. If your doctor
output gets less green after upgrading, nothing broke: those checks were never
running.

**Step `retry:` blocks now work.** If you have workflows with a `retry:` block
(`on`, `max_attempts`, `backoff`, `initial_delay`), it was being silently
dropped and the executor used three fixed values instead. It now takes effect —
so **review any `retry:` you have configured before upgrading**, because this
release is the first time it will actually run. Defaults are unchanged for steps
with no block. The `retryPolicy:` spelling never worked and has been removed;
`vornikctl workflow validate` will now tell you if you have it, along with any
other unknown key in a step — those used to be dropped in silence.

**Check your role permission keys.** A role written `permissions.allowed_tools`
instead of `allowedTools` used to parse cleanly and produce an *empty* tool
allowlist, which the MCP gate treats as "unrestricted". That misspelling is now
a load error. If a swarm fails to load after upgrading, this is likely why — and
that role has been running without its tool restriction.

**MCP tool results are now scanned for injection attempts and credential
leakage.** Detect-only in this release. Content from Jira, Sentry, scrapers and
other MCP servers was previously not scanned at all; only the daemon's own fetch
tool and the chat path were.

**MCP connectors that lose OAuth authentication now fail visibly.** A connector
whose token expired could 401 for hours while its tasks reported COMPLETED, with
the error visible only inside the task's result payload. Tokens are also now
refreshed per call rather than frozen at connection time. Additional hardening:
secrets are no longer replayed on redirects, insecure OAuth endpoints are
refused, and a grant issued for one resource path is no longer presented to a
sibling path on the same host — if you use a server whose audience is
path-scoped, reconnect it after changing its URL path.

**The documented update path now rebuilds container images by default.** If you
have been following `UPDATING.md`, you may not have received an updated agent
image for some time — the rebuild was opt-in. Parts of each release ship inside
that image, so an update that swapped only the daemon delivered half of it.

**The `image_freshness` check now works at all.** It previously compared two
different SHA formats and could never pass, on any host. It now reports honestly
whether your images and daemon were built from the same commit — and compares in
both directions, so a package upgrade you have not restarted into yet is
reported rather than passing silently.

*Correction, 2026-09-01:* this entry originally also said packaged (rpm/deb)
installs can verify their images against a record shipped in the package. They
cannot — the EE package does not ship that record, and did not in this release.
The record is built from images that exist on the machine doing the build, which
a cross-architecture package build does not have. On a packaged host the check
reports that it cannot verify, rather than reporting a pass. Hosts that build
their images locally (the quickstart and `make install-enterprise` paths) are
unaffected and get the full check.

**The agent image is published to GHCR**, so hosts pull it rather than building
it. Air-gapped hosts continue to build locally under the same name.

**New doctor check: `unclassified_step_failures`**, reporting the share of step
failures with no named cause over a trailing 30 days and warning above 15%.
Counts of fail-open events in the secret-redaction path are now published too,
so "no findings" can be distinguished from "never scanned".

**Agents recover better from two common failure modes.** A step killed for
looping now keeps its output if it had already satisfied its contract, instead
of discarding the work and failing the pipeline. And a call to a tool that does
not exist now returns the available tools and the nearest match instead of a
bare error.

**Benchmarks:** this release publishes the first scored agent-quality figures
(30 tasks, 100% task success, $0.74/task — with the 9.7% of steps that produced
no output stated alongside it), and restores the memory-benchmark track record
with an explanation of the axis change that broke it. See
[Benchmarks](../benchmarks/results.md).

## Earlier releases

One line per release. Upgrade requirements are kept in full.

### 2026.8.x

- **2026.8.9** — Invalid step results are stored as readable diagnostics so executions continue past the cause, delegations and checkpoints survive a deleted ancestor task, and right-to-erasure now also removes derived knowledge-graph data.
- **2026.8.8** — Telemetry's reporting URL changed (update egress rules), credential masking no longer breaks the output-field check that failed correctly-completed steps, and failed steps now carry the failure message and end of the log.
- **2026.8.7** — A tester role must now list the cases it checked and a step with a verified output file no longer fails for an empty summary, both measured to eliminate most retries; a daemon with broken config now stops instead of serving.
- **2026.8.6** — Bedrock roles with a declared output schema had been failing outright and now work; failed steps report their actual cause instead of a generic container-exit label; and agents are now shown the output rules they are judged against.
- **2026.8.5** — Retried step-spend was silently overwriting prior attempts (unrecoverable historically; fixed going forward), cached prompt tokens are now counted on streamed calls, and a new adoption dashboard shows how much of the product each team has used.
- **2026.8.4** — Agentic work on self-hosted open-weight models was non-functional because a `response_format` directive blocked tool calls and is now withheld when tools are offered; new optional `speed_aware_timeouts` and `default_step_timeout` settings.
- **2026.8.3** — Bedrock embeddings never computed a query vector so semantic search was silently keyword-only; embedding spend now appears in rollups; and `vornikctl bench memory` is a new retrieval-quality benchmark.
- **2026.8.2** — Slack direct messages were silently dropped when `channel_allowlist` was empty (affects 2026.8.0 and 2026.8.1) and are now exempt; the App Home tab now reflects actual channel access.
- **2026.8.1** — Four read-only agent tools (`memory_search`, `skill_fetch`, `get_conversation_window`, `summarize_thread`) were usable by every role regardless of its tool allow-list; a finished task's result could be permanently lost from its chat of origin.
- **2026.8.0** — Vornik can self-authenticate to remote MCP servers via OAuth 2.1, chat allowlists now fail closed by default, and a knowledge-graph extractor that silently discarded most of its input is fixed (run `vornikctl memory regraph` to reclaim marked chunks).

!!! note "New configuration keys"
    `telegram.allow_unlisted_users`, Slack `allow_unlisted_senders`. Both default
    to the safe value. See the configuration reference.

### 2026.7.x

- **2026.7.7** — The assistant can now see sent images, hear transcribed audio, and read video keyframes; GDPR Article 17 erasure and Article 15/20 data-subject tooling landed; and AI-disclosure notice now covers pull-request comments and third-party API posts.
- **2026.7.6** — Slack replies to top-level messages now land in the channel rather than a thread, channel-level Slack conversations now share one memory, and quoted email trailers are trimmed so the bot stops reading its own words as someone else's.
- **2026.7.5** — A security release from two audits, whose critical fixes closed command execution in the macOS installer and a symlink escape in archive extraction; anonymous usage telemetry is now on by default; and per-task spend caps plus parallel workflow steps.
- **2026.7.4** — Non-swarm LLM paths now fail over and recover automatically via `chat.router.model_fallbacks`; tool-budget refinements including a `standard` tier downscale; and reminders can run a task on a cadence and deliver the outcome.
- **2026.7.3** — A security hardening pass (path confinement, cookie `Secure`, bounded allocations), autonomy workspace-state hygiene with an updated auto-commit message, and an agent LLM circuit breaker matching the chat router's.
- **2026.7.2** — Automation Composer turns a plain-language description into a validated project (disabled by default), narrated execution shows task progress, and Fix-It Doctor opens failed tasks into a guided repair chat.
- **2026.7.1** — Large tool outputs stay out of the context window via compact handles, end-to-end project creation with a readiness page, and clearer Community/Enterprise edition boundaries including Memory Firewall in Community.
- **2026.7.0** — The product was renamed `swarmd` → **vornik** and `swarmctl` → **vornikctl** across commands, paths, env vars, and dashboards; edition-aware Community/Enterprise builds; and public docs at docs.vornik.io.

!!! warning "This release renames the product"
    `swarmd` is now **vornik** and `swarmctl` is now **vornikctl**. Service
    names, command names, config paths, environment variables, and dashboards
    changed. Review your automation for the `swarmd` → `vornik` migration before
    upgrading, and always take a backup first.

!!! note "Restart required"
    Restart the daemon after upgrading to pick up the new behavior.

### 2026.6.x

- **2026.6.1** — Workflows can be edited as a graph, project workspaces can be cloned and pushed over HTTPS with a per-project key, and `vornikctl support-report` produces a redacted self-contained diagnostic archive.
- **2026.6.0** — Vornik can plan a labeled issue, make the change across subtasks, review its own diff, and open a draft pull request; self-improving workflows replay-test proposed repairs; and GitHub login for the web UI.

!!! note "Upgrade"
    Restart the daemon to pick up the new behavior. Add `TimeoutStopSec=90s` to
    the systemd unit so the bounded shutdown has room to drain. Log forwarding,
    the support report, real-IP resolution, and git-over-HTTPS workspace access
    are all opt-in — existing deployments are unaffected until you configure
    them.

### 2026.5.x

- **2026.5.9** — The conversational project-setup wizard now works end-to-end and can be pinned to its own model; new installs default to zero-egress agent network isolation; and prebuilt binaries plus RPM/DEB packages ship per release.
- **2026.5.8** — Counterfactual replay re-runs a past task with one variable changed for side-by-side comparison, and memory items now carry policy metadata with advisory enforcement logging every retrieval by default.
- **2026.5.7** — A new landing page with activity tiles, a project template gallery, per-project home pages, plain-language failure banners, and retry-from-the-failed-step in the UI.
- **2026.5.6** — Runaway-loop protection caps adaptive routing depth, and adaptive routing no longer silently falls back to the default workflow but fails loudly instead.
- **2026.5.5** — Recall improved via richer embedding context, reranking, diversification, and TTL enforcement; new `vornikctl memory` commands reclassify, reembed, and list prune candidates.
- **2026.5.4** — Native AWS Bedrock support for chat and agents, faster interrupted-task recovery, and dashboards surfacing which models produce malformed output.
- **2026.5.3** — Declarative output schemas for agent roles eliminate a class of wrong-shape failures, with `vornikctl doctor` flagging mismatches; security hardening across container isolation and secret detection.
- **2026.5.2** — Failed tasks include a plain-language post-mortem explainer; web UI subtree now requires authentication and a cross-project access path was closed.
- **2026.5.1** — Cancelled tasks now show their accumulated LLM cost, and autonomy's duplicate-suppression matches by topic similarity rather than exact text.
- **2026.5.0** — A hallucination-detection pipeline flags unsupported claims across agents, chat, and autonomy; multimodal image input through chat; and secret redaction at every persistence point.

### 2026.4.x

The 2026.4 series built vornik from its first stable release into a capable
multi-agent platform. Notable user-facing milestones:

- **2026.4.14** — A typed agent tool set (`file_edit`, `grep`, `glob`, and
  typed git inspection tools) with per-role allowlists; live task-log tailing
  in the CLI and UI; signed webhook ingress with audit history; a project
  onboarding wizard; and a browser-based project YAML editor with validation.
- **2026.4.13** — Direct-API access to subscription-billed model backends
  (no CLI subprocess), a multi-backend chat router, a headless-browser fetch
  capability for agents, and an expanded `vornikctl doctor` with new schema,
  security, cost, and hygiene checks.
- **2026.4.12** — Per-task cost drill-down, local-timezone budgets, tool-call
  rate limits, a per-project data-retention sweeper, and `vornikctl backup` /
  `restore`.
- **2026.4.11** — First-class **LLM spend tracking**: per-step token and cost
  metrics, a per-project spend panel (24h / 7d / 30d / month-to-date with a
  per-role breakdown), and soft/hard USD budgets enforced before work starts.
  Introduced "effective cost" (spend per successful step) so a cheap-but-flaky
  model stops looking cheap. See [Cost and caching](../guides/cost-and-caching.md).
- **2026.4.10** — Per-user channel project scoping enforced on every action,
  per-project tool servers reached through the daemon (no per-agent
  credentials), and a deterministic git-backed workspace so "task completed"
  reliably means "output was saved."
- **2026.4.9** — Per-task git worktree isolation, a swarm-agnostic adaptive
  workflow that works with any swarm, a runtime autopilot on/off toggle, and
  lenient registry loading so one broken project no longer takes down the rest.
- **2026.4.8** — Per-role model overrides applied correctly to warm containers,
  an expanded `vornikctl doctor`, and additional conversation session controls.
- **2026.4.7** — **First stable release.** The autonomous development pipeline,
  durable task queue and scheduler, the multi-step workflow engine, the
  server-rendered web dashboard, and the conversational bot interface.

---

For configuration details on anything mentioned here, see
[Configuration reference](../reference/configuration.md) and the
[Guides](../guides/index.md).
