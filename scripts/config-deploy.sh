#!/usr/bin/env bash
#
# config-deploy.sh — copy the repo's deployable config assets into the deployed
# tree the daemon reads (LLD 2026-07-16 §4.2). Driven by the ONE manifest
# (config-deployable.sh) so a new subtree ships the moment it is listed — the
# fix for role-library never being copied (2026-07-16) and workflows before it.
#
# Preserve-existing: an operator-tuned deployed file is never clobbered
# (recursive, per-file). After copying, a self-check verifies every deployable
# landed on disk (a COPY-I/O guard — not a content-drift check; content drift is
# config-drift-check.sh's job). Under STRICT_CONFIG_DEPLOY=1 a missing/empty
# canonical subtree aborts the install instead of warning.
#
# Usage: scripts/config-deploy.sh <target-config-dir>
#        (writes into <target-config-dir>/configs/)
# Env:   VORNIK_REPO_CONFIGS_DIR   repo configs/ source (default <repo>/configs)
#        STRICT_CONFIG_DEPLOY=1     abort on a missing deployable after copy
#        VORNIK_DEPLOY_REVISION     the revision being deployed; stamps .templates
# Exit:  0 = all deployables present after copy, 1 = usage error or (strict) a
#        deployable missing/empty on disk.
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=scripts/config-deployable.sh
. "$SCRIPT_DIR/config-deployable.sh"

REPO_CONFIGS="${VORNIK_REPO_CONFIGS_DIR:-$(cd "$SCRIPT_DIR/.." && pwd)/configs}"
TARGET="${1:-}"
if [ -z "$TARGET" ]; then
	echo "config-deploy: usage: config-deploy.sh <target-config-dir>" >&2
	exit 1
fi
DEST="$TARGET/configs"
if [ -L "$DEST" ]; then
	echo "config-deploy: refusing symlinked deployed configs dir: $DEST" >&2
	exit 1
fi
mkdir -p "$DEST"

# Strict policy: deployed config paths must be real directories/files, not
# symlinks. The daemon reads the deployed tree as configuration authority; a
# symlink can redirect a copy or validation check outside that tree. This
# portable shell guard narrows accidental/misconfigured trees, but it is not an
# openat/O_NOFOLLOW implementation. The deployment target is expected to be an
# operator-owned config directory, not a concurrently attacker-writable tree.
path_has_symlink_component() {
	local path="$1" rel cur part
	rel="${path#"$DEST"}"
	rel="${rel#/}"
	cur="$DEST"
	[ -L "$cur" ] && return 0
	[ -z "$rel" ] && return 1
	local IFS='/'
	for part in $rel; do
		cur="$cur/$part"
		[ -L "$cur" ] && return 0
	done
	return 1
}

refuse_unsafe_dest_path() {
	local path="$1"
	if [ -L "$path" ]; then
		echo "config-deploy: refusing symlinked deployed path: $path" >&2
		exit 1
	fi
	if path_has_symlink_component "$(dirname "$path")"; then
		echo "config-deploy: refusing deployed path with symlinked parent: $path" >&2
		exit 1
	fi
}

# --- config_template_drift baselines (drift design, third amendment) ---
# Preserve-existing is right (Vornik tunes the deployed tree) but it means a
# template FIX never arrives, and nothing said so. Two baselines let the
# daemon's config_template_drift check say it:
#   .templates/<rel>  the CURRENT template, overwritten every install, pruned
#                     to the manifest set, stamped with the deploying revision
#                     in .templates/.stamp (absent = unstamped, untrustworthy)
#   .origin/<rel>     the template as it stood when the deployed file was
#                     CREATED (or, for a pre-existing file IDENTICAL to the
#                     template being installed, seeded from it). Write-once;
#                     .origin/.index records rel, the deployed file's hash as
#                     created, the revision, the date, and created|seeded.
TEMPLATES="$DEST/.templates"
ORIGIN="$DEST/.origin"
ORIGIN_INDEX="$ORIGIN/.index"
DEPLOY_REV="${VORNIK_DEPLOY_REVISION:-}"
MANIFEST_SET="$(mktemp "${TMPDIR:-/tmp}/config-deploy-set.XXXXXX")"
trap 'rm -f "$MANIFEST_SET" "$MANIFEST_SET".*' EXIT
refuse_unsafe_dest_path "$TEMPLATES"
refuse_unsafe_dest_path "$ORIGIN"
mkdir -p "$TEMPLATES" "$ORIGIN"
touch "$ORIGIN_INDEX"

