# Vornik 101 · Visual edition

Open [index.html](index.html) in a browser. The page is a standalone HTML file with inline styles, diagrams, and JavaScript; it does not need a build, service, or external font library.

The reading path targets 10–15 minutes. Expandable panels contain optional setup and workflow details. Copy buttons copy the prompts; if the browser blocks clipboard access, the page selects the text for manual copying.

The core page works offline. Supporting local Markdown links require the adjacent `vornik-101` folder, while official documentation links require internet access. Browsers may download Markdown links rather than render them.

Content is based on the [text edition](../vornik-101/index.md) and its [research and verification record](../vornik-101/research.md). This version is saved locally and has not been deployed.

Verified in Chromium at desktop (1440px), mobile (390px), and narrow mobile (320px) widths with no page overflow. Checked expandable workflow details, copy-button feedback, JavaScript runtime errors, print rendering, local link targets, and internal anchors. Reading time is an editorial estimate, not a timed learner test.

## Branding source

RAG searches on 2026-10-08 found references to an “EaseIT Labs — Brand 2026” folder, but did not return the guide's actual rules. The linked Drive folder was not accessible. This edition therefore applies the brand system verifiable in the targeted repository, pending comparison with the full guide:

- EaseIT Labs blue `#0078C3` and lime `#78B41E`, with their documented lighter/darker variants and charcoal surfaces from `internal/ui/templates/_partials.html`.
- Bricolage Grotesque for headings, Hanken Grotesk for body text, and JetBrains Mono for prompts, matching that file's type system.
- The exact Vornik mark from `docs/public/assets/vornik-mark.svg` and favicon from `internal/ui/static/icon.svg`, replacing the initial custom V glyph.

Latin font subsets are embedded in the HTML for offline rendering. Their SIL Open Font License notices are included under `licenses/`. No RAG meeting notes or personal information were copied into the public guide.
