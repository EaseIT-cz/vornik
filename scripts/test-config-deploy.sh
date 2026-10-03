#!/usr/bin/env bash
#
# test-config-deploy.sh — tests for config-deploy.sh (LLD 2026-07-16 §4.2/§6).
# The installer copies every manifest-listed deployable subtree into the
# deployed tree, preserves operator-tuned files, and (strict) aborts if a
# canonical subtree is missing after copy.
#
# Self-contained; drives config-deploy.sh against a fixture repo configs/ and a
# temp target dir. No bats.
#
# Usage: scripts/test-config-deploy.sh
# Exit:  0 = all pass, 1 = a case failed
set -u

HERE="$(cd "$(dirname "$0")" && pwd)"
DEPLOY="$HERE/config-deploy.sh"
TMP="$(mktemp -d "${TMPDIR:-/tmp}/cfgdeploy-test.XXXXXX")"
trap 'rm -rf "$TMP"' EXIT

fails=0
pass() { echo "ok   - $1"; }
fail() { echo "FAIL - $1"; fails=$((fails+1)); }

build_repo() {
	REPO="$TMP/repo/configs"; rm -rf "$TMP/repo"; mkdir -p "$REPO"
	for d in swarms workflows role-library; do mkdir -p "$REPO/$d"; done
	echo "coder"   > "$REPO/role-library/coder.md"
	echo "lead"    > "$REPO/role-library/lead.md"
	echo "dev"     > "$REPO/swarms/dev-swarm.md"
	echo "adaptive"> "$REPO/workflows/adaptive.md"
	mkdir -p "$REPO/project-templates/blog"
	echo "tmpl"    > "$REPO/project-templates/blog/project.md"
	echo "pricing" > "$REPO/pricing.yaml"
}

run_deploy() { VORNIK_REPO_CONFIGS_DIR="$REPO" "$DEPLOY" "$TGT" 2>&1; }

# --- Case 1 (regression: role-library was never copied 2026-07-16): a fresh
# install copies every deployable subtree, including role-library. ---
build_repo
TGT="$TMP/t1"; rm -rf "$TGT"
out="$(run_deploy)"; rc=$?
if [ "$rc" -eq 0 ] \
	&& [ -f "$TGT/configs/role-library/coder.md" ] \
	&& [ -f "$TGT/configs/role-library/lead.md" ] \
	&& [ -f "$TGT/configs/swarms/dev-swarm.md" ] \
	&& [ -f "$TGT/configs/workflows/adaptive.md" ] \
	&& [ -f "$TGT/configs/project-templates/blog/project.md" ] \
	&& [ -f "$TGT/configs/pricing.yaml" ]; then
	pass "fresh install copies all deployable subtrees incl role-library + templates + pricing"
else
	fail "fresh install missing a deployable (rc=$rc); got: $out; tree: $(find "$TGT" -type f 2>/dev/null | sort | tr '\n' ' ')"
fi

# --- Case 2: preserve-AND-add — an operator-tuned deployed file survives, AND a
# new repo file lands next to it (older-GNU `cp -n` skip-don't-descend would
# preserve coder.md but silently drop the new lead.md; review finding). ---
build_repo
TGT="$TMP/t2"; rm -rf "$TGT"; mkdir -p "$TGT/configs/role-library"
echo "OPERATOR TUNED" > "$TGT/configs/role-library/coder.md"   # pre-existing, tuned
run_deploy >/dev/null 2>&1
got="$(cat "$TGT/configs/role-library/coder.md" 2>/dev/null)"
if [ "$got" = "OPERATOR TUNED" ] && [ -f "$TGT/configs/role-library/lead.md" ]; then
	pass "preserve-existing keeps coder.md AND adds the new lead.md"
else
	fail "preserve+add failed: coder.md='$got', lead.md present=$([ -f "$TGT/configs/role-library/lead.md" ] && echo yes || echo NO)"
fi

# --- Case 3 (strict): a real copy-I/O failure — repo HAS role-library content
# but it can't land on disk (unwritable target subdir) — aborts under
# STRICT_CONFIG_DEPLOY=1. This is the copy-I/O guard the self-check exists for.
# Skipped as root (chmod 500 doesn't block root writes -> would false-red). ---
if [ "$(id -u)" -eq 0 ]; then
	echo "skip - strict copy-I/O case (running as root; chmod 500 doesn't block root)"
