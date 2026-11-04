---
sources:
    - path: internal/api/middleware.go
      sha256: 8f51fec1e58343181a8981123f3eeb25cec855147f8e1e1f27d067dafa1a8f3e
    - path: internal/api/admin_class_auth.go
      sha256: 8f952ecfcc872c4f41b6541e48fa1c80ce79728d9eb95d14c7c27b96a8ab8c9b
    - path: internal/spawn/spawn.go
      sha256: b2764167a90d022278cccde6d60fb4f5663d2b12b64e644fda9f0b1cec5c0b0d
    - path: internal/sandboxtool/sandboxtool.go
      sha256: c377b12b2dd9764f508df8161deac6c57fd71396f7cebf81c29176f5fecdb068
    - path: internal/runtime/manager.go
      sha256: 682736916139aa3bf48466246051bd25f25d7c253a0ef12f59ed307aaf04dfaf
    - path: internal/secrets/secrets.go
      sha256: 34037e9a4065b1f33117b7695f1208abbb814f4f4e3358eefa7e2a3755b2661a
    - path: internal/netguard/netguard.go
      sha256: 7c26dd6c35779aa45022cdf5f7fa5f7d0809628de516b3a6fc21977e27dcf331
    - path: internal/outputguard/outputguard.go
      sha256: e5090fd8f664c75d4c8095b5b401c451d02a3663140db5e2b4d7fa9d348bcc0d
    - path: internal/egressscan/egressscan.go
      sha256: 39ab44bae5d6ab6e4b4029d5e882ffa92779cc70454f94f309fd72d5de7f4167
    - path: internal/approval/approval.go
      sha256: e5c042190f09de50edc949f67151c8f6cd2d8c5f5b834b5edc76ed38477d913c
    - path: internal/approverdevice/rotation.go
      sha256: 90ddcf37b46b733e31f4527d5fff6eef8aae66e6ce78d43124d34e4cd7eeced6
    - path: internal/brokeractions/worker.go
      sha256: f3279d54d93c11720f57b3a396afe03cf9cc9ff5ff08b85b68df9232d1c9220b
    - path: internal/mcp/mutating_tools.go
      sha256: c588041405728d918a9f038fb1fd7ecaeddda459bb77c6d91e503fb5b0ed0b96
    - path: internal/config/stdio_mcp_guard.go
      sha256: 666783c921544f0887e7cb35158f275cbf2860807b551d2aa434c4f932323179
    - path: internal/datasubject/erasure.go
      sha256: b673318643998ddd2f6d38531483b6b50c142b53819267f58463d3ffe9766052
    - path: internal/report/scrubbed_view.go
      sha256: 136859891e38a78230e8be5314f0d777514f1828dba1ecf311c849a749c9e6d1
---
# Security model

**Last reviewed:** 1 November 2026, against `main` after release 2026.11.1.

What Vornik defends against, its trust boundaries, and the controls in place
today, with what each does **not** cover. It applies to both editions;
Enterprise-only controls are marked **(Enterprise)**. Everything here is
product behaviour; where an operator's deployment choice (proxy, model
providers, host) changes the outcome, the page says so. To report a
vulnerability, see [Security policy](security.md).

---

## What Vornik is, for this purpose

Vornik is a daemon on a single host, with a control CLI (`vornikctl`). It:

1. serves an HTTP API and a web console;
2. runs AI agents in Podman containers, one container per task step;
3. runs untrusted media (documents, images, audio, video) through tools in
   short-lived sandbox containers;
4. talks to the outside: model providers, MCP servers, forges (GitHub and
   others), chat channels (Telegram, Slack, e-mail, web chat), and an
   optional third-party API gateway;
5. serves other AI assistants (Claude Code, Codex, Hermes, …) as a
   companion: a memory, a delegate for privileged work, and, if the operator
   allows it, an administrator of its own workflows;
6. asks a person to approve sensitive actions on a paired phone.

