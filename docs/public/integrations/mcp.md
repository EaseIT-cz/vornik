# MCP tool servers

!!! note "Not managed in the Integrations Hub"

    MCP tool servers are not one of the Integrations Hub's guided
    integrations. The daemon's MCP catalog has its own management surface,
    described below for each edition.

vornik reaches external tools through MCP (Model Context Protocol) servers.
An agent can call a server's tool only when three settings allow it:

1. **The daemon catalog** (`mcp.servers` in `config.yaml`): the servers your
   installation knows about. Adding a server here is a discovery step, not
   an access grant. It makes the server visible so projects can use it, and
   connects nothing by itself.
2. **The project's subscription** (`mcp.servers` in the project's YAML): a
   project sees only the tools of a server it lists by name, optionally
   narrowed to some of them. See [Connect Your Tools (MCP)](../guides/mcp-tools.md)
   for how a project declares and restricts its servers.
3. **The role's tool list** (`permissions.allowedTools` of a role in the
   project's swarm): an agent step calls a server's tool only when its
   role's list names it (`mcp__<server>__<tool>`) or a pattern that covers
   it. A role with no list at all is not narrowed.

## Managing the catalog

**Enterprise Edition.** Use the **MCP servers** tab of the control-plane hub
(`/ui/admin/control-plane`, admin-only).

- It lists the catalog with each server's reachability and tool count.
- **Test** connects to a candidate endpoint and lists its tools before you
  commit anything.
- Every add and remove is filed as a
  [control-plane proposal](../features/control-plane.md): you review the
  diff and apply it. Nothing is written silently.
- **Use in a project**, on a server's row, puts the server to work: pick a
  project and tick which tools of the server each role of its swarm may
  call. It files one proposal that subscribes the project and updates those
  roles' tool lists, which you review and apply like any other. When the
  swarm serves other projects too, the page says so.
- The tab handles HTTP servers (`sse`, `streamable-http`) only. A **stdio**
  server is a program the daemon runs, so it is registered by editing
  `config.yaml` on the host, never through the daemon. The UI refuses to
  probe, add or change one, and so does any control-plane proposal or
  project-config edit made through the daemon.

**Community Edition.** Edit `mcp.servers` in `config.yaml`:

```yaml
mcp:
  servers:
    - name: home-assistant
      transport: streamable-http   # or "sse", or "stdio" with command:
      url: https://mcp.example.com/mcp
```

The [configuration reference](../reference/configuration.md) lists every
field.

## Good to know

- **A catalog change takes effect at the next daemon restart**, not on a hot
  config reload. A project's own subscription list does apply on reload.
  Either way a running task keeps the tools it started with: changes reach
  new runs only.
- The catalog is admin territory in every edition. It is shared,
  installation-wide state, which is why it never appears in the
  Integrations Hub beside per-project channels such as email or Slack.