else
	build_repo                                # repo role-library has coder.md, lead.md
	TGT="$TMP/t3"; rm -rf "$TGT"; mkdir -p "$TGT/configs/role-library"
	chmod 500 "$TGT/configs/role-library"     # r-x, no write -> cp cannot populate it
	out="$(STRICT_CONFIG_DEPLOY=1 VORNIK_REPO_CONFIGS_DIR="$REPO" "$DEPLOY" "$TGT" 2>&1)"; rc=$?
	chmod 700 "$TGT/configs/role-library" 2>/dev/null || true   # restore for cleanup
	if [ "$rc" -ne 0 ] && printf '%s' "$out" | grep -q "role-library"; then
		pass "strict mode aborts on a real copy-I/O failure (role-library couldn't land)"
	else
		fail "strict mode should abort naming role-library (rc=$rc); got: $out"
	fi
fi

# --- Case 4 (review finding 4): partial copy of a canonical dir fails the
# per-file self-check under strict (one file lands, another doesn't). ---
if [ "$(id -u)" -eq 0 ]; then
	echo "skip - partial-copy self-check case (running as root)"
else
	build_repo
	TGT="$TMP/t4"; rm -rf "$TGT"; mkdir -p "$TGT/configs/role-library"
	# Pre-place coder.md so it's preserved, then block new writes: lead.md can't land.
	echo "kept" > "$TGT/configs/role-library/coder.md"
	chmod 500 "$TGT/configs/role-library"
	out="$(STRICT_CONFIG_DEPLOY=1 VORNIK_REPO_CONFIGS_DIR="$REPO" "$DEPLOY" "$TGT" 2>&1)"; rc=$?
	chmod 700 "$TGT/configs/role-library" 2>/dev/null || true
	if [ "$rc" -ne 0 ] && printf '%s' "$out" | grep -q "role-library"; then
		pass "strict per-file self-check catches a PARTIAL canonical copy (lead.md missing)"
	else
		fail "partial canonical copy should abort under strict (rc=$rc); got: $out"
	fi
fi

# --- Case 5 (security): deployed config entries must not be symlinks. A
# symlinked destination file would make deploy follow/validate an arbitrary
# target outside the config tree. Refuse even outside strict mode. ---
build_repo
TGT="$TMP/t5"; rm -rf "$TGT"; mkdir -p "$TGT/configs/role-library"
outside="$TMP/outside-coder.md"; echo "outside" > "$outside"
ln -s "$outside" "$TGT/configs/role-library/coder.md"
out="$(run_deploy)"; rc=$?
if [ "$rc" -ne 0 ] && printf '%s' "$out" | grep -q "refusing symlinked deployed path"; then
	pass "refuses symlinked deployed file destination"
else
	fail "symlinked deployed file should be refused (rc=$rc); got: $out"
fi

# --- Case 6 (security): symlinked deployed directories are refused before
# mkdir/cp can follow them and write outside the target config tree. ---
build_repo
TGT="$TMP/t6"; rm -rf "$TGT"; mkdir -p "$TGT/configs" "$TMP/outside-role-library"
ln -s "$TMP/outside-role-library" "$TGT/configs/role-library"
out="$(run_deploy)"; rc=$?
if [ "$rc" -ne 0 ] && printf '%s' "$out" | grep -q "refusing symlinked deployed path"; then
	pass "refuses symlinked deployed directory destination"
else
	fail "symlinked deployed directory should be refused (rc=$rc); got: $out"
fi

# ===================================================================
# config_template_drift baselines (drift design, third amendment, 2026-09-24).
# The deployed tree is preserve-existing, so a template FIX never arrives and
# nothing says so. The installer therefore writes two baselines beside the
# deployed files: .templates/ (the CURRENT template, overwritten, stamped,
# pruned) and .origin/ (the template as it stood when a deployed file was
# created, write-once, with the created file's hash). The daemon's
# config_template_drift check compares against them.
# ===================================================================

