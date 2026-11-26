#!/usr/bin/env bash
#
# check-release-notes-sections.sh — every EE release-notes file must have a
# matching curated section in the PUBLIC notes index, extractable by the same
# rule the CE release workflow uses.
#
# WHY THIS EXISTS. The CE release workflow (scripts/public-ce-templates/
# publish-release.yml) builds a release body by extracting "## <tag>" from
# docs/public/release-notes/index.md, and fails closed when the section is
# absent. Fail-closed is right, but at release time the FIRST release after a
# format drift is the canary — and that is exactly what happened to 2026.8.4:
# the CE tag was pushed before the public prose for it was written, so the
# section did not exist and the release was silently never created. Nobody
# noticed for five weeks.
#
# So the same check runs here, in EE CI, at PR time: the drift is caught where
# it is introduced rather than in a release run that has already published a
# draft. This reads the ENTERPRISE tree's copy; the workflow reads the exported
# one. Two files in two repositories, so they can still disagree — which is why
# the workflow's failure names the file's size and hash. This check makes that
# disagreement rare, not impossible.
#
# Usage: scripts/check-release-notes-sections.sh [--release <tag>] [notes-dir] [public-index]
#
# THE DRAFT MARKER (packaging design, "Note 2026-10-14 — a draft marker never
# reaches a release body"). An unreleased section says "Draft: not yet
# released." while it is written, and the CE release copies the section into
# the GitHub release body verbatim — 2026.10.5 shipped with the sentence in its
# live body. So a section whose EE notes carry a dated "**Release date:**" line
# must not contain the token "Draft:", and --release <tag> (run before tagging,
# RELEASE.md step 1) refuses it in that tag's section whatever its date.
set -euo pipefail

RELEASE_TAG=""
if [ "${1:-}" = "--release" ]; then
	[ -n "${2:-}" ] || { echo "::error::--release needs a tag"; exit 2; }
	RELEASE_TAG="$2"
	shift 2
fi

NOTES_DIR="${1:-docs/release-notes}"
INDEX="${2:-docs/public/release-notes/index.md}"

# #334 item 6 (2026-10-13): examining nothing is not a pass. A missing notes
# directory, or one holding no release note at or above the floor, fails.
[ -d "$NOTES_DIR" ] || { echo "::error::check-release-notes-sections: no $NOTES_DIR (examined 0 releases)"; exit 1; }
[ -f "$INDEX" ] || { echo "::error::$INDEX is missing — the CE release workflow has no body to publish from"; exit 1; }

# The extractor the CE workflow uses, kept identical on purpose: anchored on
# the tag TOKEN with a boundary so a titled heading still matches, stopping at
# the next top-level "## ".
section_lines() {
	awk -v tag="$1" '
		$0 ~ "^## " tag "([^0-9.]|$)" { f=1; next }
		/^## / { f=0 }
		f { print }
	' "$INDEX" | grep -c '[^[:space:]]' || true
}

# The section's text, for the draft-marker rule (same extractor as above).
section_text() {
	awk -v tag="$1" '
		$0 ~ "^## " tag "([^0-9.]|$)" { f=1; next }
		/^## / { f=0 }
		f { print }
	' "$INDEX"
}

# The same regex as the publish backstop in
# scripts/public-ce-templates/publish-release.yml: change both together.
DRAFT_MARKER='(^|[^[:alnum:]_])Draft:'

# draft_marker_lines TAG — the section's lines carrying the marker, numbered.
draft_marker_lines() {
	section_text "$1" | grep -nE "$DRAFT_MARKER" || true
}

# From the 2026.12 line on, every release line has a codename and its story
# (operator 2026-11-25: 2026.11 was already released without one).
CODENAME_FLOOR="2026.12.0"
STORY_MIN_WORDS=60

# older_than A B — A sorts strictly before B by version.
older_than() {
	[ "$1" = "$2" ] && return 1
	[ "$(printf '%s\n%s\n' "$1" "$2" | sort -V | sed -n 1p)" = "$1" ]
}