# norm_hash <file> — sha256 of the file with CR removed, so a CRLF copy of a
# template hashes like the template. sha256sum (GNU, BusyBox) or shasum -a 256
# (macOS). With NEITHER, .origin is not written at all: an empty hash would make
# every file look identical to its template and seed a wrong, write-once origin.
if command -v sha256sum >/dev/null 2>&1; then
	norm_hash() { tr -d '\r' < "$1" | sha256sum | cut -d' ' -f1; }
elif command -v shasum >/dev/null 2>&1; then
	norm_hash() { tr -d '\r' < "$1" | shasum -a 256 | cut -d' ' -f1; }
else
	norm_hash() { return 1; }
	echo "WARN: config-deploy: no sha256 tool; .origin baselines not written (config_template_drift will report the vague form)" >&2
fi

# record_template <rel> <src> — overwrite the current-template baseline.
record_template() {
	local rel="$1" src="$2"
	printf '%s\n' "$rel" >> "$MANIFEST_SET"
	refuse_unsafe_dest_path "$TEMPLATES/$rel"
	mkdir -p "$(dirname "$TEMPLATES/$rel")"
	cp "$src" "$TEMPLATES/$rel" || echo "WARN: baseline copy failed: $rel" >&2
}

origin_has() { awk -F'\t' -v r="$1" '$1 == r { found = 1 } END { exit found ? 0 : 1 }' "$ORIGIN_INDEX"; }

# write_origin <rel> <src> <deployed> <created|seeded> — record where a deployed
# file came from. Replaces any entry for rel (a created file is new).
write_origin() {
	local rel="$1" src="$2" deployed="$3" how="$4" tmp hash
	hash="$(norm_hash "$deployed")" && [ -n "$hash" ] || return 0
	refuse_unsafe_dest_path "$ORIGIN/$rel"
	mkdir -p "$(dirname "$ORIGIN/$rel")"
	cp "$src" "$ORIGIN/$rel" || { echo "WARN: origin copy failed: $rel" >&2; return; }
	tmp="$ORIGIN_INDEX.tmp.$$"
	awk -F'\t' -v r="$rel" '$1 != r' "$ORIGIN_INDEX" > "$tmp"
	printf '%s\t%s\t%s\t%s\t%s\n' "$rel" "$hash" "${DEPLOY_REV:-unknown}" \
		"$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$how" >> "$tmp"
	replace_file "$tmp" "$ORIGIN_INDEX"
}

# replace_file <tmp> <dest> — rename in the same directory, so a crash leaves
# the old or the new file, never a truncated one (round 8 N3: the index and
# .removed are rewritten in place, not strictly appended).
replace_file() { mv -f "$1" "$2"; }

# deploy_one <rel> <src> <dest> — copy if absent (preserve-existing), and keep
# both baselines. .origin: on CREATE, or seeded on a preserve-existing hit when
# no entry exists and the deployed file is identical to this template (it IS
# this template now, so the entry is true). A diverged file gets none — its
# origin is unknowable, and the check reports it as permanently vague.
deploy_one() {
	local rel="$1" src="$2" dest="$3"
	record_template "$rel" "$src"
	if [ ! -e "$dest" ]; then
		mkdir -p "$(dirname "$dest")"
		if cp "$src" "$dest"; then
			write_origin "$rel" "$src" "$dest" created
		else
			echo "WARN: cp failed: $rel" >&2
		fi
	elif ! origin_has "$rel"; then
		local dh sh
		dh="$(norm_hash "$dest")" && sh="$(norm_hash "$src")" && [ -n "$dh" ] && [ "$dh" = "$sh" ] &&
			write_origin "$rel" "$src" "$dest" seeded
	fi
}

