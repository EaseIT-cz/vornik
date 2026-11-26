"""MkDocs hook: never publish an unreleased release-notes section.

docs-publishing design, Amendment 2026-11-25. The public release notes are
written in place while a release is prepared; without this hook every push
to main published the draft section (docs.vornik.io showed 2026.12.1 before
it was released). A ``## <tag>`` section is released when the EE notes
``docs/release-notes/<tag>.md`` carry a dated ``**Release date:** YYYY-MM-DD``
line, the rule check-release-notes-sections.sh uses. A section without an
EE notes file is kept: that state is the notes check's to refuse.
"""

import os
import re

_HEADING = re.compile(r"^## (20[0-9]{2}\.[0-9]+\.[0-9]+)(?=[^0-9.]|$)")
_DATED = re.compile(r"^\*\*Release date:\*\* [0-9]{4}-[0-9]{2}-[0-9]{2}", re.M)
_PAGE = "release-notes/index.md"


def _released(tag, notes_dir):
    path = os.path.join(notes_dir, tag + ".md")
    if not os.path.isfile(path):
        return True
    with open(path, encoding="utf-8") as f:
        return bool(_DATED.search(f.read()))


def drop_unreleased(markdown, notes_dir):
    """Return markdown without the ``## <tag>`` sections that are not released."""
    out, dropping = [], False
    for line in markdown.splitlines(keepends=True):
        if line.startswith("## "):
            m = _HEADING.match(line)
            dropping = bool(m) and not _released(m.group(1), notes_dir)
        if not dropping:
            out.append(line)
    return "".join(out)


def on_page_markdown(markdown, page, config, files):  # noqa: ARG001 (MkDocs hook signature)
    if page.file.src_uri != _PAGE:
        return markdown
    root = os.path.dirname(os.path.abspath(config["config_file_path"]))
    return drop_unreleased(markdown, os.path.join(root, "docs", "release-notes"))
