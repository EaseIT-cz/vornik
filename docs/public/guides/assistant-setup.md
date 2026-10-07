---
sources:
    - path: internal/cli/agent_connect.go
      sha256: 9b7c42ca54f447562aefb465ae5daefa1808de670a33e91125c26a68f5220bf6
    - path: internal/harnessconfig/harnessconfig.go
      sha256: 601958b2eed5b85284f43c80973d23b0bf9565a5d9e4db3fb42208ff69ea5984
    - path: internal/agentadmin/harness.go
      sha256: 330a8aaf3e48e5300155a68e10ae017b93d1e4acfb194622db080880cecb161e
    - path: internal/agentadmin/admin_guidance.md
      sha256: 840b2e11c52a4be9611c0d84b193b0c29d44c9eac744b07ff0d36c10792ffbf9
---
# Let your assistant set up Vornik

You can let an AI assistant (Hermes, Claude Desktop, Claude Code or Codex) build your automations for you through Vornik. For Hermes, start with [Set up Hermes with Vornik](hermes-setup.md): its default is the narrower broker setup, and this page is the opt-in. It can create projects and workflows, and connect your mail, bank, calendar or any other service that has an MCP server or a REST API. It then runs that work for you.

Vornik is the safety harness around it:

- **The assistant never sees a credential.** When a service needs a password, token or sign-in, the assistant asks Vornik for it. You enter the value, or sign in, **on your own phone**, and Vornik stores it where the assistant cannot read it.
- **Nothing that widens its reach happens without you.** Each of these is an approval request on your phone, worded plainly:
  - connecting a service;
  - every credential;
  - each new workflow, and each change to what a workflow returns or can reach;
  - a team member on a remote model, once per destination (the provider and its host): what that member works on is sent there;
  - a higher budget.
- **Models come from your operator's list.** A team member may use a model your operator listed for assistants (`agent_admin.models`); the assistant cannot pick any other. A model on this machine or your local network applies at once. Nothing else changes for a member given no model: it runs on the installation's default, which may itself be a remote provider. A local proxy that forwards to a remote provider counts as local, because Vornik sees only the proxy's address: whoever sets one up is responsible for what it forwards.
- **Automations run on a schedule only if you approve it.** When the assistant gives a workflow a schedule (for example 08:00 on the 1st of every month), your phone shows the schedule in words, its timezone and the fixed inputs each run gets. Any change to them is a new approval. A schedule runs at most hourly. A run whose time passed while Vornik was down is skipped, not made up later.
- **Writes are proposals.** A workflow that would send, pay, book or change something proposes it, and you approve each one before it happens. If the workflow declares it (and you approved that declaration with the workflow), the approval page also offers to approve future writes to the same recipient for up to 7 days, at most 20 times (your operator can lower both). Those later writes are sent without being shown to you first. Your phone's Standing approvals page lists each one with every write it sent, and pauses or revokes it with one tap; once a day you get a count of what was sent.
- **It stays in its own namespace.** Everything it creates is prefixed with its namespace (for example `hermes--finance`), and it cannot touch anything else: not another assistant's setup, not your own projects, not Vornik's settings.
- **Keys and tokens do not leak out through it.** Arguments, API requests, proposed writes and returned results are scanned for credential-shaped values. A finding refuses the call.

- **Ready-made workflows come first.** Vornik ships tested recipes: an inbox digest, an agenda and a morning brief (mail and calendar together). The assistant installs one into a project as a single change, which you approve once; your phone then asks for each credential it needs. Until a credential is entered the workflow is installed but does not run. A recipe's answer names its source for each item, says when the data was read, and tells "nothing arrived" apart from "could not read".

What it does receive is what you asked for. If you ask "summarise my spending", the summary of your spending reaches the assistant: that output was approved, by you, when the workflow was.

## Before you start

1. **Pair your phone** once, from the machine that runs Vornik:

   ```
   vornikctl pair-device
   ```

   Open the printed address on your phone and enter the code. Pairing needs HTTPS: the approval pages refuse plain http to any address except this machine's own (`localhost`), so serve Vornik over HTTPS (or behind a TLS proxy listed in `server.real_ip.trusted_proxies`), or pair on this machine through `localhost`. The phone is now what approves everything the assistant asks for. Without a paired phone, no assistant can be connected.

   **Do this yourself, never through the assistant.** The first phone is paired by the code alone, so an assistant that runs this command, or sees its output, could pair itself as the approver of its own requests.

   **If the phone later shows the pairing page instead of your approvals,** the page says why:

   - **The answer to its last approval did not reach it.** For example, you switched apps or lost the connection mid-tap. Run `vornikctl pair-device` again and enter the new code on that phone. It restores the same device; nothing needs revoking.
   - **Another browser used its sign-in.** A code alone does not restore it: a device you already use must approve it again. If you did not expect this, revoke the device first (`vornikctl devices revoke`).

   Vornik renews the phone's sign-in on every approval, so a copied sign-in stops working at your next approval. Every restore, and a browser that keeps using the phone's sign-in, raises an alert on your notification channel.