sha() { tr -d '\r' < "$1" | sha256sum | cut -d' ' -f1; }
run_deploy_rev() { VORNIK_DEPLOY_REVISION="$1" VORNIK_REPO_CONFIGS_DIR="$REPO" "$DEPLOY" "$TGT" >/dev/null 2>&1; }

# --- Case 7: .templates holds the CURRENT template for every manifest file,
# and is overwritten on re-install even though the deployed file is preserved.
build_repo
TGT="$TMP/t7"; rm -rf "$TGT"
run_deploy_rev "rev-one"
echo "OPERATOR TUNED" > "$TGT/configs/workflows/adaptive.md"
echo "adaptive v2 (template fix)" > "$REPO/workflows/adaptive.md"
run_deploy_rev "rev-two"
if [ "$(cat "$TGT/configs/.templates/workflows/adaptive.md")" = "adaptive v2 (template fix)" ] \
	&& [ "$(cat "$TGT/configs/workflows/adaptive.md")" = "OPERATOR TUNED" ] \
	&& [ -f "$TGT/configs/.templates/pricing.yaml" ] \
	&& [ -f "$TGT/configs/.templates/project-templates/blog/project.md" ] \
	&& [ -f "$TGT/configs/.templates/role-library/coder.md" ]; then
	pass ".templates is the current template for every manifest file; the deployed file is preserved"
else
	fail ".templates not current or incomplete: $(find "$TGT/configs/.templates" -type f | sort | tr '\n' ' ')"
fi

# --- Case 8: the stamp is the deploying revision; an install with no revision
# REMOVES a previous stamp (the baseline now reflects an unknown revision).
if [ "$(cat "$TGT/configs/.templates/.stamp" 2>/dev/null)" = "rev-two" ]; then
	pass ".templates/.stamp records the deploying revision"
else
	fail ".templates/.stamp = '$(cat "$TGT/configs/.templates/.stamp" 2>/dev/null)', want rev-two"
fi
run_deploy_rev ""
if [ ! -e "$TGT/configs/.templates/.stamp" ]; then
	pass "an unstamped install removes the old stamp rather than leaving it to vouch for new content"
else
	fail "unstamped install kept a stale stamp: $(cat "$TGT/configs/.templates/.stamp")"
fi

# --- Case 9: .templates is pruned to the manifest set; a vanished template file
# does not linger as a baseline.
rm "$REPO/role-library/lead.md"
run_deploy_rev "rev-three"
if [ ! -e "$TGT/configs/.templates/role-library/lead.md" ] && [ -f "$TGT/configs/.templates/role-library/coder.md" ]; then
	pass ".templates is pruned when a template file leaves the manifest"
else
	fail ".templates kept a vanished template: $(find "$TGT/configs/.templates/role-library" -type f | tr '\n' ' ')"
fi

# --- Case 10: .origin is written when the installer CREATES a deployed file,
# with the created file's hash and the revision.
build_repo
TGT="$TMP/t10"; rm -rf "$TGT"
run_deploy_rev "rev-a"
idx="$TGT/configs/.origin/.index"
line="$(grep -P '^workflows/adaptive\.md\t' "$idx" 2>/dev/null)"
want_hash="$(sha "$TGT/configs/workflows/adaptive.md")"
if [ -f "$TGT/configs/.origin/workflows/adaptive.md" ] \
	&& [ "$(printf '%s' "$line" | cut -f2)" = "$want_hash" ] \
	&& [ "$(printf '%s' "$line" | cut -f3)" = "rev-a" ] \
	&& [ "$(printf '%s' "$line" | cut -f5)" = "created" ]; then
	pass ".origin written on create, indexed with the created hash, revision and 'created'"
else
	fail ".origin on create: line='$line' want hash $want_hash"
fi