The trust boundary contains the daemon, its database and its configuration
directory. Everything else is treated as potentially hostile: network
clients, agent containers, model output, tool and web content, chat users,
connected assistants, and upstream services.

---

## Threat model

### In scope

| Actor | Can | Concern |
|---|---|---|
| Network attacker | reach the HTTP port | read or change data without a key; spoof a forwarding header; exhaust resources |
| API client with a key | call the API | reach another project's data; widen its own reach |
| Agent container | run model-directed code | escape its workspace; read another project's data; reach the host, the internet or the daemon's credentials; make the daemon run a program |
| Prompt-injected content | arrive in a web page, mail, ticket, document or tool result | steer an agent into exfiltration or an unwanted write |
| Chat user | write to a channel | trigger work they are not allowed; read a task's error text or secrets |
| Connected assistant (companion or agent-admin key) | call Vornik's tools | read raw private data behind a summary; act outside its grant; approve its own requests |
| Holder of a stolen approver device or its cookie | open approval pages | approve sensitive actions |
| Hostile upstream (model, MCP server, forge, gateway, vendor OAuth server) | shape responses | crash or hang the daemon; redirect a request with secrets; plant instructions |
| Crafted upload | be extracted or rendered | read local files, fetch URLs, or exhaust the host through a parser |

### Out of scope

- Physical access to the host, and root on the host.
- Escapes from Podman or the kernel. Vornik hardens what it starts but does
  not defend against a container-runtime escape.
- Network-level floods (DDoS). There are per-request limits, not traffic
  shaping.
- People who may write the configuration directory. Operator configuration
  is inside the trust boundary (see Residual risks).

---

## Trust boundaries

```
 network client ──▶ HTTP API / console ──▶ database (trusted)
 (key, session,          │
  approver device)       ▼
                    Vornik daemon ──────────▶ model providers, MCP servers,
                    (scheduler,                forges, chat platforms, gateway
                     executor, guards)         (outbound, untrusted responses)
                     │            │
         podman run  │            │ podman run (one-shot, no network)
                     ▼            ▼
             agent container   sandbox tool run
             (untrusted)       (untrusted input)

 connected assistant ──MCP──▶ companion endpoint (scoped key) ──▶ daemon
 paired phone ──────────────▶ approval pages (device-bound session)
```

---

## Controls

### HTTP API and console

- **Authentication on every route except a short public list:** liveness
  and readiness probes, `/metrics` (hidden on a managed instance's public
  listener), the login page, static console assets, the A2A agent cards, and
  the EU AI Act Art 50(2) detection routes. The quickstart configuration
  requires an API key. Credentials are checked by one ordered chain
  (session, signed webhook, database keys, static keys) that stops at the
  first refusal. Key comparison is constant-time.
- **The detection routes are read-only and say little.** Only three exact
  shapes are public: `/provenance/<64 lowercase hex>`, `/provenance/verify`
  and `/.well-known/vornik-ai-mark-keys`. They answer whether exact content
  (by SHA-256) was generated by this instance and on which day, whether a
  mark is genuine, and which public keys sign marks. They never return
  content, a task, a project or the channel, and a failed verification does
  not say which part failed. A hash must be known to ask, so the ledger
  cannot be enumerated. Public routes skip the general per-IP limiter, so
  each applies its own: 5 requests per second per address, burst 10.
- **Admin class is decided once per request.** One decision function governs
  the operator surface (configuration, reloads, daemon-wide repairs); a
  second decides who may read error text and logs (operator, scrubbed, or
  denied, with denied as the default).
- **Per-project scoping** on every project resource. A project-scoped key
  that asks for another project's object gets "not found", not "forbidden",
  so existence does not leak.
- **Agent keys reach only the agent route set**, on every listener (TCP and
  the unix socket), and cannot post as the operator.
- **A database key's flags bind it on every route.** A key minted with
  `vornikctl key create` reaches its project's memory only with
  `memory_read` (reads) or `memory_write` (writes, including document
  extraction into memory). With a workflow list it reaches only the A2A
  routes of those workflows. It cannot create, list or change keys: key
  management is for static keys, admin keys and the console.
