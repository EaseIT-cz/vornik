#!/usr/bin/env bash
# issue-templates_test.sh — every label an issue template applies is one the
# repository's label vocabulary has.
#
# GitHub #399 (found 2026-10-16, housekeeping review): bug_report.yml applied
# labels: ["bug", "triage"], and the repository has no `triage` label, only
# `triage:*` labels for enhancements (ENGINEERING-TENETS §9). GitHub drops an
# unknown template label without a word, so the template read as triaging
# every bug while it applied nothing. A template applies its category label
# only (tenets §9.2: one of `bug` or `enhancement`); the priority label is
# triage's decision, not the filer's.
#
# The label set lives on GitHub, which this offline suite cannot read, so it
# pins the templates to the category vocabulary the tenets name. Denominator:
# the suite fails when it examined no template, or a template with no labels
# line.
set -uo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DIR="${ISSUE_TEMPLATE_DIR:-$HERE/../.github/ISSUE_TEMPLATE}"
ALLOWED=" bug enhancement "
fails=0 examined=0
for f in "$DIR"/*.yml "$DIR"/*.yaml; do
  [ -f "$f" ] || continue
  # config.yml configures the chooser; it is not a template.
  [ "$(basename "$f")" = config.yml ] && continue
  examined=$((examined + 1))
  line="$(grep -m1 '^labels:' "$f" || true)"
  if [ -z "$line" ]; then
    echo "FAIL $(basename "$f"): no labels line"; fails=$((fails + 1)); continue
  fi
  labels="$(sed -E 's/^labels:[[:space:]]*\[?//; s/\][[:space:]]*$//; s/["'\'']//g; s/,/ /g' <<<"$line")"
  for l in $labels; do
    case "$ALLOWED" in
      *" $l "*) echo "ok   $(basename "$f"): $l" ;;
      *) echo "FAIL $(basename "$f"): label '$l' is not in the template vocabulary ($ALLOWED)"
         fails=$((fails + 1)) ;;
    esac
  done
done
if [ "$examined" -eq 0 ]; then
  echo "FAIL examined 0 issue templates in $DIR"; exit 1
fi
if [ "$fails" -ne 0 ]; then echo "issue-templates_test: $fails failure(s) over $examined template(s)"; exit 1; fi
echo "issue-templates_test: all passed ($examined template(s) examined)"
