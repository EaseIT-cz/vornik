# Compliance status

**Last updated:** 1 November 2026

Where Vornik stands on the EU AI Act and GDPR, including what is not done.

| | Status |
|---|---|
| **EU AI Act Art 50(1)** (telling people they talk to an AI) | 🟢 Implemented on every conversational and publishing surface |
| **EU AI Act Art 50(2)** (machine-readable marking) | 🟢 Built: signed marks, a ledger and public detection; due 2 Dec 2026 under transitional relief |
| **EU AI Act Art 4** (AI literacy) | 🟢 Documentation published for operators |
| **EU AI Act high-risk (Annex III)** | ⚪ Not applicable today; obligations begin 2 Dec 2027 |
| **GDPR, technical measures** | 🟡 In place, except that per-person rights reach only records linked to that person (see the gaps) |
| **GDPR, documentation** | 🟢 Templates for the record of processing, DPA and DPIA, and a statement of Vornik's own measures; ask for them |

**We do not claim to be "GDPR compliant".** In a self-hosted deployment you
are the controller. Measures only you can provide (encryption at rest,
backups, access review) are yours.

## Self-hosting

- Your data lives in your PostgreSQL, on your infrastructure.
- Data leaves your network only to services you configure: model providers,
  chat platforms, forges, MCP servers. With local models and no external
  services, nothing leaves.
- No Vornik-operated cloud holds your data, so there is no vendor-side breach
  surface.

## EU AI Act

### Article 50(1): people are told they are talking to an AI

Telegram, Slack, email, GitHub comments and the web chat disclose that
replies are AI-generated. Chat surfaces disclose once per conversation. Email
and GitHub comments carry the notice on every message, because they are
forwarded and quoted out of their thread. Each disclosure is recorded with a
timestamp and a hash of the exact wording served. Details:
[AI transparency](ai-transparency.md).

### Article 50(2): AI-generated content is marked

- **Marks.** A signed mark (Ed25519) in every carrier that can hold one:
  response and email headers, a hidden GitHub comment, Slack message
  metadata, document metadata.
- **Ledger.** Every mark is recorded, including where a channel can carry
  nothing (Telegram, agent files, voice).
- **Detection.** Anyone can look up a content hash, verify a mark and fetch
  the public keys.
- **No watermark.** Vornik meets the law directly and does not follow the
  voluntary Code of Practice, which also asks for an invisible watermark.
  Voice replies are off by default because audio carries no watermark; an
  operator who turns them on records that they accept this.

Vornik was on the market before 2 August 2026, so the transitional relief
moves this obligation to **2 December 2026**. We rely on that relief. How
marking works, its limits and how to check content:
[AI transparency](ai-transparency.md#how-ai-generated-output-is-marked).

### High-risk obligations (Annex III)

Vornik is not placed on the market as a high-risk system. If you deploy it in
an Annex III use case (recruitment, credit scoring, education, essential
services, critical infrastructure), you become the deployer of a high-risk
system from **2 December 2027**, with the Article 26 duties. We are building
the evidence pack you would need (logging guarantees, instructions for use,
human-oversight documentation, data governance). Tell us if you plan such a
deployment.

## GDPR

### In place (shipped code)

- **Erasure.** Project-scoped hard deletion across all tables. Per-person
  erasure with a preview: records only about that person are deleted; shared
  memory records are redacted and re-embedded. Chunk-level eviction leaves a
  tombstone, so a deletion can be evidenced.
- **Retention.** A sweeper with per-table and per-project windows and
  evidence floors. **Off by default**, so an upgrade never deletes data: you
  turn it on and choose the windows. `vornikctl doctor` warns while personal
  data is kept indefinitely.
- **Breach handling.** A breach ledger (`vornikctl incident`): the 72-hour
  clock runs from awareness; the authority and data-subject thresholds are
  asked separately.
- **Purpose limitation.** Memory chunks carry a sensitivity and purpose
  policy, evaluated on every retrieval; block decisions are kept for a year.
- **Accountability.** Audit trails for tool calls, chat, retrieval and
  ingest.
- **Access control.** User accounts, per-project scoping, API keys; OIDC
  single sign-on with group permissions in the Enterprise Edition.

### Not in place

| Gap | Consequence | Status |
|---|---|---|
| **Per-person rights are partial** | `vornikctl subject` handles access, portability, erasure, rectification (Art 16) and restriction (Art 18), but only over records linked to that person. Rectification corrects memory records and knowledge-graph entities. Restriction withholds memory records and knowledge-graph entities with their relations; transcripts, profiles and files stay in use, and the graph extractor can still match a new mention against a restricted name. A record that mentions someone but was never linked to them is not reached | Rectification and restriction shipped 2026-10-12; limits in §8 of the DPA template |
| **Erasure in conversation transcripts** | In a multi-person conversation, the person's own messages are replaced by a dated removal note and their identifiers masked elsewhere. What remains in others' messages is kept only on a ground the operator assesses per request. A record a running task still uses is deferred, and the request is reported as not fully actioned | Shipped 2026-10-30; limits in §8 of the DPA template |
| **Retention off by default** | Personal data is kept indefinitely until you enable the sweeper | Recommended profile shipped; `vornikctl doctor` warns |
| **Record of processing (Art 30)** | As controller, you keep your own | Template with a worked example; ask for it |
| **DPIA (Art 35)** | Yours to perform as controller where it is triggered | Template, and a statement of Vornik's own measures (Art 32); ask for them |
| **Data processing agreement (Art 28)** | Needed only where we process personal data for you (for example, support access) | Template meeting Art 28(3); ask for it |

If you are evaluating Vornik for personal data at scale, weigh the first row.

### Sub-processors

Conversation content goes to the model providers you configure, listed on the
[AI transparency](ai-transparency.md) page. A local-model configuration sends
nothing out.

Approval notifications, once a paired device turns them on, go through that
browser's push service (Google, Mozilla, Apple or Microsoft). Each one is
encrypted to the device and says only that a request is waiting, never what
it asks. The push service sees the device's address and when a notification
was sent.

## Contact

Security issues: [security](security.md). Compliance and data protection:
**enterprise@vornik.io**.

<!-- summary-reviewed-through: 2026.11.2 -->