- Request bodies are size-capped and strictly decoded. The configuration dump
  redacts every credential-shaped field; a test plants a marker in every
  string field of the configuration and requires each credential-shaped one
  to come back redacted.
- Same-origin checks on every state-changing console request, through one
  shared gate; `Secure` cookies on HTTPS.
- **A content-security policy on every console response.** It admits only
  this origin for scripts, and an inline script only by the SHA-256 of its
  exact text, computed at start from the embedded templates (a console whose
  hashes cannot be computed does not start). There is no `'unsafe-inline'`
  or `'unsafe-eval'` for scripts, no inline event handler, and htmx neither
  evaluates attributes nor runs scripts from swapped HTML. What it does not
  cover: a hash admits that exact text anywhere on the console, so an
  injection could replay one of the console's own scripts in another place;
  inline styles remain allowed.
- **Real client IP behind a proxy** is taken from the configured header only
  when the immediate peer is in the configured trusted-proxy list; otherwise
  the peer address is used. A trusted-proxy entry wider than one address logs
  a warning at startup. Rate limits and login lockouts key on this value.
- **Managed hosting** (`hosting.mode: managed`, for hosted instances; off by
  default) splits the daemon into a public listener and an operations
  listener. The public listener refuses every admin-class key, in any header
  it can be presented in, before routing, serves no metrics, and treats an
  administrator's browser session there as the instance owner, who may use a
  short, closed list of operator surfaces (the project wizard, the template
  gallery, MCP OAuth connections, the operator audit) and no other;
  administrator access exists only on the operations listener, which must
  bind a loopback or private address. Managed mode refuses to start with authentication off.
  It also requires the per-IP request limit and a narrow trusted-proxy list,
  starts no stdio MCP server, limits model endpoints to public addresses
  (except the listed model gateway names, which may resolve to private
  addresses but never to loopback or metadata addresses), and limits every MCP
  server it dials to public addresses and same-host redirects.
