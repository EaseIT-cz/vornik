# Compliance status

**Last updated:** 25 October 2026

This page states where Vornik actually stands on EU AI Act and GDPR
conformity — including what is **not** done. We publish the gaps because a
vendor's compliance page is the first thing a buyer's legal team tests, and an
unverifiable claim fails that test immediately.

If you need a document listed below as "in preparation", ask — the timeline is
real and we would rather tell you than have you discover it in procurement.

---

## The short version

| | Status |
|---|---|
| **EU AI Act Art 50(1)** (AI-interaction transparency) | 🟢 Implemented on every conversational and publishing surface |
| **EU AI Act Art 50(2)** (machine-readable marking) | 🟡 Designed, approach decided; being built — due 2 Dec 2026 under transitional relief |
| **EU AI Act high-risk (Annex III)** | ⚪ Not applicable today — obligations begin 2 Dec 2027 |
| **GDPR — technical measures** | 🟡 Substantially in place; erasure inside multi-person conversation transcripts is partial (see the gap table) |
| **GDPR — documentation** | 🟡 Records-of-processing and DPA templates exist; the DPIA template is in preparation |

**We do not claim to be "GDPR compliant."** In a self-hosted deployment
compliance is the controller's, and one template (the DPIA) is still being
written. See below.

---

## The self-hosting position, which matters more than any badge

Vornik is self-hosted. In a self-hosted deployment:

- **You are the data controller.** Your data lives in your Postgres, on your
  infrastructure.
- The only data leaving your network is what you send to the services
  **you** configure: model providers, chat platforms, forges and MCP servers.
  With local models and no external services, nothing leaves at all.
- There is no Vornik-operated cloud holding your data, so there is no
  vendor-side breach surface to trust us about.

For a privacy-motivated buyer this is a stronger guarantee than a certification
on a page, because it is structural rather than contractual.

---

## EU AI Act

### Article 50(1) — you must be told you are talking to an AI

**Status: implemented.**

Every conversational surface — Telegram, Slack, email, GitHub comments, web
chat — discloses that replies are AI-generated. Continuous chat surfaces
disclose once at the start of a conversation; email replies and GitHub comments
carry the notice on every message, because those get forwarded and quoted away
from their original thread.

Each disclosure is recorded with a timestamp and a hash of the exact wording
served, so conformity can be demonstrated rather than asserted.

Details: [AI transparency](ai-transparency.md).

### Article 50(2) — machine-readable marking of AI-generated content

**Status: designed and decided; being built. Due 2 December 2026.**

Vornik generates synthetic text — pull request bodies, commit messages, issue
comments, emails — and synthetic speech (voice replies). Article 50(2) requires
that content be marked in a machine-readable way.

The approach: signed, machine-readable metadata on every carrier (commit
trailers, message headers, file manifests) plus a server-side ledger of every
mark, with the limits of marking plain text stated publicly. Synthetic voice
replies will be **off by default** until they can carry an embedded audio
watermark; an operator who turns them on records that they accept the
single-layer state.

Vornik was placed on the market before 2 August 2026 and therefore qualifies
for the transitional relief that moves this obligation to **2 December 2026**.
We are relying on that relief deliberately and disclosing that we are.

### Article 4 — AI literacy

**Status: documentation published for operators.**

### High-risk obligations (Annex III)

**Status: not applicable today.**

Vornik is not currently placed on the market as a high-risk AI system. If you
deploy it into an Annex III use case — recruitment, credit scoring, education,
essential services, critical infrastructure — **you** become the deployer of a
high-risk system from **2 December 2027** and inherit Article 26 duties.

We are building the vendor-side evidence pack you would need (logging
guarantees, instructions for use, human-oversight documentation, data
governance) ahead of that date. Ask if you need it sooner; tell us if you are
planning such a deployment.

---

## GDPR

### What is in place

These are properties of the shipped code, not intentions:

- **Erasure.** Project-scoped hard deletion across the full table set;
  per-person erasure with a preview, where records only about that person are
  deleted and shared records (memory) are redacted and re-embedded rather than
  destroyed; chunk-level eviction with a tombstone audit trail, so a deletion
  can itself be evidenced.
- **Retention.** A configurable sweeper with per-table and per-project
  windows and evidence floors. **Off by default** so an upgrade never silently
  deletes your data — which means you must turn it on and choose your windows;
  `vornikctl doctor` warns while personal data is kept indefinitely.
- **Breach handling.** A breach ledger (`vornikctl incident`) with the 72-hour
  clock running from awareness, and the authority and data-subject thresholds
  asked separately.
- **Purpose limitation.** Memory chunks carry sensitivity and purpose policy,
  evaluated on every retrieval, with the block decisions retained for a year.
- **Accountability.** Full tool-call, chat, retrieval and ingest audit trails.
- **Access control.** User accounts, per-project scoping and API-key
  authentication; OIDC single sign-on with group-based permissions in the
  Enterprise Edition.

### What is not in place

We would rather you read this from us than find it in a questionnaire.

| Gap | Consequence | Status |
|---|---|---|
| **Records of processing (Art 30)** | In a self-hosted deployment you are the controller and keep your own record | A template with a worked example exists; ask for it |
| **Data processing agreement (Art 28)** | Needed only where we process personal data on your behalf (for example support access) | A template satisfying Art 28(3) exists; ask for it |
| **Per-subject rights are partial** | `vornikctl subject` handles access, portability, erasure, rectification (Art 16) and restriction (Art 18) for one person, but only over records the deployment has linked to that person. Rectification corrects memory records and knowledge-graph entities. Restriction withholds memory records and knowledge-graph entities (with the relations that involve them): transcripts, profiles and files stay in use, and the graph extractor can still compare a new mention against a restricted name. The limits are listed in §8 of the DPA template | Shipped 2026-10-12 (rectification, restriction); remaining limits documented |
| **DPIA (Art 35)** | Yours to perform as controller where the trigger conditions apply | A template and a statement of the measures Vornik itself provides are in preparation |
| **Retention off by default** | Personal data is kept indefinitely unless you enable the sweeper | Recommended profile shipped; `vornikctl doctor` warns |
| **Erasure inside conversation transcripts** | Removing one person's turns from a multi-person chat transcript is partly built; such a request is reported as not fully actioned rather than marked done | In progress |

The third row is the one to weigh if you are evaluating Vornik for personal
data at scale. The per-person tools act on what the deployment has linked to a
person. A record that mentions someone but was never identified as being about
them is not reached, and the stores listed above need manual handling.

### Sub-processors

Where configured, conversation content is transmitted to the model providers
listed on the [AI transparency](ai-transparency.md) page. In a self-hosted
deployment you choose these, and a local-model configuration transmits nothing
externally.

---

## Reporting a problem

Security issues: see [security](security.md).
Compliance and data-protection questions: **enterprise@vornik.io**.

<!-- summary-reviewed-through: 2026.10.8 -->