# --- Case 11: a preserve-existing hit writes NO origin for a DIVERGED file, and
# SEEDS one for a file IDENTICAL to the template (CRLF-tolerant).
build_repo
TGT="$TMP/t11"; rm -rf "$TGT"; mkdir -p "$TGT/configs/workflows" "$TGT/configs/swarms"
echo "operator's own adaptive" > "$TGT/configs/workflows/adaptive.md"   # diverged
printf 'dev\r\n' > "$TGT/configs/swarms/dev-swarm.md"                       # identical but CRLF
run_deploy_rev "rev-b"
idx="$TGT/configs/.origin/.index"
if ! grep -qP '^workflows/adaptive\.md\t' "$idx" 2>/dev/null \
	&& [ ! -e "$TGT/configs/.origin/workflows/adaptive.md" ] \
	&& [ "$(grep -P '^swarms/dev-swarm\.md\t' "$idx" | cut -f5)" = "seeded" ]; then
	pass "diverged pre-existing file gets no origin; an identical (CRLF) one is seeded"
else
	fail "seed rule: index=$(tr '\n' '|' < "$idx" 2>/dev/null)"
fi

# --- Case 12: .origin is WRITE-ONCE — a later template change does not rewrite it.
before="$(cat "$TGT/configs/.origin/swarms/dev-swarm.md")"
echo "dev v2" > "$REPO/swarms/dev-swarm.md"
run_deploy_rev "rev-c"
if [ "$(cat "$TGT/configs/.origin/swarms/dev-swarm.md")" = "$before" ] \
	&& [ "$(grep -P '^swarms/dev-swarm\.md\t' "$TGT/configs/.origin/.index" | cut -f3)" = "rev-b" ]; then
	pass ".origin is never overwritten by a later install"
else
	fail ".origin was rewritten: $(cat "$TGT/configs/.origin/swarms/dev-swarm.md")"
fi

# --- Case 13: .origin is pruned when a path leaves the manifest, and a RE-ADD
# writes a fresh entry under the same rules (drift design round 5 F4).
rm "$REPO/swarms/dev-swarm.md"
run_deploy_rev "rev-d"
gone=0; grep -qP '^swarms/dev-swarm\.md\t' "$TGT/configs/.origin/.index" || gone=1
echo "dev v3 re-added" > "$REPO/swarms/dev-swarm.md"
rm "$TGT/configs/swarms/dev-swarm.md"
run_deploy_rev "rev-e"
readd="$(grep -P '^swarms/dev-swarm\.md\t' "$TGT/configs/.origin/.index" | cut -f3,5)"
if [ "$gone" = 1 ] && [ "$readd" = "$(printf 'rev-e\tcreated')" ] \
	&& [ "$(cat "$TGT/configs/.origin/swarms/dev-swarm.md")" = "dev v3 re-added" ]; then
	pass ".origin pruned on manifest exit; a re-add is written fresh"
else
	fail ".origin prune/re-add: gone=$gone readd='$readd'"
fi

# --- Case 14: an origin with no revision records 'unknown', not an empty field.
build_repo
TGT="$TMP/t14"; rm -rf "$TGT"
run_deploy_rev ""
if [ "$(grep -P '^workflows/adaptive\.md\t' "$TGT/configs/.origin/.index" | cut -f3)" = "unknown" ]; then
	pass "an unstamped create records revision 'unknown'"
else
	fail "unstamped origin revision: $(grep -P '^workflows/adaptive' "$TGT/configs/.origin/.index")"
fi

# --- Case 15: the baseline artifacts are named by ONE predicate the drift
# check shares, and a genuine host-only file is not.
. "$HERE/config-deployable.sh"
ok15=1
for p in .templates/workflows/x.md .origin/.index .origin/swarms/a.md .template-acks; do
	config_is_baseline_artifact "$p" || ok15=0
done
config_is_baseline_artifact "workflows/host-only.md" && ok15=0
if [ "$ok15" = 1 ]; then
	pass "config_is_baseline_artifact names the dot-trees and ack store, and not a real file"
else
	fail "config_is_baseline_artifact misclassified a path"
fi

# --- Case 16: with NO sha256 tool, .origin is not written at all — an empty
# hash would make every file look identical to its template and seed a wrong,
# write-once origin. The deploy itself and .templates still work.
build_repo
TGT="$TMP/t16"; rm -rf "$TGT"; mkdir -p "$TGT/configs/workflows" "$TMP/nohash"
echo "operator's own adaptive" > "$TGT/configs/workflows/adaptive.md"
for tool in bash tr cut awk cp mkdir find grep dirname date cat mktemp rm touch printf sort head env; do
	p="$(command -v "$tool" 2>/dev/null)" && ln -sf "$p" "$TMP/nohash/$tool"
