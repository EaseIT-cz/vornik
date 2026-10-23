#!/usr/bin/env bash
# shellcheck disable=SC2015  # pass/fail never fail, so `A && pass || fail` is an if-then-else here
# Tests for scripts/check-release-notes-sections.sh. Two concerns, merged
# 2026-10-14 when both landed in one file:
#
# 1. The gate must not pass by examining nothing. #334 item 6, audit
#    2026-10-12: a missing notes directory printed "nothing to check" and an
#    empty one "OK — 0 release(s)", both exit 0.
# 2. The draft-marker rule. Incident (lead audit, 2026-10-14): the public
#    2026.10.5 section still said "Draft: not yet released." after the tag,
#    and publish-release.yml copied it verbatim into the live GitHub release
#    body. Packaging design, "Note 2026-10-14 — a draft marker never reaches
#    a release body".
#
# Drives the real script against fixture trees (it takes the notes dir and
# the index as args).
set -uo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
script="$here/check-release-notes-sections.sh"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

fails=0
pass() { printf 'ok   - %s\n' "$1"; }
fail() { printf 'FAIL - %s\n' "$1"; fails=$((fails + 1)); }

# --- 1. examined-nothing (#334 item 6) ---
mkdir -p "$tmp/n6/notes" "$tmp/n6/empty"
printf '# Release Notes\n\n## 2026.9.1\n\n- a change\n' >"$tmp/n6/index.md"
printf 'notes\n' >"$tmp/n6/notes/2026.9.1.md"

out="$(bash "$script" "$tmp/n6/notes" "$tmp/n6/index.md" 2>&1)"; rc=$?
[ "$rc" -eq 0 ] && grep -q 'OK — 1 release(s)' <<<"$out" && pass "a covered release passes and is counted" || fail "covered: rc=$rc $out"
out="$(bash "$script" "$tmp/n6/missing" "$tmp/n6/index.md" 2>&1)"; rc=$?
[ "$rc" -ne 0 ] && pass "a missing notes directory fails" || fail "missing dir must fail: rc=$rc $out"
out="$(bash "$script" "$tmp/n6/empty" "$tmp/n6/index.md" 2>&1)"; rc=$?
[ "$rc" -ne 0 ] && grep -q 'examined 0' <<<"$out" && pass "an empty notes directory fails, naming 0" || fail "empty dir must fail: rc=$rc $out"
printf 'notes\n' >"$tmp/n6/notes/2026.9.2.md"
out="$(bash "$script" "$tmp/n6/notes" "$tmp/n6/index.md" 2>&1)"; rc=$?
[ "$rc" -ne 0 ] && grep -q "no '## 2026.9.2' section" <<<"$out" && pass "a release with no section fails" || fail "missing section: rc=$rc $out"

# --- 2. draft marker ---
# fixture NAME RELDATE_1_1 SECTION_1_1_TEXT
fixture() {
	local d="$tmp/$1"
	mkdir -p "$d/notes"
	printf '# vornik 2026.1.0\n\n**Release date:** 2026-01-05\n' >"$d/notes/2026.1.0.md"
	printf '# vornik 2026.1.1\n\n**Release date:** %s\n' "$2" >"$d/notes/2026.1.1.md"
	{
		printf '# Release Notes\n\n'
		printf '## 2026.1.1\n\n%s\n\n- **Thing.** Changed.\n\n---\n\n' "$3"
		printf '## 2026.1.0\n\nA release.\n\n- **Other.** Fixed.\n'
	} >"$d/index.md"
	echo "$d"
}

expect() { # expect EXIT DESC ARGS...
	local want="$1" desc="$2"
	shift 2
	local out rc=0
	out="$("$script" "$@" 2>&1)" || rc=$?
	if [ "$rc" -eq "$want" ]; then
		pass "$desc"
	else
		fail "$desc (exit $rc, want $want)"
		printf '%s\n' "$out" | sed 's/^/    /'
	fi
}

d="$(fixture released-marker 2026-01-09 'A release. Draft: not yet released.')"
expect 1 "a released section carrying the draft marker is refused" "$d/notes" "$d/index.md"

d="$(fixture draft-marker 'not released (draft of 2026-01-08)' 'A release. Draft: not yet released.')"
expect 0 "an unreleased section may carry the draft marker" "$d/notes" "$d/index.md"
expect 1 "--release refuses the marker in the tag being cut" --release 2026.1.1 "$d/notes" "$d/index.md"
expect 0 "--release ignores another section's marker" --release 2026.1.0 "$d/notes" "$d/index.md"

d="$(fixture released-clean 2026-01-09 'A release.')"
expect 0 "a released section without the marker passes" "$d/notes" "$d/index.md"
expect 0 "--release passes a clean section" --release 2026.1.1 "$d/notes" "$d/index.md"

d="$(fixture released-word 2026-01-09 'Drafts of skills are reviewed; a draft pull request opens.')"
expect 0 "the word draft without the marker token passes" "$d/notes" "$d/index.md"

# --- 3. the EE notes of the tag being cut ---
# audit 2026-10-14 (#334 delta audit, W8-V): release.yaml publishes
# docs/release-notes/<tag>.md verbatim as the GitHub release body, and nothing
# refused its "> **Draft.**" banner or an undated "**Release date:** not
# released" line: 2026.10.5's EE notes still carry the banner today. --release
# refuses both for the tag being cut; other tags' notes are not its business.
d="$(fixture ee-banner 2026-01-09 'A release.')"
printf '\n> **Draft.** Nothing here is tagged or published yet.\n' >>"$d/notes/2026.1.1.md"
expect 1 "--release refuses the EE notes' draft banner" --release 2026.1.1 "$d/notes" "$d/index.md"
expect 0 "--release ignores another tag's draft banner" --release 2026.1.0 "$d/notes" "$d/index.md"

d="$(fixture ee-undated 'not released (draft of 2026-01-08)' 'A release.')"
expect 1 "--release refuses an undated EE release date" --release 2026.1.1 "$d/notes" "$d/index.md"

d="$(fixture ee-missing 2026-01-09 'A release.')"
rm "$d/notes/2026.1.1.md"
expect 1 "--release refuses a tag with no EE notes (release.yaml would publish nothing)" --release 2026.1.1 "$d/notes" "$d/index.md"

d="$(fixture ee-final 2026-01-09 'A release.')"
printf '\n> A release about drafts: drafting is reviewed.\n' >>"$d/notes/2026.1.1.md"
expect 0 "--release passes final EE notes that mention drafts in prose" --release 2026.1.1 "$d/notes" "$d/index.md"

if [ "$fails" -gt 0 ]; then
	echo "check-release-notes-sections_test: $fails failure(s)"
	exit 1
fi
echo "check-release-notes-sections_test: OK"