2. Make sure your `vornikctl` can reach Vornik with the operator's admin key, as for any other admin command (`VORNIK_API_KEY`, or `vornikctl auth login`).

Agent administration is on by default (`agent_admin.enabled`). On its own that opens nothing: an assistant can use it only with an agent key, which only you can mint, and only once a phone is paired.

It also needs the agent templates that `make install-config-assets` installs. If connect says Vornik does not offer agent administration, check `agent_admin.enabled` and run `make install-config-assets`; Vornik picks the templates up without a restart.

## Connect the assistant

Run this as the OS user the assistant runs as:

```
vornikctl agent connect hermes          # or: claude-desktop, claude-code, codex
```

Connect does five things:

1. It checks that Vornik offers agent administration and that a phone is paired.
2. It mints a key for the assistant's namespace. The default namespace is the assistant's name without dashes (`hermes`, `claudedesktop`, `claudecode`, `codex`); choose another with `--namespace`.
3. It stores the key in `~/.config/vornik/agents/<namespace>.key`. The file is readable by you only, and **no assistant config file ever contains the key**.
4. It adds one MCP server entry, `vornik-<namespace>`, to the assistant's own config. The entry runs `vornikctl agent mcp-bridge` with Vornik's address, which relays the assistant's requests to Vornik using the key file. Connect keeps everything else in that file, comments included.
5. It prints what it did and what to do next.

| Assistant | Config file connect edits |
|---|---|
| Hermes | `$HERMES_HOME/config.yaml` (default `~/.hermes/config.yaml`) |
| Claude Desktop | `~/.config/Claude/claude_desktop_config.json` (Linux), `~/Library/Application Support/Claude/claude_desktop_config.json` (macOS) |
| Claude Code | `~/.claude.json` |
| Codex | `$CODEX_HOME/config.toml` (default `~/.codex/config.toml`) |

Then **restart the assistant** and ask it what it can do with Vornik. It starts by reading `describe_installation`, which tells it the rules above and what you have already set up. For Claude Code and Codex, update the `vornik-companion` plugin first. Its `vornik-admin` skill carries the same guidance, and Vornik also sends that guidance over the connection itself.

An assistant reads Vornik's list of tools when its session starts. After you upgrade Vornik, or change the models offered to assistants, a session that was already open can still hold the old list and refuse an option Vornik now accepts. The connection tells the assistant when the list changes, and an assistant that supports this reads it again by itself. If yours does not, it asks you to reconnect: restart the assistant, or reconnect Vornik in its MCP settings.

`--dry-run` checks everything and prints what would be done, without changing anything.

If connect refuses because it cannot handle a config file (malformed JSON, or a Codex entry written in an unusual form), it says which file and changes nothing. Fix the file, or remove the old entry by hand, and run it again.

## Assistants that can run shell commands