done
out="$(PATH="$TMP/nohash" VORNIK_DEPLOY_REVISION=rev-x VORNIK_REPO_CONFIGS_DIR="$REPO" "$TMP/nohash/bash" "$DEPLOY" "$TGT" 2>&1)"; rc=$?
if [ "$rc" -eq 0 ] && [ ! -s "$TGT/configs/.origin/.index" ] \
	&& [ -f "$TGT/configs/.templates/workflows/adaptive.md" ] \
	&& printf '%s' "$out" | grep -q "no sha256 tool"; then
	pass "no sha256 tool: no .origin written, deploy and .templates still work, and it says so"
else
	fail "no-hash install: rc=$rc index=$(cat "$TGT/configs/.origin/.index" 2>/dev/null | tr '\n' '|') out=$out"
fi

# --- Case 17: .templates/.classes records the manifest's tunable axis: a dir
# is tunable iff in CONFIG_TUNABLE_DIRS, every top-level file is canonical.
build_repo
TGT="$TMP/t17"; rm -rf "$TGT"
run_deploy_rev "rev-k"
cls="$TGT/configs/.templates/.classes"
if [ "$(awk -F'\t' '$1=="workflows"{print $2"/"$3}' "$cls")" = "tunable/dir" ] \
	&& [ "$(awk -F'\t' '$1=="role-library"{print $2"/"$3}' "$cls")" = "canonical/dir" ] \
	&& [ "$(awk -F'\t' '$1=="pricing.yaml"{print $2"/"$3}' "$cls")" = "canonical/file" ]; then
	pass ".templates/.classes records tunable dirs, canonical dirs and canonical files"
else
	fail ".classes wrong: $(tr '\t\n' ' |' < "$cls" 2>/dev/null)"
fi

# --- Case 18: a template the product deletes is recorded in .templates/.removed
# with the revision that removed it, and the entry SURVIVES the next install's
# prune (round 7 F1); it is dropped once the deployed copy is gone.
build_repo
TGT="$TMP/t18"; rm -rf "$TGT"
run_deploy_rev "rev-1"
rm "$REPO/workflows/adaptive.md"
run_deploy_rev "rev-2"
first="$(awk -F'\t' '$1=="workflows/adaptive.md"{print $2}' "$TGT/configs/.templates/.removed" 2>/dev/null)"
run_deploy_rev "rev-3"
second="$(awk -F'\t' '$1=="workflows/adaptive.md"{print $2}' "$TGT/configs/.templates/.removed" 2>/dev/null)"
rm "$TGT/configs/workflows/adaptive.md"
run_deploy_rev "rev-4"
third="$(awk -F'\t' '$1=="workflows/adaptive.md"' "$TGT/configs/.templates/.removed" 2>/dev/null)"
if [ "$first" = "rev-2" ] && [ "$second" = "rev-2" ] && [ -z "$third" ]; then
	pass ".removed records the removing revision, survives the next prune, drops once the deployed copy is gone"
else
	fail ".removed: first='$first' second='$second' third='$third'"
fi

# --- Case 19: a re-added path leaves .removed.
build_repo
TGT="$TMP/t19"; rm -rf "$TGT"
run_deploy_rev "rev-1"
mv "$REPO/workflows/adaptive.md" "$TMP/adaptive.hold"
run_deploy_rev "rev-2"
mv "$TMP/adaptive.hold" "$REPO/workflows/adaptive.md"
run_deploy_rev "rev-3"
if ! awk -F'\t' '$1=="workflows/adaptive.md"{f=1} END{exit !f}' "$TGT/configs/.templates/.removed" 2>/dev/null; then
	pass "a path re-added to the manifest leaves .removed"
else
	fail "re-added path still in .removed"
fi

# --- Case 20: a temp file a crashed install left in .templates is deleted, not
# recorded as a template the product stopped shipping.
build_repo
TGT="$TMP/t20"; rm -rf "$TGT"
run_deploy_rev "rev-1"
echo junk > "$TGT/configs/.templates/.removed.tmp.999"
run_deploy_rev "rev-2"
if [ ! -e "$TGT/configs/.templates/.removed.tmp.999" ] && ! grep -q "tmp" "$TGT/configs/.templates/.removed"; then
	pass "a crashed install's temp file is swept, not recorded as a removal"