# --- copy deployable directories (portable per-file recursive, preserve-existing) ---
# Per-file rather than `cp -rn`: BusyBox cp has no -n (would error and, with
# stderr suppressed + `|| true`, silently copy NOTHING), and older GNU `cp -n`
# skips existing dirs without descending (a NEW file in an existing deployed
# subdir would never land). This loop copies each repo file only if absent —
# portable, descends, preserves operator-tuned files, adds new ones. cp errors
# surface (not suppressed) so a genuine failure is visible.
for dir in "${CONFIG_DEPLOYABLE_DIRS[@]}"; do
	src="$REPO_CONFIGS/$dir"
	[ -d "$src" ] || continue
	refuse_unsafe_dest_path "$DEST/$dir"
	if [ -e "$DEST/$dir" ] && [ ! -d "$DEST/$dir" ]; then
		echo "config-deploy: refusing non-directory deployed path: $DEST/$dir" >&2
		exit 1
	fi
	mkdir -p "$DEST/$dir"
	while IFS= read -r rel; do
		rel="${rel#./}"
		dest="$DEST/$dir/$rel"
		refuse_unsafe_dest_path "$dest"
		deploy_one "$dir/$rel" "$src/$rel" "$dest"
	done < <(cd "$src" && find . -type f)
done

# --- copy deployable top-level files (preserve-existing) ---
for file in "${CONFIG_DEPLOYABLE_FILES[@]}"; do
	src="$REPO_CONFIGS/$file"
	[ -f "$src" ] || continue
	refuse_unsafe_dest_path "$DEST/$file"
	deploy_one "$file" "$src" "$DEST/$file"
done

# --- prune both baselines to the manifest set; stamp the current one ---
# A template file that left the manifest must not linger as a baseline, and an
# .origin entry for a path that left must not survive to mis-classify a later
# re-add of the same path (drift design round 5 F4).
# .templates/.removed: a template file this install prunes is a file the
# product STOPPED shipping; the daemon cannot tell it from an operator's own
# host-local file by inference, so the installer — the one party that knows —
# records it with the revision performing the removal. Persistent history:
# an entry is dropped only when the path is re-added to the manifest or the
# deployed copy is gone.
REMOVED="$TEMPLATES/.removed"
touch "$REMOVED"
while IFS= read -r rel; do
	rel="${rel#./}"
	case "$rel" in
		.stamp|.classes|.removed) continue ;;
		# A temp file left by a crashed install is not a template the product
		# stopped shipping: delete it, record nothing.
		.*.tmp.*) rm -f "$TEMPLATES/$rel"; continue ;;
	esac
	if ! grep -qxF -- "$rel" "$MANIFEST_SET"; then
		rm -f "$TEMPLATES/$rel"
		awk -F'\t' -v r="$rel" '$1 == r { f = 1 } END { exit f ? 0 : 1 }' "$REMOVED" ||
			printf '%s\t%s\t%s\n' "$rel" "${DEPLOY_REV:-unknown}" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" >> "$REMOVED"
	fi
done < <(cd "$TEMPLATES" && find . -type f)
awk -F'\t' -v dest="$DEST" 'NR == FNR { inset[$0] = 1; next }
	!($1 in inset) { f = dest "/" $1; if ((getline line < f) >= 0) { close(f); print } }' \
	"$MANIFEST_SET" "$REMOVED" > "$REMOVED.tmp.$$"
replace_file "$REMOVED.tmp.$$" "$REMOVED"

# .templates/.classes: the manifest's tunable axis, recorded for the daemon —
# a CE box has no shell manifest, and a Go copy would be a second manifest. A
# directory is tunable iff it is in CONFIG_TUNABLE_DIRS; every top-level file
# is canonical (the axis names only directories; pricing.yaml is a catalog).
: > "$TEMPLATES/.classes"
for dir in "${CONFIG_DEPLOYABLE_DIRS[@]}"; do
	cls=canonical; config_is_tunable "$dir" && cls=tunable
	printf '%s\t%s\tdir\n' "$dir" "$cls" >> "$TEMPLATES/.classes"