Claude Code and Codex can run commands as your OS user. If Vornik also runs as **your** user, the assistant's shell could read Vornik's files directly (its secrets, its database and the assistant's own key), and no Vornik control can stop that.

So for these two assistants, connect tries to open Vornik's secret-store key as you:

- **Not readable:** Vornik runs as another OS user, and connect proceeds. This checks one file. It is good evidence of a separate user, but not a test of every file.
- **Readable** (including a fresh install where Vornik runs as your own OS user and has not yet created its store key): connect refuses. Run Vornik as a separate OS user (recommended), or pass `--accept-shared-user` to accept the risk knowingly.
- **Cannot be checked** (Vornik is on another machine, in a container, or the key you use is not an admin key; or Vornik runs as a separate user on a fresh install that has not stored a credential yet, so its store key does not exist): connect refuses unless you pass `--accept-shared-user`. In the fresh-install case, store any credential first and run connect again, or pass the flag knowingly.

Connect prints which guarantees hold in your case. With a shared OS user, the very first phone pairing is also not protected. Every pairing raises an alert, so a pairing you did not make is detectable, but not prevented.

Hermes and Claude Desktop reach Vornik only through the MCP connection, so this check does not apply to them.

## Vornik on another machine

The assistant can run on your laptop while Vornik runs on a server.

- Connect **on the laptop**, as the user the assistant runs as.
- Pass Vornik's HTTPS address with `--url`. Connect refuses plain HTTP to another machine. A tunnel or a reverse proxy with a certificate both work.
- `connect` needs the operator's admin key for this one command. Put it in that command's environment only, from a terminal the assistant does not control, and read it so it is not on a command line or in your shell history:

  ```
  read -rs VORNIK_API_KEY && export VORNIK_API_KEY     # paste the admin key
  vornikctl agent connect claude-code --url https://vornik.example --dry-run
  vornikctl agent connect claude-code --url https://vornik.example
  unset VORNIK_API_KEY
  ```

- **Which name to use for Claude.**
  - `claude-code` for Claude Code, including routines created in the Claude desktop app's **Code** tab.
  - `claude-desktop` for Claude Desktop chats, and Cowork tasks that run on your computer.
- **On a Mac,** download `vornikctl` for your Mac (`darwin-arm64` on Apple silicon, `darwin-amd64` on Intel) from the release, and check it against `checksums.txt`. It is not signed, so clear macOS's download quarantine before running it: `xattr -d com.apple.quarantine vornikctl`.
- **The shell check.** For Claude Code and Codex, connect cannot see from the laptop whether the assistant can read Vornik's files, so it refuses until you pass `--accept-shared-user`. Pass it only when the assistant has **no shell or SSH access to the Vornik server** as the user Vornik runs as. With that access, the assistant could read Vornik's secrets and database directly, and nothing Vornik does can prevent that. Remove that access, or limit it to an OS user with no access to Vornik's files, first.

## Assistants that run as a service

If the assistant runs as its own service user (for example a Hermes gateway under systemd), run `vornikctl agent connect` **as that user**, so the key file and the config entry belong to it. The bridge refuses a key file owned by anyone else and says so.

That user needs a `vornikctl` it can execute at a path that stays put, because connect records the path into the assistant's config, and the admin key for the one connect command, since your own sign-in lives in your home. [Set up Hermes with Vornik](hermes-setup.md#admin-setup-opt-in) shows both for Hermes; the same steps apply to any assistant under its own user.

## Answer Hermes's own approvals on your phone

Hermes has its own safety rules: when it wants to run a command they flag (a recursive delete, a force-push, a write outside its workspace), it asks you in its terminal or chat, and an unattended Hermes waits and then refuses. With Hermes connected and the plugin installed (version 0.7.0 or later), the phone you paired for Vornik can answer those questions too:

```
hermes vornik approvals on
```

This sets `security.approval.transport: vornik` in Hermes's config. From then on, each flagged command appears on your phone with Hermes's own description and the command (secrets masked), and you choose **Allow once**, **Allow for this session** (when Hermes offers it) or **Deny**. "Always allow" stays in Hermes's terminal: a permanent change to Hermes's safety rules is not something to approve in a few seconds on a phone. `hermes vornik approvals off` returns the questions to Hermes's own prompt; `hermes vornik status` shows whether the transport is on and which namespace answers.

What this does and does not promise:

- Hermes still decides and enforces. Vornik shows Hermes's text, records which phone answered and when, and hands the answer back. A modified Hermes, or one with the transport switched off, ignores the phone.
- Only a paired phone can answer. The assistant's own key cannot, and neither can an operator key or a console session.
- While it is on, an answer needs Vornik. If Vornik is down or unreachable, Hermes refuses every flagged command, unless you also set `security.approval.transport_fallback: builtin` in Hermes's config, in which case Hermes asks in its own prompt instead.
- The phone answers questions Hermes asks while you, or a chat gateway, are present. Hermes's single-query mode (`hermes -z`) and cron jobs never ask: they follow their own `approvals.*_mode` settings.
- At most 3 questions wait at once per assistant, and 30 an hour; beyond that Hermes is told no.

## See what an assistant has set up

The console's **Assistants** page (`/ui/admin/agents`) lists each connected assistant: its namespace, its key and when it was last used, its projects, and the requests waiting on your phone. Open one to see its projects, workflows (with their schedules and next run) and connections with their approval status, and its credentials by name and status. Credential values are never shown.

## Disconnect

```
vornikctl agent disconnect <namespace>
```

This revokes the assistant's key, removes the `vornik-<namespace>` entry from its config, and deletes the key file. Its projects, workflows and approvals stay, so connecting again picks up where it left off. After a disconnect, the assistant's next call to Vornik fails, and the message tells it to connect again.