if [ -n "$RELEASE_TAG" ]; then
	if [ "$(section_lines "$RELEASE_TAG")" -eq 0 ]; then
		echo "::error::no '## $RELEASE_TAG' section in $INDEX — the CE release would refuse to publish"
		exit 1
	fi
	hits="$(draft_marker_lines "$RELEASE_TAG")"
	if [ -n "$hits" ]; then
		echo "::error::the '## $RELEASE_TAG' section in $INDEX still carries the draft marker; it would be published in the release body:"
		printf '%s\n' "$hits" | sed 's/^/    /'
		exit 1
	fi
	# The EE notes of the tag being cut (audit 2026-10-14, #334 delta audit):
	# release.yaml publishes this file verbatim as the release body, so it
	# must exist, carry a dated release date, and carry no draft banner.
	# 2026.10.5's notes still open with "> **Draft.** Nothing here is tagged".
	ee="$NOTES_DIR/$RELEASE_TAG.md"
	if [ ! -f "$ee" ]; then
		echo "::error::no $ee — release.yaml publishes it as the release body"
		exit 1
	fi
	bad=0
	if ! grep -qE '^\*\*Release date:\*\* [0-9]{4}-[0-9]{2}-[0-9]{2}' "$ee"; then
		echo "::error::$ee has no dated '**Release date:** YYYY-MM-DD' line; it would be published as the release body:"
		{ grep -nE '^\*\*Release date:\*\*' "$ee" || echo "0:(no release-date line)"; } | sed 's/^/    /'
		bad=1
	fi
	banner="$(grep -nE '^[[:space:]>]*\*\*Draft\.?\*\*' "$ee" || true)"
	if [ -n "$banner" ]; then
		echo "::error::$ee still carries the draft banner; it would be published in the release body:"
		printf '%s\n' "$banner" | sed 's/^/    /'
		bad=1
	fi
	# The codename and its story (packaging design, Amendment 2026-11-25):
	# from CODENAME_FLOOR on, the heading carries " — <Name>" and the section
	# a "### The name" subsection of at least STORY_MIN_WORDS words, the
	# story of the word that reaches the GitHub release body.
	if ! older_than "$RELEASE_TAG" "$CODENAME_FLOOR"; then
		if ! grep -qE "^## $RELEASE_TAG — [A-Z][A-Za-z-]+" "$INDEX"; then
			echo "::error::the '## $RELEASE_TAG' heading in $INDEX has no codename; from $CODENAME_FLOOR it reads '## $RELEASE_TAG — <Name>'"
			bad=1
		fi
		words="$(section_text "$RELEASE_TAG" | awk '/^### The name/{f=1; next} /^###? /{f=0} f' | wc -w)"
		if [ "$words" -lt "$STORY_MIN_WORDS" ]; then
			echo "::error::the '## $RELEASE_TAG' section in $INDEX tells the codename's story in $words words; a '### The name' subsection of at least $STORY_MIN_WORDS words is required (where the word comes from, what it means, why it matters, historic facts)"
			bad=1
		fi
	fi
	[ "$bad" -eq 0 ] || exit 1
	echo "check-release-notes-sections: OK — '## $RELEASE_TAG' is present and carries no draft marker; $ee is dated and carries no draft banner"
	exit 0
fi

# THE FLOOR, and why there is one. Releases older than the public repository's
# own history are deliberately collapsed into a single "## Earlier releases"
# rollup rather than given a section each — an editorial choice, not a gap, and
# those versions were never published to EaseIT-cz/vornik at all. Requiring a
# section for them would make this check fire permanently on history nobody is
# going to rewrite.
#
# The floor is DERIVED from the index rather than hardcoded, so curating an
# older release into its own section lowers it automatically and nobody has to
# remember to edit a constant here.
FLOOR="$(grep -oE '^## 20[0-9]{2}\.[0-9]+\.[0-9]+' "$INDEX" | sed 's/^## //' | sort -V | sed -n 1p)"
[ -n "$FLOOR" ] || { echo "::error::$INDEX has no release sections at all"; exit 1; }

older_than_floor() {
	[ "$1" = "$FLOOR" ] && return 1
	[ "$(printf '%s\n%s\n' "$1" "$FLOOR" | sort -V | sed -n 1p)" = "$1" ]
}