else
	fail "temp file handling: $(ls -a "$TGT/configs/.templates") / $(cat "$TGT/configs/.templates/.removed")"
fi

# --- Slice H (LLD 2026-07-16, 2026-10-03): a canonical file nobody edited
# follows the template. Before it, every directory was preserve-existing, so
# the reference host ran 2026-10-02's agent-templates after two shipped fixes.
origin_field() { grep -P "^$1\t" "$TGT/configs/.origin/.index" | cut -f"$2"; }

# H1: untouched canonical file + changed template -> replaced, .origin updated.
build_repo
TGT="$TMP/h1"; rm -rf "$TGT"
run_deploy_rev "rev-1"
echo "coder v2" > "$REPO/role-library/coder.md"
run_deploy_rev "rev-2"
if [ "$(cat "$TGT/configs/role-library/coder.md")" = "coder v2" ] \
	&& [ "$(origin_field 'role-library/coder\.md' 3)" = "rev-2" ] \
	&& [ "$(origin_field 'role-library/coder\.md' 5)" = "updated" ] \
	&& [ "$(cat "$TGT/configs/.origin/role-library/coder.md")" = "coder v2" ]; then
	pass "an untouched canonical file follows the template; .origin says updated at rev-2"
else
	fail "untouched canonical not updated: file='$(cat "$TGT/configs/role-library/coder.md")' origin='$(origin_field 'role-library/coder\.md' 3-5)'"
fi

# H2: an edited canonical file is kept, and its .origin is unchanged.
build_repo
TGT="$TMP/h2"; rm -rf "$TGT"
run_deploy_rev "rev-1"
echo "OPERATOR EDIT" > "$TGT/configs/role-library/coder.md"
before="$(origin_field 'role-library/coder\.md' 2-5)"
echo "coder v2" > "$REPO/role-library/coder.md"
run_deploy_rev "rev-2"
if [ "$(cat "$TGT/configs/role-library/coder.md")" = "OPERATOR EDIT" ] \
	&& [ "$(origin_field 'role-library/coder\.md' 2-5)" = "$before" ]; then
	pass "an edited canonical file is kept and its .origin untouched"
else
	fail "edited canonical overwritten or origin moved: file='$(cat "$TGT/configs/role-library/coder.md")'"
fi

# H3: already equal to the new template (synced by hand) -> .origin re-seeded,
# file not rewritten.
build_repo
TGT="$TMP/h3"; rm -rf "$TGT"
run_deploy_rev "rev-1"
echo "coder v2" > "$REPO/role-library/coder.md"
echo "coder v2" > "$TGT/configs/role-library/coder.md"
touch -d '2001-01-01' "$TGT/configs/role-library/coder.md"
run_deploy_rev "rev-2"
if [ "$(origin_field 'role-library/coder\.md' 5)" = "seeded" ] \
	&& [ "$(origin_field 'role-library/coder\.md' 3)" = "rev-2" ] \
	&& [ "$(stat -c %Y "$TGT/configs/role-library/coder.md")" = "$(date -d '2001-01-01' +%s)" ]; then
	pass "a canonical file already equal to the template re-seeds .origin without a rewrite"
else
	fail "re-seed: origin='$(origin_field 'role-library/coder\.md' 3-5)'"
fi

# H4: a canonical file with no .origin that differs is kept.
build_repo
TGT="$TMP/h4"; rm -rf "$TGT"; mkdir -p "$TGT/configs/role-library"
echo "UNKNOWN ORIGIN" > "$TGT/configs/role-library/coder.md"
run_deploy_rev "rev-1"
if [ "$(cat "$TGT/configs/role-library/coder.md")" = "UNKNOWN ORIGIN" ]; then
	pass "a differing canonical file with no .origin is kept"
else
	fail "a file of unknown origin was overwritten"
fi