- **Binding a managed instance to its customer.** A managed instance gets
  its owner and models from one setup operation on the operations listener,
  never from the public one.
  - **Idempotent, with a fixed owner.** A repeat writes nothing; another
    owner is refused, and so is another issuer or client id once an owner
    is bound. Only the current `instance_admin` key may call it.
  - **Checked before it writes.** It probes the issuer's discovery and
    requires every listed model to be served by a gateway named in
    `hosting.model_hosts`.
  - **The owner's login is OpenID Connect against the hosting service's
    issuer:**
    - PKCE (S256) and a nonce on every sign-in, held in a cookie bound to
      that sign-in's state;
    - an ID token checked for its signature against the issuer's published
      keys, issuer, audience (and `azp` when the audience names others),
      expiry, issue time and nonce;
    - the issuer reached only through the outbound guard, with every
      response capped at 1 MiB.
  - **Replacing the operations credentials.** The `instance_admin` key and
    the metrics scrape token are replaced through an overlap: the old and
    the new are both accepted until the old is retired.
    - The new value is returned only sealed to the caller's post-quantum
      hybrid (X-Wing) key.
    - Only the current `instance_admin` key may rotate or retire: the one
      the config file lists, not the previous one, and not a key retired
      but still accepted until the restart. So a stolen old key cannot lock
      out the current one or mint a new one.
    - A change takes effect at the restart the control plane performs.
  - **What it does not cover:**
    - a stolen **current** `instance_admin` key can rotate it away (it is
      the instance's administrator credential by design);
    - authorization-code single-use is the issuer's job;
    - models from the hosting service's gateway are classed "provided" and
      need no per-destination approval, on the strength of the owner's
      sign-up consent and the service's terms, which the instance cannot
      check itself.
- **Operating a managed instance.** These run on the machine as root, or on
  the operations listener; none is reachable from the public listener.
  - **Upgrades:**
    - **Refusals before anything changes.** An upgrade refuses an unsigned
      package (the signature is checked on a private copy of the file and
      enforced again by the package manager at install), a downgrade, and an
      unaccepted licence version.
    - **Order.** It waits for idle, stops, snapshots, installs and starts.
    - **Rollback.** A failed start rolls back by itself. Once the new
      version has started, the upgrade is final, because it may already
      have accepted writes.
    - **A newer database is refused.** A daemon refuses to start on a
      database a newer release migrated, so an older binary never runs
      against a schema it does not know.
  - **Backups.** Every backup archive is readable by its owner only. A
    *restorable* backup also carries the secret store's key; it is as
    sensitive as the secrets. A restore writes secrets only into the
    directory provisioning created and never through a link inside it.
  - **The owner export:**
    - **Contents.** It leaves out the secrets, redacts the configuration,
      and drops the rows of every table holding keys, tokens or sessions.
      A test classifies every table, so a new one cannot reach an export
      unexamined.
    - **Who downloads it, and from where.** Only the instance owner, signed
      in, on the public listener: not the hosting service's administrator
      key, not another role. The link carries no credential.
    - **What is served.** An object the daemon names from its own instance
      id, for 7 days from its registration. An unknown or expired id gets
      the same answer, and at most two downloads run at once.

### Configuration integrity

- Every request-derived name joined into the configuration or workspace tree
  (project, swarm, workflow, schema, file) is cleaned and confined under its
  root by one path helper; separators, `.`, `..` and empty names are refused.
- Every read-modify-write of the daemon configuration file goes through one
  file lock, so two subsystems cannot overwrite each other's changes.
- Applying a configuration change records the deployed file it replaces
  first, so every apply can be rolled back; an apply that cannot record it is
  refused. No request commits to git on the host.
- The configuration assistant sees a redacted snapshot: secret-named values
  and lists are withheld, and a file it cannot screen fully (a secret reached
  through a YAML alias, more than one YAML document) is refused rather than
  shown.

### Identity, chat channels and who sees what

- People are user accounts; API keys, chat identities (Telegram, Slack,
  e-mail) and devices map to accounts. Single sign-on via OIDC is
  **(Enterprise)**.
- Chat channels have sender allowlists that fail closed when empty (a Slack
  direct message is gated by the sender list, not the channel list).
- **Non-operators see scrubbed task text.** Task errors, status text, input
  prompts and output excerpts reaching a chat user who is not an
  admin-linked operator in a private chat are passed through a scrubber that
  withholds secret-bearing lines and replaces e-mail addresses, home paths,
  IP addresses and the host name. The same rule applies to the chat model's
  status tools.
- Every AI-generated chat reply carries the EU AI Act Art 50(1) disclosure
  for its channel; a turn whose disclosure cannot be delivered is refused.

### Approvals and approver devices

- Sensitive actions (an assistant's new connection or credential, a
  widening of its reach, a proposed write) are approved on a **paired
  phone**, not in the requesting channel. Pairing is started by the operator
  from `vornikctl`.
- A device's session cookie rotates on every decision; a copied cookie dies
  at the device's next approval, and a browser riding a device's approvals
  raises an alert. Restoring a device is an operator action and is alerted.
- An approval binds to the hash of the exact arguments shown, computed over
  a canonical form that refuses duplicate keys, so the approver cannot be
  shown one thing and approve another. Choice cards accept only a choice.
- Approval notices pushed to a channel carry no content, only a link.

### Agent containers

- Every container runs with all Linux capabilities dropped,
  `no-new-privileges`, a non-root user, and labels for discovery and reaping.
  Only the pinned agent image may be started. The scripts that drive the
  container are owned by root, so an agent cannot rewrite them.
- **Read-only root and a PID limit** (`runtime.agent_read_only_root`,
  `runtime.agent_pids_limit`; off by default, always on in managed hosting).
  With the read-only root, an agent writes only its mounts, `/tmp`, `/var/tmp`
  and its home (disk-backed volumes removed with the container) and two small
  bounded tmpfs (`/run`, `/dev/shm`); the PID limit bounds a fork bomb. A
  periodic sweep removes containers, and their volumes, that a crash left
  behind, only once their task has finished. Without the settings, the PID
  limit is the host podman's own default.
- **Network policy per role.** The default is `daemon-only`: no network
  device at all, with the daemon reachable over a unix socket for tools and
  model calls. Roles that need the internet opt in to an isolated `egress`
  namespace; `host` networking is available but discouraged.
- **Per-task credentials.** Each task step gets its own short-lived,
  project-bound key. If minting it fails, the step is refused; it never falls
  back to a static or provider key.
- **Workspaces are git worktrees.** Daemon git commands pin the git directory
  and work tree, read a global git config composed by the daemon (hooks,
  filter drivers and `core.hooksPath` dropped), and never read state an agent
  can write. Warm containers mount no project. A task the daemon cannot give
  a worktree fails before any container starts.
- Inputs are staged only from the task's own project; outputs are harvested
  through one pinned directory handle, so a running agent cannot swap files
  under the daemon. Symlinks that leave the workspace are refused.
- **Credentials stay out of containers.** Third-party API credentials live in
  the gateway, MCP credentials in the daemon, and an agent's tool arguments
  carry none.

### No daemon surface chooses a program

- Exactly one part of the daemon may start a process, and it runs a closed
  set of program kinds: the pinned agent image (with an allowlist of flags),
  container control verbs, git under a registered workspace root, the git
  HTTP backend, and a stdio MCP server or CLI model provider that only the
  configuration loader can define. Each kind parses its argv against a
  closed grammar before a process exists.
- No request, UI form or configuration write from a request may add or alter
  a stdio MCP server. Host diagnostics (podman, systemd, journal) run only in
  `vornikctl doctor` on the host, never in the daemon.
- An architecture test fails the build when a daemon-linked package gains
  the ability to start processes outside that one place.

### Untrusted media: sandbox tool runs

- Extraction (PDF, OCR, audio, video), document rendering, and voice
  (speech-to-text, text-to-speech) run as one-shot containers of the pinned
  agent image: no network, read-only root, a small tmpfs, a pids limit, all
  capabilities dropped, inputs mounted read-only, and per-feature memory,
  CPU, time and input-size limits. Inputs over the limit are refused before
  a container starts. Output is bounded by a per-file size limit plus a
  once-a-second sample, not a disk quota.
- Renderers cannot read local files, fetch, or run script. `ffmpeg` reads
  only the uploaded file, through a file-only protocol list and a short list
  of permitted formats.
- Archive extraction is guarded against zip bombs, oversized expansion,
  too many entries, and symlinks that escape the destination.

### Prompt injection and untrusted content

- **Third-party content is scanned on the way to a model**, including MCP
  tool results, for injection attempts, credential leakage and encoded
  payloads. Findings name the rule that produced them.
- Content from outside is wrapped in an untrusted-content envelope whose
  close tag the model can see; the wrapper fails closed on malformed or
  over-encoded input.
- **Taint lineage.** Each autonomous step records whether it consumed
  untrusted content. In `enforce` mode, a write by a tainted task parks for
  operator review with its sources attached. The default mode is `advisory`.
- **Memory ingest gates** run on every deposit: secret scan (a mostly
  redacted "secret dump" is rejected outright), per-project deny patterns
  (substring, not regex), optional injection detection, dedup and size
  checks. A memory firewall attaches sensitivity, role, purpose and expiry
  policy to chunks and audits every retrieval decision. A connected
  assistant's deposits land unverified and low-confidence, and its
  corrections cannot overrule an operator's.
- Learned skills are never drafted from goals whose words came from a third
  party (webhook, forge, chat or A2A).

### Secrets

- A secret detector (known token shapes plus an entropy heuristic) runs at
  every persistence and display boundary: task results, tool audit,
  container logs, artifacts, transcripts, chat notices and memory.
  Credential-shaped findings are redacted; inbound webhooks with a secret are
  blocked. Heuristic findings are counted but no longer rewrite stored
  artifacts, because they produced no true positives in production.
- Results are validated before redaction, so redaction cannot corrupt a
  result into a schema failure; everything stored or forwarded is the
  redacted form.
- **Outbound tool arguments are scanned too.** For an assistant-administered
  project a credential-shaped value in a tool argument blocks the call; a
  refusal names the field, never the value.
- `vornikctl secrets scan-history` rescans stored history (tool audit, step
  prompts, transcripts, task messages) and redacts what it finds.
- Credentials an assistant provides are sealed at rest with a key file the
  operator must back up; the doctor checks that the key opens them.

### Outbound connections and SSRF

- Every outbound client has a timeout and a read cap.
- A dial guard refuses loopback, link-local, private, special-use and
  reserved ranges, including IPv6 transition forms that embed a blocked IPv4
  address (NAT64, 6to4, IPv4-compatible), and the cloud instance-metadata
  addresses (IPv4 and the IPv6 ones in private ranges) even where private
  ranges are allowed. A hostname is judged by the address it resolves to, and
  guarded clients ignore proxy environment settings. One list decides for
  every guard. An operator-listed allowed host is an explicit vouch and
  bypasses the list, metadata addresses included.
  Operator-listed private ranges are allowed only where a feature needs them.
- Redirects that would carry secrets are refused; gateway redirects stay on
  the same origin.

### MCP servers

- **OAuth 2.1** for remote MCP servers: discovery, dynamic client
  registration, PKCE, resource indicators, refresh. A stored grant is bound to
  its resource audience, so a token minted for one path is not presented to a
  sibling path. Token requests do not follow redirects. Write scopes for
  daemon-wide servers must be named in configuration, and the CLI says what
  it withheld.
- Every stored OAuth token is sealed at rest with the secret store key file,
  bound to its project, server and field, so a value copied to another row
  does not open. A token found unsealed in the database is never served, and
  the doctor reports it.
- A server can be required to declare its tools; it is then refused when it
  advertises a mutating tool that is not in its allowlist.
- A configuration reload that removes a server cannot be undone by a
  connection that was still dialling.
- **On a managed instance, sign-ins go through the hosting service's OAuth
  router**, which forwards an authorization code only to the browser of a
  signed-in member who started that flow:
  - every flow starts at the router;
  - the authorization response's issuer is checked (RFC 9207) before any
    token is requested;
  - a provider whose metadata names another issuer than its own address
    is refused;
  - a grant wider than requested is refused, and the token revoked where
    the provider allows it.

  A self-hosted daemon checks the issuer where the provider supports it,
  and logs instead of refusing the last two cases, so working connectors
  to non-conforming providers keep working.

### Assistants: the companion, the broker and agent administration

- A companion key is bound to one project, optionally to a list of
  workflows and a budget, and may use only the companion endpoint. A memory
  key can be minted without any task tools.
- **Broker projects** let an assistant ask for privileged work (read my mail,
  check my calendar) without ever holding the credentials or seeing raw
  data. A broker workflow takes typed, bounded inputs and returns one
  schema-checked, scanned document; error text is reduced to a closed class.
- **Writes are proposals.** A broker workflow may only propose a write; a
  person approves the exact arguments; a single worker performs it once and
  never retries, recording "unknown" when the outcome is uncertain. Writes are
  off until the operator enables them. Standing grants for repeated writes
  are bounded in days, uses and number.
- **Agent administration** lets an assistant build projects and workflows in
  its own namespace. Its key lives in a file only the user can read, read by
  a bridge command, so no assistant configuration holds it. For an assistant
  that can run shell commands, connecting checks that Vornik runs as a
  different OS user. Every credential and every widening of reach is approved
  on the phone; the assistant never sees a credential. Refused calls are
  recorded and shown.
- Completion pushes to an assistant carry only ids and closed states, and
  refuse loopback, link-local and unlisted private addresses.

### Data protection

- **Retention** windows per data class, with floors for audit evidence
  (tool audit is kept at least six months, as the AI Act requires deployers
  to keep logs) and an AI-disclosure log that is never pruned.
- **Data-subject rights**: a register of whom a record concerns,
  identity-verified access and portability reports, erasure with a preview,
  and a breach ledger with the 72-hour clock. Exclusive records are deleted;
  records that also concern other people are redacted, not destroyed, and
  re-embedded. In a multi-person conversation transcript, the messages the
  person wrote are replaced by a dated removal note under a pseudonym, and
  their known identifiers are masked in everyone else's messages and in the
  instructions of tasks those messages started; nobody's words are rewritten
  by a model. What remains of the person in others' messages is kept only on
  a ground the operator assesses and records for each request. A chat
  channel's stored conversation is deleted or masked the same way (deleted
  in full where the channel does not record who wrote each message), and every daemon node checks a version on the stored row before
  serving its cached copy and saves only over the version it read, so an
  erased conversation is neither served from memory nor written back. A record
  a running task still uses, a conversation that keeps changing during the
  erasure, and a session held only in memory (single-process installs) are
  deferred, and the request is reported as not fully actioned rather than
  marked done.