missing=0
checked=0
skipped=0
for f in "$NOTES_DIR"/*.md; do
	[ -e "$f" ] || continue
	tag="$(basename "$f" .md)"
	case "$tag" in
		20[0-9][0-9].*) ;;
		*) continue ;;
	esac
	if older_than_floor "$tag"; then
		skipped=$((skipped + 1))
		continue
	fi
	checked=$((checked + 1))
	if [ "$(section_lines "$tag")" -eq 0 ]; then
		echo "::error::no '## $tag' section in $INDEX — the CE release for $tag would refuse to publish"
		missing=$((missing + 1))
		continue
	fi
	if grep -qE '^\*\*Release date:\*\* [0-9]{4}-[0-9]{2}-[0-9]{2}' "$f"; then
		hits="$(draft_marker_lines "$tag")"
		if [ -n "$hits" ]; then
			echo "::error::'## $tag' in $INDEX is released ($f has a release date) but still carries the draft marker, which the CE release copies into its body:"
			printf '%s\n' "$hits" | sed 's/^/    /'
			missing=$((missing + 1))
		fi
	fi
done

# A nested "##" inside a section truncates the extracted body silently, so the
# convention forbids it (subsections are "###"). Linted rather than trusted:
# the failure is invisible in the published release, which just looks short.
# "## Earlier releases (…)" is the rollup the floor above refers to and is an
# intended top-level heading, not a nested one — it ends the last real section
# rather than truncating it. Anything ELSE at this level does truncate.
ALLOWED='^[0-9]+:## (20[0-9][0-9]\.|Release Notes|Earlier releases)'
if grep -nE '^## ' "$INDEX" | grep -vE "$ALLOWED" >/dev/null; then
	echo "::error::$INDEX has a '## ' heading that is not a release section:"
	grep -nE '^## ' "$INDEX" | grep -vE "$ALLOWED" | sed 's/^/    /'
	echo "    A nested '## ' truncates the extracted release body. Use '### ' for subsections."
	missing=$((missing + 1))
fi

# A stale undated release (docs-publishing design, Amendment 2026-11-25). The
# docs build hides a section whose EE notes carry no dated release line, so
# an undated version BELOW the newest dated one is a released version the
# site would silently drop (2026.7.7, tagged 2026-07-29, still said
# "unreleased" in 2026-11). Only versions after the newest dated one may be
# undated: they are the release in preparation.
newest_dated=""
for f in "$NOTES_DIR"/*.md; do
	tag="$(basename "$f" .md)"
	case "$tag" in 20[0-9][0-9].*) ;; *) continue ;; esac
	grep -qE '^\*\*Release date:\*\* [0-9]{4}-[0-9]{2}-[0-9]{2}' "$f" || continue
	if [ -z "$newest_dated" ] || older_than "$newest_dated" "$tag"; then newest_dated="$tag"; fi
done
for f in "$NOTES_DIR"/*.md; do
	tag="$(basename "$f" .md)"
	case "$tag" in 20[0-9][0-9].*) ;; *) continue ;; esac
	older_than_floor "$tag" && continue
	grep -qE '^\*\*Release date:\*\* [0-9]{4}-[0-9]{2}-[0-9]{2}' "$f" && continue
	if [ -n "$newest_dated" ] && older_than "$tag" "$newest_dated"; then
		echo "::error::$f has no dated '**Release date:**' line, but $newest_dated after it is released; docs.vornik.io hides an undated section, so date it from its tag ($(grep -m1 -E '^\*\*Release date:\*\*' "$f" || echo 'no release-date line'))"
		missing=$((missing + 1))
	fi
done

# Older release lines are rolled up (packaging design, Amendment 2026-11-25):
# at most three released monthly lines keep full "## <tag>" sections. A line
# whose every section is unreleased (undated EE notes) is being prepared and
# does not count. Older lines go under "## Earlier releases".
MAX_FULL_LINES=3
full_lines="$(grep -oE '^## 20[0-9]{2}\.[0-9]+\.[0-9]+' "$INDEX" | sed 's/^## //' | while read -r t; do
	n="$NOTES_DIR/$t.md"
	if [ ! -f "$n" ] || grep -qE '^\*\*Release date:\*\* [0-9]{4}-[0-9]{2}-[0-9]{2}' "$n"; then
		printf '%s\n' "${t%.*}"
	fi
done | sort -uV)"
n_full="$(printf '%s' "$full_lines" | grep -c . || true)"
if [ "$n_full" -gt "$MAX_FULL_LINES" ]; then
	oldest="$(printf '%s\n' "$full_lines" | sed -n 1p)"
	echo "::error::$INDEX writes out $n_full released lines in full ($(printf '%s' "$full_lines" | tr '\n' ' ')); at most $MAX_FULL_LINES are kept. Roll $oldest.x up under '## Earlier releases' (one bullet per release, Upgrade requirements kept verbatim)"
	missing=$((missing + 1))
fi

# Issue numbers in the public notes: scripts/check-public-issue-refs.sh,
# which covers every public page (one check, not two).

if [ "$checked" -eq 0 ]; then
	echo "::error::check-release-notes-sections: examined 0 release(s) in $NOTES_DIR ($skipped older than the floor $FLOOR); a gate that looked at nothing has not passed"
	missing=$((missing + 1))
fi

if [ "$missing" -gt 0 ]; then
	echo "check-release-notes-sections: $missing problem(s) across $checked release(s)"
	exit 1
fi
echo "check-release-notes-sections: OK — $checked release(s) have an extractable public section (floor $FLOOR; $skipped older, covered by the rollup)"