# H5: the same untouched-and-changed case in a tunable directory is kept.
build_repo
TGT="$TMP/h5"; rm -rf "$TGT"
run_deploy_rev "rev-1"
echo "dev v2" > "$REPO/swarms/dev-swarm.md"
run_deploy_rev "rev-2"
if [ "$(cat "$TGT/configs/swarms/dev-swarm.md")" = "dev" ]; then
	pass "an untouched tunable file is still preserve-existing"
else
	fail "a tunable file was overwritten"
fi

# H6: a top-level file is kept.
build_repo
TGT="$TMP/h6"; rm -rf "$TGT"
run_deploy_rev "rev-1"
echo "pricing v2" > "$REPO/pricing.yaml"
run_deploy_rev "rev-2"
if [ "$(cat "$TGT/configs/pricing.yaml")" = "pricing" ]; then
	pass "a top-level file is still preserve-existing"
else
	fail "pricing.yaml was overwritten"
fi

# H7: a second install of the same revision changes nothing.
build_repo
TGT="$TMP/h7"; rm -rf "$TGT"
run_deploy_rev "rev-1"
echo "coder v2" > "$REPO/role-library/coder.md"
run_deploy_rev "rev-2"
snap="$(cat "$TGT/configs/.origin/.index"; cat "$TGT/configs/role-library/coder.md")"
run_deploy_rev "rev-2"
if [ "$(cat "$TGT/configs/.origin/.index"; cat "$TGT/configs/role-library/coder.md")" = "$snap" ] \
	&& [ "$(origin_field 'role-library/coder\.md' 5)" = "updated" ]; then
	pass "a repeated install is a no-op"
else
	fail "a repeated install changed .origin or the file"
fi

# H8 (review 508e F1, 0947 F4a): an acknowledgement is a decline. An untouched
# canonical file with an ack record (the writer's real layout; its key names
# an older template change) is kept after a new one, and its .origin unchanged.
build_repo
TGT="$TMP/h8"; rm -rf "$TGT"
run_deploy_rev "rev-1"
printf 'role-library/coder.md\tcanonical\tk1\texact\t2026-10-03T00:00:00Z\toperator\n' > "$TGT/configs/.template-acks"
before="$(origin_field 'role-library/coder\.md' 2-5)"
echo "coder v2" > "$REPO/role-library/coder.md"
run_deploy_rev "rev-2"
if [ "$(cat "$TGT/configs/role-library/coder.md")" = "coder" ] \
	&& [ "$(origin_field 'role-library/coder\.md' 2-5)" = "$before" ]; then
	pass "an acknowledged canonical file is kept"
else
	fail "an acknowledged canonical file was overwritten: '$(cat "$TGT/configs/role-library/coder.md")'"
fi

# H9 (508e F5): untouched means normalised-equal; a CRLF-only difference is
# untouched, and the file comes back as the template.
build_repo
TGT="$TMP/h9"; rm -rf "$TGT"
run_deploy_rev "rev-1"
printf 'coder\r\n' > "$TGT/configs/role-library/coder.md"
echo "coder v2" > "$REPO/role-library/coder.md"
run_deploy_rev "rev-2"
if [ "$(cat "$TGT/configs/role-library/coder.md")" = "coder v2" ]; then
	pass "a CRLF-only difference counts as untouched"
else
	fail "CRLF-only file kept: '$(od -c "$TGT/configs/role-library/coder.md" | head -1)'"
fi

# H10: an untouched canonical path that is a symlink is kept, and the file it
# points at is not written through it (refuse_unsafe_dest_path stops the
# install before deploy_one; this pins that slice H did not open a way round).
build_repo
TGT="$TMP/h10"; rm -rf "$TGT"
run_deploy_rev "rev-1"
echo "coder" > "$TMP/h10-target"
rm "$TGT/configs/role-library/coder.md"
ln -s "$TMP/h10-target" "$TGT/configs/role-library/coder.md"
echo "coder v2" > "$REPO/role-library/coder.md"
run_deploy_rev "rev-2"
if [ -L "$TGT/configs/role-library/coder.md" ] && [ "$(cat "$TMP/h10-target")" = "coder" ]; then
	pass "a symlinked canonical file is kept and its target untouched"
else
	fail "wrote through a symlink: target='$(cat "$TMP/h10-target")'"
fi

