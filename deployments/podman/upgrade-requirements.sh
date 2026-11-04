#!/usr/bin/env bash
# upgrade-requirements.sh — print the "Upgrade requirements" box of every
# release newer than the installed one (#610).
#
# Usage: upgrade-requirements.sh <installed-release> < release-notes.md
#
# Reads docs/public/release-notes/index.md on stdin. Release sections are
# "## <version>" headings, newest first. A release that needs action before
# upgrading carries a block that starts with the line
#     !!! warning "Upgrade requirements"
# whose content is the following lines indented by four spaces.
#
# stdout: each box in range, under its version, newest first; nothing when no
#         release in range has one.
# stderr: the denominator, "examined N release(s) newer than X, M with upgrade
#         requirements". When <installed-release> is empty or has no section in
#         the notes, every box is printed and the line says why: showing too
#         much is the safe failure.
#
# FAIL-SAFE ON FORMAT DRIFT. This helper runs from the INSTALLED checkout while
# the notes come from the TARGET, so it is one release older than what it
# parses. A section that mentions "Upgrade requirements" (any case) in a shape
# this helper does not recognise is printed WHOLE, flagged as unrecognised,
# and counted as having requirements: under-reporting would skip the gate.
#
# Design: https://docs.vornik.io §23.
set -euo pipefail

installed="${1:-}"
notes="$(cat)"

found=0
if [[ -n "$installed" ]] && grep -qxF "## $installed" <<<"$notes"; then
  found=1
fi

awk -v installed="$installed" -v found="$found" '
  function flush() {
    if (version != "" && box != "") {
      printf "%s\n", "== " version
      printf "%s", box
      printf "\n"
      with_box++
    } else if (version != "" && mention) {
      printf "%s\n", "== " version " (format not recognised: the whole section follows)"
      printf "%s", raw
      printf "\n"
      with_box++
    }
    box = ""; raw = ""; mention = 0
  }
  /^## / {
    flush()
    candidate = substr($0, 4)
    if (found && candidate == installed) { stop = 1 }
    if (stop) { version = ""; next }
    version = candidate
    examined++
    inbox = 0
    next
  }
  stop { next }
  { raw = raw $0 "\n" }
  tolower($0) ~ /upgrade requirements/ { mention = 1 }
  /^!!! warning "Upgrade requirements"[[:space:]]*$/ { inbox = 1; next }
  inbox && /^    / { box = box substr($0, 5) "\n"; next }
  inbox && /^[[:space:]]*$/ { next }
  inbox { inbox = 0 }
  END {
    flush()
    if (found) {
      printf "upgrade requirements: examined %d release(s) newer than %s, %d with upgrade requirements\n",
        examined, installed, with_box > "/dev/stderr"
    } else {
      printf "upgrade requirements: installed release \"%s\" not found in the release notes; showing every release: examined %d release(s), %d with upgrade requirements\n",
        installed, examined, with_box > "/dev/stderr"
    }
  }
' <<<"$notes"