- **AI Act Art 5**: the vision agent refuses identification, emotion
  inference and inference of sensitive characteristics.

### Supply chain and releases

- Release and Community-export builds start from empty module, build and
  tool caches and run `go mod verify` before building. `govulncheck` and CodeQL
  run in CI.
- Only the pinned agent image may run. An image provenance record lets the
  daemon compare running images to a release; a packaged host without the
  record reports that it cannot verify rather than passing. Locally built
  images are recorded by source commit only, because their digests differ
  per host.
- The Community export is scanned for leaked private material before
  publication.

### Availability

- Request size caps, per-project rate limits on internal endpoints, bounded
  queues that drop and count rather than block, bounded mail polling, and
  panic recovery around provider SDK calls.
- Log forwarding with TLS and CA pinning is **(Enterprise)**.

---

## Residual risks

1. **Operator configuration is trusted.** Whoever can write the configuration
   directory can point Vornik at any model, MCP server or channel. Protect it
   like `/etc`.
2. **Model endpoints may be private.** Configured chat and embedding
   endpoints dial through the guard with private ranges allowed, so a local or
   LAN model server works: link-local addresses (the cloud metadata service)
   and redirects to another host are refused, but a configured endpoint can
   reach any loopback or private address. Use network-level egress filtering
   if configuration is partly delegated.
