# Discover and validate redirects

The repository ships the portable capability skill
`contrib/skills/redirect-discovery.swarm-skill.md`. It investigates an unexpected
public redirect, corroborates candidate hosts with independent official
references or explicit operator confirmation, and produces a report.

The target project must have a working scraper MCP connection exposing
`mcp__scraper__web_fetch`. From a repository checkout, preview then import into
that project's existing swarm, using the configuration directory your daemon
reads:

```bash
vornikctl skill import contrib/skills/redirect-discovery.swarm-skill.md --project PROJECT --configs-dir CONFIGS --dry-run
vornikctl skill import contrib/skills/redirect-discovery.swarm-skill.md --project PROJECT --configs-dir CONFIGS
vornikctl config reload
vornikctl task submit -p PROJECT --workflow redirect-discovery --prompt "Investigate this public URL and its intended organization/purpose: URL. Independent official reference: REFERENCE."
```

Import creates the `redirect-discovery` workflow and adds `redirect-researcher`
to the chosen swarm. Import conflicts are reported before writing; use the
existing import rename flags if those names already exist. Importing the file
is an explicit deployment step; merely updating this checkout does not enable
it in the running daemon.

The agent starts with the source host. A scraper host denial identifies a
candidate; the denial does not establish trust. Independently corroborated
hosts can be included in a narrow subsequent fetch. Each original URL gets at
most three attempts, including the first, and at most two evidence fetches.
Missing evidence, exhausted attempts, or an unverified downstream host give
`needs-review`. Unsafe, authenticated, gated, or unreachable requests give
`blocked`. Only independently corroborated hosts plus a successful final fetch
can give `verified`.

Read `artifacts/out/redirect-discovery.md` for the observed hosts, corroborating
sources, actual host list, attempt counts, and unresolved questions. The
workflow's `COMPLETED` state means it produced a report; check the report's
verification status separately. Reports omit URL query values and fragments.

The skill uses the existing scraper navigation and SSRF guards. It does not
alter project configuration or automatically approve unknown domains, and it
does not investigate credential-bearing links, OAuth/write redirects, or
user-authenticated browser profiles. If independent official evidence is not
available, supply explicit confirmation in a later investigation rather than
asking the agent to guess from branding or a working TLS certificate.