# H11: a canonical file with a second hard link (it could share an inode with
# a tunable file) is kept, and so is its twin.
build_repo
TGT="$TMP/h11"; rm -rf "$TGT"
run_deploy_rev "rev-1"
ln "$TGT/configs/role-library/coder.md" "$TMP/h11-twin"
echo "coder v2" > "$REPO/role-library/coder.md"
run_deploy_rev "rev-2"
if [ "$(cat "$TMP/h11-twin")" = "coder" ] && [ "$(cat "$TGT/configs/role-library/coder.md")" = "coder" ]; then
	pass "a hardlinked canonical file is kept, and its twin unchanged"
else
	fail "hardlinked file replaced: twin='$(cat "$TMP/h11-twin")'"
fi

# H12 (0947 F4b): an ack store that exists but cannot be read keeps every
# canonical file and rewrites no entry. Skipped as root (chmod 000 does not
# stop root reading).
if [ "$(id -u)" -eq 0 ]; then
	echo "skip - unreadable ack store (running as root)"
else
	build_repo
	TGT="$TMP/h12"; rm -rf "$TGT"
	run_deploy_rev "rev-1"
	: > "$TGT/configs/.template-acks"; chmod 000 "$TGT/configs/.template-acks"
	echo "coder v2" > "$REPO/role-library/coder.md"
	run_deploy_rev "rev-2"
	chmod 600 "$TGT/configs/.template-acks"
	if [ "$(cat "$TGT/configs/role-library/coder.md")" = "coder" ] \
		&& [ "$(origin_field 'role-library/coder\.md' 5)" = "created" ]; then
		pass "an unreadable ack store keeps canonical files"
	else
		fail "unreadable ack store: file='$(cat "$TGT/configs/role-library/coder.md")'"
	fi
fi

# H13 (0947 F5): with no working sha256 tool the hash cannot prove a file
# untouched, so it is kept.
build_repo
TGT="$TMP/h13"; rm -rf "$TGT"
run_deploy_rev "rev-1"
mkdir -p "$TMP/nohash"
printf '#!/bin/sh\nexit 1\n' > "$TMP/nohash/sha256sum"; cp "$TMP/nohash/sha256sum" "$TMP/nohash/shasum"
chmod +x "$TMP/nohash/sha256sum" "$TMP/nohash/shasum"
echo "coder v2" > "$REPO/role-library/coder.md"
PATH="$TMP/nohash:$PATH" run_deploy_rev "rev-2"
if [ "$(cat "$TGT/configs/role-library/coder.md")" = "coder" ]; then
	pass "with no working sha256 tool an untouched canonical file is kept"
else
	fail "overwrote without a hash: '$(cat "$TGT/configs/role-library/coder.md")'"
fi

# H14 (review 2733 F5): an update that cannot be written leaves the file and
# its .origin entry as they were, with a WARN. Skipped as root.
if [ "$(id -u)" -eq 0 ]; then
	echo "skip - unwritable canonical update (running as root)"
else
	build_repo
	TGT="$TMP/h14"; rm -rf "$TGT"
	run_deploy_rev "rev-1"
	before="$(origin_field 'role-library/coder\.md' 2-5)"
	echo "coder v2" > "$REPO/role-library/coder.md"
	chmod 500 "$TGT/configs/role-library"
	out="$(VORNIK_DEPLOY_REVISION=rev-2 VORNIK_REPO_CONFIGS_DIR="$REPO" "$DEPLOY" "$TGT" 2>&1)"
	chmod 700 "$TGT/configs/role-library"
	if [ "$(cat "$TGT/configs/role-library/coder.md")" = "coder" ] \
		&& [ "$(origin_field 'role-library/coder\.md' 2-5)" = "$before" ] \
		&& printf '%s' "$out" | grep -q "WARN: config-deploy: could not update"; then
		pass "an update that cannot be written changes nothing and warns"
	else
		fail "unwritable update: file='$(cat "$TGT/configs/role-library/coder.md")' out=$out"
	fi
fi

echo ""
if [ "$fails" -eq 0 ]; then echo "test-config-deploy: ALL PASS"; exit 0; fi
echo "test-config-deploy: $fails case(s) failed"; exit 1
