#!/usr/bin/env bash
# mkdocs-unreleased-hook_test.sh — the docs build drops an unreleased
# release-notes section (docs-publishing design, Amendment 2026-11-25).
#
# Incident 2026-11-25: docs.vornik.io showed the unreleased 2026.12.1
# notes, because the build published docs/public/release-notes/index.md as
# written while the release was being prepared.
set -euo pipefail
fail() { echo "FAIL: $*" >&2; exit 1; }
root="$(cd "$(dirname "$0")/.." && pwd)"
hook="$root/scripts/mkdocs_hooks/unreleased_sections.py"
[ -f "$hook" ] || fail "no hook at $hook"
if ! grep -qE '^hooks:' "$root/mkdocs.yml" || ! grep -q 'scripts/mkdocs_hooks/unreleased_sections.py' "$root/mkdocs.yml"; then
	fail "mkdocs.yml does not register the hook, so neither publish path runs it"
fi
tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT
mkdir -p "$tmp/docs/release-notes"
printf '# vornik 2026.11.8\n\n**Release date:** 2026-11-28\n' >"$tmp/docs/release-notes/2026.11.8.md"
printf '# vornik 2026.12.1\n\n> **Draft.** Not yet released.\n' >"$tmp/docs/release-notes/2026.12.1.md"
cat >"$tmp/index.md" <<'MD'
# Release Notes

Intro.

## 2026.12.1

Unreleased body SECRET-DRAFT.

---

## 2026.11.8

Released body.

## 2026.11.7

No notes file: kept.

## Earlier releases

Rollup.
MD
out="$(python3 -I - "$hook" "$tmp" <<'PY'
import importlib.util, sys
spec = importlib.util.spec_from_file_location("h", sys.argv[1]); h = importlib.util.module_from_spec(spec); spec.loader.exec_module(h)
md = open(sys.argv[2] + "/index.md").read()
print(h.drop_unreleased(md, sys.argv[2] + "/docs/release-notes"))
print("=== other page ===")
class F: src_uri = "guides/x.md"
class P: file = F()
print(h.on_page_markdown("## 2026.12.1\nkeep me", page=P(), config={"config_file_path": sys.argv[2] + "/mkdocs.yml"}, files=None))
PY
)"
grep -q "SECRET-DRAFT" <<<"$out" && fail "the unreleased section was published"
grep -q "## 2026.12.1" <<<"$(sed '/=== other page ===/,$d' <<<"$out")" && fail "the unreleased heading was published"
grep -q "Released body." <<<"$out" || fail "a released section was dropped"
grep -q "No notes file: kept." <<<"$out" || fail "a section without notes was dropped"
grep -q "Rollup." <<<"$out" || fail "the earlier-releases rollup was dropped"
grep -q "keep me" <<<"$out" || fail "another page was changed"
# The real index: every section whose EE notes are undated is gone, every
# other one stays.
real="$(python3 -I - "$hook" "$root" <<'PY'
import importlib.util, sys, re, os
spec = importlib.util.spec_from_file_location("h", sys.argv[1]); h = importlib.util.module_from_spec(spec); spec.loader.exec_module(h)
src = open(sys.argv[2] + "/docs/public/release-notes/index.md").read()
notes = sys.argv[2] + "/docs/release-notes"
out = h.drop_unreleased(src, notes)
tags = re.findall(r"^## (20\d{2}\.\d+\.\d+)", src, re.M)
bad = [t for t in tags if (t in re.findall(r"^## (20\d{2}\.\d+\.\d+)", out, re.M)) != h._released(t, notes)]
print("mismatch:" + ",".join(bad) if bad else "ok %d sections, %d unreleased" % (len(tags), sum(not h._released(t, notes) for t in tags)))
PY
)"
[[ "$real" == ok* ]] || fail "the real index: $real"
echo "PASS mkdocs-unreleased-hook_test ($real)"