3. **Rate limiting is per feature, not global.** The general API's per-IP
   limit (`api.rate_limit.per_ip`) is off unless configured; managed hosting
   requires it. Without it the API relies on a small set of authenticated
   keys. IP-based defences are only as good as the trusted-proxy list.
4. **Prompt injection is mitigated, not solved.** Scanning detects and
   wrapping isolates, but a model can still be misled; the structural
   defences are scoped keys, schema-bound outputs, approvals for writes and,
   in `enforce` mode, taint parking.
5. **Approvals are only as good as the approver.** An approver who approves
   without reading defeats the gate; nothing in software prevents that.
6. **Sandbox output is bounded by sampling**, so a run can overshoot its
   output cap by up to one second of writes.
7. **Marks on AI-generated content can be stripped, and an edited copy is
   not recognised** (EU AI Act Art 50(2)). Marks are signed metadata and a
   ledger keyed by the exact bytes; there is no embedded text or audio
   watermark, so synthetic voice replies are off by default. Details:
   [AI transparency](ai-transparency.md#how-ai-generated-output-is-marked).
8. **Image signatures are not verified.** Pin images by digest or use a
   private registry with content trust.

---

## Reporting

See [Security policy](security.md) for how to report a vulnerability and
which versions are supported.

<!-- summary-reviewed-through: 2026.11.1 -->