done
for file in "${CONFIG_DEPLOYABLE_FILES[@]}"; do
	printf '%s\tcanonical\tfile\n' "$file" >> "$TEMPLATES/.classes"
done
find "$TEMPLATES" -mindepth 1 -type d -empty -delete 2>/dev/null || true
awk -F'\t' 'NR == FNR { keep[$0] = 1; next } ($1 in keep)' "$MANIFEST_SET" "$ORIGIN_INDEX" > "$ORIGIN_INDEX.tmp.$$"
# Rename BEFORE the sweep below, which deletes every file in .origin that is
# not a manifest path — the temp file included.
replace_file "$ORIGIN_INDEX.tmp.$$" "$ORIGIN_INDEX"
while IFS= read -r rel; do
	rel="${rel#./}"
	[ "$rel" = ".index" ] || [ "$rel" = ".acks.journal" ] && continue
	grep -qxF -- "$rel" "$MANIFEST_SET" || rm -f "$ORIGIN/$rel"
done < <(cd "$ORIGIN" && find . -type f)
find "$ORIGIN" -mindepth 1 -type d -empty -delete 2>/dev/null || true
if [ -n "$DEPLOY_REV" ]; then
	printf '%s\n' "$DEPLOY_REV" > "$TEMPLATES/.stamp"
else
	# An install that cannot say which revision it is must not leave a
	# previous stamp vouching for the content it just wrote.
	rm -f "$TEMPLATES/.stamp"
fi

# --- self-check: deployables landed on disk (copy-I/O guard, not content) ---
# Canonical (non-tunable) dirs get PER-FILE completeness — every repo file must
# exist in the deployed tree (catches a partial copy: some files land, others
# don't). Tunable dirs get the looser any-file check: it tolerates a partial
# I/O failure on a non-daemon-critical dir (swarms/workflows/templates) rather
# than aborting the whole install. (Note: the copy loop restores any absent
# file, so operator-pruning of a tunable file does not survive install anyway —
# the looser check is about I/O tolerance, not preserving deletions.)
missing=""
for dir in "${CONFIG_DEPLOYABLE_DIRS[@]}"; do
	[ -d "$REPO_CONFIGS/$dir" ] || continue
	# skip dirs with no repo content — an empty repo subtree ships nothing
	[ -n "$(find "$REPO_CONFIGS/$dir" -type f 2>/dev/null | head -1)" ] || continue
	if config_is_tunable "$dir"; then
		[ -n "$(find "$DEST/$dir" -type f 2>/dev/null | head -1)" ] || missing="$missing $dir/"
	else
		while IFS= read -r rel; do
			rel="${rel#./}"
			refuse_unsafe_dest_path "$DEST/$dir/$rel"
			[ -f "$DEST/$dir/$rel" ] || missing="$missing $dir/$rel"
		done < <(cd "$REPO_CONFIGS/$dir" && find . -type f)
	fi
done
for file in "${CONFIG_DEPLOYABLE_FILES[@]}"; do
	[ -f "$REPO_CONFIGS/$file" ] || continue
	refuse_unsafe_dest_path "$DEST/$file"
	[ -f "$DEST/$file" ] || missing="$missing $file"
done

if [ -n "$missing" ]; then
	msg="config-deploy: deployable(s) missing on disk after copy:$missing"
	if [ "${STRICT_CONFIG_DEPLOY:-0}" = "1" ]; then
		echo "$msg" >&2
		echo "config-deploy: aborting (STRICT_CONFIG_DEPLOY=1). A canonical config subtree did not deploy." >&2
		exit 1
	fi
	echo "WARN: $msg" >&2
	echo "config-deploy: continuing (set STRICT_CONFIG_DEPLOY=1 to make this fatal)." >&2
fi

echo "config-deploy: deployed config assets into $DEST"
exit 0
