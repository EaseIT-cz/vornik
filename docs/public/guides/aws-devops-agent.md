# Use Vornik from AWS DevOps Agent

AWS DevOps Agent investigates incidents in your AWS accounts. Connect it to
your Vornik and an investigation can do three more things:

- **Recall your own incident memory.** Before a root-cause analysis, it asks
  Vornik what is already known about the service: earlier root causes,
  runbooks, decisions. The memory stays in your Vornik, and your engineers'
  Claude Code and Codex sessions read the same memory.
- **Read approved procedures.** Knowledge skills an operator approved in
  Vornik reach the agent, the same ones every assistant connected to Vornik
  gets.
- **Write back and hand off work**, if you allow it. The agent stores its
  finding with `remember`, or delegates research or a summary to a Vornik
  workflow. The workflow runs in a sandboxed container, on the models you
  configured, within the key's budget.

The connection is Vornik's companion MCP endpoint, listed in the DevOps Agent
console as the **Vornik** partner connector. Vornik is self-hosted: the agent
calls your daemon, and Vornik sends nothing to EaseIT Labs.

## What you need

- A running Vornik, reachable over HTTPS by DevOps Agent (see
  [Reach Vornik](#reach-vornik)).
- `vornikctl` on the Vornik host, to mint the key.
- An AWS DevOps Agent Agent Space, and permission to register a capability
  provider in its account.

## Make a project and a key for the agent

Give the agent its own project, so an investigation's notes do not mix with
a developer's until someone links them:

```bash
vornikctl init project --template companion devops-agent \
  --config-dir "$HOME/.config/vornik/configs"
vornikctl config reload
```

Then mint a key. Pick one of two profiles.

**Read-only (recommended).** AWS advises allowlisting only read-only tools:

```bash
vornikctl companion grant --project=devops-agent --client=aws-devops-agent \
  --label="agent-space/<name>" --memory-read --skill-read --no-delegate \
  --expires=2027-03-01
```

This key is offered `recall`, `recent_memory`, `memory_list`, `list_scopes`,
`skill_search`, `skill_get`, `skill_list`, `whoami` and `report_problem`, and
nothing that writes to memory or starts work.

**Read and write back.** Add memory writes and two workflows, under a budget:

```bash
vornikctl companion grant --project=devops-agent --client=aws-devops-agent \
  --label="agent-space/<name>" --memory-all --skill-read \
  --workflows=companion-research-gather,companion-report-summarize \
  --budget-usd=10 --expires=2027-03-01
```

This adds `remember` and `memory_correct`, and the task tools: `delegate`,
`status`, `result`, `task_logs`, `task_explain`, `cancel`, `list`, `catalog`,
`list_artifacts` and `read_artifact`.

**On a Vornik-hosted instance** you have no shell. Open **Connect your
companion** (`/ui/companion`) and choose **Connect AWS DevOps Agent**. It
mints a key with the read-only profile in your instance's `companion`
project, the one your engineers' assistants use, so the agent recalls what
they recorded. The key has no expiry; revoke it on the same page.

The grant prints the key, `sk-vornik-…`, once. Keep it only until you have
entered it in the AWS console.

## Reach Vornik

DevOps Agent calls an `https://` URL. You have two options.

- **Private connection (recommended when Vornik runs in your VPC).** Create a
  [private connection](https://docs.aws.amazon.com/devopsagent/latest/userguide/configuring-integrations-and-knowledge-connecting-to-privately-hosted-tools.html)
  to the Vornik host, with its HTTPS port in the port ranges. Nothing is
  exposed to the internet. Terminate TLS in front of Vornik, for example with
  Caddy or an internal load balancer. If the certificate comes from a private
  certificate authority, add that authority's certificate to the private
  connection.
- **Public HTTPS.** Install with `VORNIK_PUBLIC_HOSTNAME=<name>` to put Caddy
  in front of the daemon (see "HTTPS with the quickstart" in
  `deployments/podman/README.md`). Then allow port 443 only from the
  [DevOps Agent IP addresses](https://docs.aws.amazon.com/devopsagent/latest/userguide/aws-devops-agent-security.html)
  in your firewall or security group.

Either way, expose only the TLS port. The daemon's own port (8080 by
default) serves plain HTTP: keep it on loopback or close it in the firewall.
Every request needs the key; without a valid key, Vornik answers 401.

**Limits.** Each companion key may make 5 requests a second, with bursts of
up to 50; past that Vornik answers 429 with `Retry-After`. Change it with
`api.rate_limit.companion_key` (`rps`, `burst`) in `config.yaml`. One
address that sends 15 wrong keys in 5 minutes is locked out for 15 minutes.
When Vornik is reachable from the internet, also limit each address before
any key is checked:

```yaml
api:
  rate_limit:
    per_ip:
      rps: 20
      burst: 40
```

Vornik-hosted instances already run with this.

## Connect in the DevOps Agent console

1. Open **Capability Providers**, find **Vornik** under partner connectors,
   and choose to connect it. Until the Vornik connector is listed, register
   it as your own **MCP Server** instead. Use the endpoint
   `https://<host>:<port>/api/v1/mcp/companion`, choose API key
   authentication, and set the header `Authorization` with the value
   `Bearer sk-vornik-…`.
2. Fill in the form:

   | Field | Value | Example |
   |---|---|---|
   | **Vornik host name** | The host name in Vornik's certificate | `vornik.internal.example.com` |
   | **Vornik HTTPS port** | The port TLS listens on | `443` |
   | **Vornik companion key** | The `sk-vornik-…` key from the grant | |

3. If you use a private connection, select it.
4. In the Agent Space, allowlist the tools your key's profile lists, and no
   others.

When you connect, DevOps Agent lists Vornik's tools. A wrong key fails the
connection.

## Check it

In the Agent Space, start a chat and ask it to recall what Vornik knows
about a service you have notes on. The agent finds the tool as
`<registration name>_recall`, for example `vornik_recall`. Tools can take a
minute to appear after you connect. On the Vornik host, `vornikctl companion
keys -p devops-agent` lists the key (never its secret) with when it was last
used, and the console's task list shows any work the agent delegated.

## Rotate or revoke

Mint a new key and enter it in the DevOps Agent registration. Then revoke
the old one with `vornikctl key revoke <keyID> -p devops-agent` (`vornikctl
companion keys -p devops-agent` lists the IDs), and delete it with `vornikctl
key remove` if you want it off the list. A key stops working on its
`--expires` date.

## What to know

- **What the agent writes is a note, not a fact.** An investigation reads
  logs and alerts that an attacker may influence. What it remembers passes
  Vornik's memory gates and is stored with the source
  `companion:aws-devops-agent:note`. Your engineers' sessions see that label when the note
  is recalled.
- **The endpoint URL is in your CloudTrail.** AWS logs the registered URL in
  the account.
- **Costs.** Recall and skill reads use your embedding endpoint. A delegated
  workflow runs your models and counts against the key's `--budget-usd`.
