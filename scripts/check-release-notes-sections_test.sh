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

# --- 4. the codename and its story (packaging design, Amendment 2026-11-25) ---
# From the 2026.12 line a release's public heading carries " — <Name>" and
# its section a "### The name" story of at least 60 words; --release refuses
# either missing. A 2026.11 tag is not asked for one.
story="word word word word word word word word word word word word word word word word word word word word word word word word word word word word word word word word word word word word word word word word word word word word word word word word word word word word word word word word word word word word word word"  # filler: the check counts words, it does not read them
# codename_fixture NAME HEADING SECTION_TEXT
codename_fixture() {
	local d="$tmp/$1"
	mkdir -p "$d/notes"
	printf '# vornik 2026.11.8\n\n**Release date:** 2026-11-28\n' >"$d/notes/2026.11.8.md"
	printf '# vornik 2026.12.1\n\n**Release date:** 2026-12-18\n' >"$d/notes/2026.12.1.md"
	{
		printf '# Release Notes\n\n'
		printf '%s\n\n%s\n\n- **Thing.** Changed.\n\n---\n\n' "$2" "$3"
		printf '## 2026.11.8\n\nA release.\n\n- **Other.** Fixed.\n'
	} >"$d/index.md"
	echo "$d"
}
d="$(codename_fixture cn-ok '## 2026.12.1 — Cifa' "A release.

### The name

$story")"
expect 0 "--release passes a titled heading with its story" --release 2026.12.1 "$d/notes" "$d/index.md"
expect 0 "--release asks no codename of a 2026.11 tag" --release 2026.11.8 "$d/notes" "$d/index.md"
d="$(codename_fixture cn-untitled '## 2026.12.1' "A release.

### The name

$story")"
expect 1 "--release refuses a heading without a codename" --release 2026.12.1 "$d/notes" "$d/index.md"
d="$(codename_fixture cn-nostory '## 2026.12.1 — Cifa' 'A release.')"
expect 1 "--release refuses a codename without its story" --release 2026.12.1 "$d/notes" "$d/index.md"
d="$(codename_fixture cn-short '## 2026.12.1 — Cifa' "A release.

### The name

Cifa was a computer.")"
expect 1 "--release refuses a story under 60 words" --release 2026.12.1 "$d/notes" "$d/index.md"
d="$(codename_fixture cn-draft '## 2026.12.1' 'A release. Draft: not yet released.')"
printf '# vornik 2026.12.1\n\n**Release date:** not released\n' >"$d/notes/2026.12.1.md"
expect 0 "a draft section is not asked for a codename outside --release" "$d/notes" "$d/index.md"

# --- 5. a stale undated release (docs-publishing design, Amendment 2026-11-25) ---
# The docs build hides a section whose EE notes are undated. A version below
# a dated one is released, so undated notes there are stale: 2026.7.7 was
# tagged on 2026-07-29 and its notes still said "unreleased".
d="$(fixture stale-undated 'unreleased (accumulating on main)' 'A release.')"
printf '# vornik 2026.1.2\n\n**Release date:** 2026-01-20\n' >"$d/notes/2026.1.2.md"
printf '## 2026.1.2\n\nLater.\n' | cat - "$d/index.md" >"$d/index2.md" && mv "$d/index2.md" "$d/index.md"
expect 1 "an undated version below a dated one is refused (the site would hide it)" "$d/notes" "$d/index.md"
d="$(fixture newest-undated 'not released (draft)' 'A release.')"
expect 0 "the newest version may be undated (it is the release in preparation)" "$d/notes" "$d/index.md"

# --- 7. older release lines are rolled up (packaging design, Amendment 2026-11-25) ---
# At most three released monthly lines keep full sections; a line in
# preparation (undated notes) does not count.
# rollup_fixture NAME MONTHS... — one released release per month, newest first
rollup_fixture() {
	local d="$tmp/$1"; shift
	mkdir -p "$d/notes"
	{
		printf '# Release Notes\n\n'
		for m in "$@"; do
			printf '# vornik 2026.%s.1\n\n**Release date:** 2026-%02d-10\n' "$m" "$m" >"$d/notes/2026.$m.1.md"
			printf '## 2026.%s.1\n\nA release.\n\n' "$m"
		done
		printf '## Earlier releases\n\n### 2026.1.x\n\n- **2026.1.1** — Old.\n'
	} >"$d/index.md"
	echo "$d"
}
d="$(rollup_fixture four-lines 9 8 7 6)"
out="$("$script" "$d/notes" "$d/index.md" 2>&1)" && rc=0 || rc=$?
if [ "$rc" -eq 1 ] && grep -q "2026.6" <<<"$out"; then pass "four released lines in full are refused, naming the oldest"; else fail "four lines: rc=$rc $out"; fi
d="$(rollup_fixture three-lines 9 8 7)"
expect 0 "three released lines in full pass" "$d/notes" "$d/index.md"
d="$(rollup_fixture draft-line 10 9 8 7)"
printf '# vornik 2026.10.1\n\n> **Draft.** Not yet released.\n' >"$d/notes/2026.10.1.md"
expect 0 "a line in preparation does not count against the three" "$d/notes" "$d/index.md"

if [ "$fails" -gt 0 ]; then
	echo "check-release-notes-sections_test: $fails failure(s)"
	exit 1
fi
echo "check-release-notes-sections_test: OK"
