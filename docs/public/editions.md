# Editions: Community vs Enterprise

Vornik comes in two editions built from the same core:

- **Community Edition (CE)** is open source under **AGPL-3.0**, published at
  [EaseIT-cz/vornik](https://github.com/EaseIT-cz/vornik). It is the
  complete orchestration core, fully usable for personal and small-team work.
- **Enterprise Edition (EE)** is a proprietary overlay that adds the
  capabilities marked Enterprise below. EaseIT Labs offers the Enterprise
  Edition with commercial support.

## Feature matrix

> A Community build includes only the rows marked **Community**.

<!-- BEGIN GENERATED editions-matrix -->

| Capability | Community | Enterprise |
|---|:---:|:---:|
| Task orchestration (tasks, leases, durable execution) | ✅ | ✅ |
| Workflows | ✅ | ✅ |
| Tool access over MCP | ✅ | ✅ |
| Control CLI (`vornikctl`) + HTTP API | ✅ | ✅ |
| Guided Integrations Hub (connect channels from the UI) | ✅ | ✅ |
| Counterfactual replay / “Black Box” | — | ✅ |
| Learning / “Instinct” layer (learned budgets) | — | ✅ |
| Clustering / horizontal scale | — | ✅ |
| Admin suite | — | ✅ |
| Memory firewall | ✅ | ✅ |
| OIDC / SSO | — | ✅ |
| Log shipping (Logship) | — | ✅ |
| Cross-project orchestration (`call_project`) | — | ✅ |
| NL Automation Composer (describe-it project setup) | — | ✅ |
| User accounts and channel/key→account mapping | ✅ | ✅ |
| Configuration assistant | ✅ | ✅ |
| Architect consultation over A2A (contract-gated) | ✅ | ✅ |

<!-- END GENERATED editions-matrix -->

A Community build registers and wires none of the Enterprise capabilities:
they are absent from the binary, not hidden.

## Which edition do I need?

- **Community**: personal use, evaluation and small deployments. It is the
  full orchestration engine, not a limited demo.
- **Enterprise**: teams that need the Enterprise capabilities above, SSO and
  governance, horizontal scale, or commercial support.

[Support](support.md) lists the commercial options. [Licensing](licensing.md)
says what the AGPL-3.0 does and does not require when you use or fork
Community.
