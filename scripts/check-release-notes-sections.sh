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

if [ "$checked" -eq 0 ]; then
	echo "::error::check-release-notes-sections: examined 0 release(s) in $NOTES_DIR ($skipped older than the floor $FLOOR); a gate that looked at nothing has not passed"
	missing=$((missing + 1))
fi

if [ "$missing" -gt 0 ]; then
	echo "check-release-notes-sections: $missing problem(s) across $checked release(s)"
	exit 1
fi
echo "check-release-notes-sections: OK — $checked release(s) have an extractable public section (floor $FLOOR; $skipped older, covered by the rollup)"
