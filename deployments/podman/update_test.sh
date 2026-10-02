#!/usr/bin/env bash
# update_test.sh — vornik-update.sh must survive checking out a new version of
# ITSELF.
#
# THE INCIDENT (2026-09-06). Step 2 of the updater runs
#
#     git -C "$REPO_DIR" checkout --quiet "$TARGET_REF"
#
# and the script being executed lives inside $REPO_DIR, so that line rewrites
# the file bash is reading. Bash reads a script lazily and seeks back to a
# saved byte offset after each command; once the file underneath changes, it
# resumes at that offset inside the NEW file and executes whatever byte lands
# there. Every CE upgrade whose vornik-update.sh changed size died there:
#
#     vornik-update.sh: line 200: F: command not found      (exit 127)
#
# 2026.8.x (13023 B) -> 2026.9.x (17612 B) and 2026.9.1 -> 2026.9.2 (20487 B)
# both did. Nothing after the checkout ran — no binary build, no image
# rebuild, no sidecar recreate, no cutover. And because the checkout HAD
# already moved, the operator's retry printed "Checkout already at target
# commit. Nothing to do." and exited 0: a wholly un-updated install reporting
# success, which is worse than the half-applied state contract C3 forbids.
#
# WHY NO EXISTING TEST CAUGHT IT. Every check in update_test.go is a
# strings.Contains over the script TEXT. TestUpdaterBuildsImagesBeforeCutover
# proves rebuild_images appears before the install line in the file; it never
# runs the file, so a defect that stops execution before reaching that code is
# invisible to all nine of them. This suite EXECUTES the script — that is the
# whole point of it existing alongside the Go one.
#
# Run: bash deployments/podman/update_test.sh
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
UPDATER="$HERE/vornik-update.sh"
[ -f "$UPDATER" ] || { echo "FAIL: $UPDATER not found"; exit 1; }

WORK="$(mktemp -d "${TMPDIR:-/tmp}/vornik-update-test.XXXXXX")"
trap 'rm -rf "$WORK"' EXIT

# ---------------------------------------------------------------------------
# The two script versions. "old" is the real shipped script; "new" models the
# next release by inserting a comment block near the top, which is what shifts
# every byte offset below it. Simulating the release this way rather than
# pinning two git tags keeps the test hermetic and keeps it meaningful after
# the tags roll.
# ---------------------------------------------------------------------------
cp "$UPDATER" "$WORK/old.sh"
awk 'NR==2 { for (i = 0; i < 40; i++) print "# next release adds a line here, and every offset below it moves." } { print }' \
  "$UPDATER" > "$WORK/new.sh"
if [ "$(wc -c < "$WORK/old.sh")" -eq "$(wc -c < "$WORK/new.sh")" ]; then
  echo "FAIL: the fixture did not change the script size; the test would prove nothing"
  exit 1
fi

# ---------------------------------------------------------------------------
# A fake CE install: a checkout, a config, a bin dir, and stubs on PATH for
# everything the updater shells out to. The git stub does the one thing that
# matters — its `checkout` swaps old.sh for new.sh under the running script,
# exactly as the real one does.
# ---------------------------------------------------------------------------
REPO="$WORK/repo"
mkdir -p "$REPO/.git" "$REPO/deployments/podman" "$REPO/.bin" "$WORK/bin" "$WORK/cfg" "$WORK/localbin" "$WORK/home"
cp "$WORK/old.sh" "$REPO/deployments/podman/vornik-update.sh"
chmod +x "$REPO/deployments/podman/vornik-update.sh"
printf 'listen: ":8080"\n' > "$WORK/cfg/config.yaml"
# The freshly built binaries. The later tests run through step 4's -version
# smoke check and the cutover, so the daemon answers like a real one.
printf '#!/usr/bin/env bash\necho "vornik 2026.9.3 (built 2026-09-06T00:00:00Z, community edition)"\n' > "$REPO/.bin/vornik"
: > "$REPO/.bin/vornikctl"
chmod +x "$REPO/.bin/vornik" "$REPO/.bin/vornikctl"

# The manifest emitter the updater builds in step 2. One always-on row, with
# the "-" placeholder in the target column (C8).
ROW='ghcr.io/easeit-cz/vornik-agent:latest\timages/vornik-agent/Containerfile\t-\t.\talways\n'
cat > "$REPO/.bin/vornik-images" <<EMIT
#!/usr/bin/env bash
# Stands in for the real emitter. With -obtain it prints only the rows that
# still need a LOCAL BUILD, which is the contract the updater relies on.
# OBTAIN_MODE=nothing models the obtain step having pulled everything.
if [ "\${1:-}" = -obtain ] && [ "\${OBTAIN_MODE:-}" = nothing ]; then
  exit 0
fi
printf '$ROW'
EMIT
chmod +x "$REPO/.bin/vornik-images"

BUILD_LOG="$WORK/podman-build.log"
: > "$BUILD_LOG"

cat > "$WORK/bin/git" <<GIT
#!/usr/bin/env bash
case "\$*" in
  *"checkout --quiet"*)
      # THE DEFECT UNDER TEST: checking out the target rewrites the script
      # that is running right now.
      cp "$WORK/new.sh" "$REPO/deployments/podman/vornik-update.sh"
      touch "$WORK/.moved" ;;
  *"2026.9.8^{commit}"*)       echo ccccccc ;;
  *"dev^{commit}"*)            echo ddddddd ;;   # a branch named dev exists (review 8a63 F1)
  *"rev-parse --short HEAD"*)  { [ -f "$WORK/.moved" ] || [ -f "$WORK/.attarget" ]; } && echo bbbbbbb || echo aaaaaaa ;;
  *"rev-parse --short"*)       echo bbbbbbb ;;
  *"rev-parse HEAD"*)          echo bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb ;;
  *"rev-parse --verify"*)      echo bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb ;;
  *"tag -l"*)                  echo 2026.9.3 ;;
  *describe*)                  echo 2026.9.3 ;;
  *"log -1"*)                  echo 2026-09-06T00:00:00+00:00 ;;
  *) : ;;
esac
GIT

cat > "$WORK/bin/podman" <<PODMAN
#!/usr/bin/env bash
case "\$1" in
  build)
    printf '%s\n' "\$*" >> "$BUILD_LOG"
    if [ "\${BUILD_FAILS:-}" = int ]; then
      echo new-working-container >> "$WORK/external"
      kill -INT "\$PPID"; sleep 2; exit 130
    fi
    if [ -n "\${BUILD_FAILS:-}" ]; then
      # An interrupted build leaves its buildah working container behind.
      echo new-working-container >> "$WORK/external"
      exit 1
    fi
    exit 0 ;;
  rm) printf '%s\n' "\${@: -1}" >> "$WORK/podman-rm.log"; exit 0 ;;
esac
case "\$*" in
  "ps -a --external"*) cat "$WORK/external" 2>/dev/null ;;
  "ps --format"*)   echo vornik-postgres ;;
  *"image exists"*) exit 1 ;;   # no deployed image -> the row must be rebuilt
  *psql*)           echo 176 ;;
  *"cp "*)          : > "\${@: -1}" ;;
  *)                : ;;
esac
PODMAN

cat > "$WORK/bin/systemctl" <<SYSTEMCTL
#!/usr/bin/env bash
case "\$*" in
  *" stop "*|*" start "*) printf '%s\n' "\$*" | awk '{print \$2}' >> "$WORK/order.log" ;;
esac
exit 0
SYSTEMCTL
mkdir -p "$REPO/scripts"
# Step 3c's helper. The earlier tests stop at step 3b; the later ones run
# through the cutover, which needs it present.
printf '#!/usr/bin/env bash\ncat >/dev/null\nexit 0\n' > "$REPO/deployments/podman/recreate-sidecars.sh"
chmod +x "$REPO/deployments/podman/recreate-sidecars.sh"
cat > "$REPO/scripts/config-deploy.sh" <<DEPLOY
#!/usr/bin/env bash
echo deploy >> "$WORK/order.log"
printf '%s rev=%s\n' "\$*" "\${VORNIK_DEPLOY_REVISION:-}" > "$WORK/deploy.args"
DEPLOY
chmod +x "$REPO/scripts/config-deploy.sh"
printf '#!/usr/bin/env bash\nexit 0\n' > "$WORK/bin/curl"
chmod +x "$WORK/bin/git" "$WORK/bin/podman" "$WORK/bin/systemctl" "$WORK/bin/curl"

run_updater() {
  rm -f "$WORK/.moved" "$WORK/order.log" "$WORK/deploy.args" "$WORK/podman-rm.log"; : > "$BUILD_LOG"
  rm -rf "$WORK/home"/vornik-upgrade-backup-*
  cp "$WORK/old.sh" "$REPO/deployments/podman/vornik-update.sh"
  chmod +x "$REPO/deployments/podman/vornik-update.sh"
  env -u VORNIK_UPDATE_REEXEC -u VORNIK_UPDATE_COPY_DIR \
    PATH="${TEST_PATH:-$WORK/bin:$PATH}" HOME="$WORK/home" OBTAIN_MODE="${OBTAIN_MODE-}" BUILD_FAILS="${BUILD_FAILS-}" \
    VORNIK_DIR="$REPO" VORNIK_CONFIG="$WORK/cfg/config.yaml" VORNIK_BIN_DIR="$WORK/localbin" \
    bash "$REPO/deployments/podman/vornik-update.sh" "$@" > "$WORK/out" 2>&1 || true
}

fail() { echo "FAIL: $*"; echo '--- updater output ---'; cat "$WORK/out"; exit 1; }

# ---------------------------------------------------------------------------
# 1. The regression itself: a checkout that rewrites the script must not
#    derail the run. Pre-fix this dies at the checkout with a nonsense
#    "command not found" and never reaches step 3b.
# ---------------------------------------------------------------------------
run_updater --yes --no-build

grep -q 'command not found\|syntax error\|unexpected end of file' "$WORK/out" && \
  fail "the checkout corrupted the running script — bash resumed at a stale byte
      offset inside the new file. vornik-update.sh must execute from a private
      copy so the file it reads cannot change underneath it."

grep -q 'Rebuilding container images' "$WORK/out" || \
  fail "the run never reached step 3b (image rebuild) after the checkout"

grep -q 'ghcr.io/easeit-cz/vornik-agent:latest' "$BUILD_LOG" || \
  fail "step 3b ran but built no image; podman build was never invoked for the manifest row"

grep -q -- '--target' "$BUILD_LOG" && \
  fail "the '-' placeholder was passed through as a real --target (C8)"

echo 'update: checkout-rewrites-self does not derail the run — OK'

# ---------------------------------------------------------------------------
# 2. The trap that hid it. A run that dies after the checkout leaves HEAD on
#    the target, so the retry must not report "nothing to do" on an install
#    that was never updated. Guard the exact string, so if the corruption ever
#    returns the second run cannot quietly absolve it.
# ---------------------------------------------------------------------------
if grep -q 'Nothing to do' "$WORK/out"; then
  fail "the first run must not short-circuit; it has real work to do"
fi

# HEAD moved during run 1; a second invocation now sees current == target.
env -u VORNIK_UPDATE_REEXEC -u VORNIK_UPDATE_COPY_DIR \
  PATH="$WORK/bin:$PATH" HOME="$WORK/home" \
  VORNIK_DIR="$REPO" VORNIK_CONFIG="$WORK/cfg/config.yaml" VORNIK_BIN_DIR="$WORK/localbin" \
  bash "$REPO/deployments/podman/vornik-update.sh" --yes --no-build --check > "$WORK/out2" 2>&1 || true
grep -q 'already at the target commit' "$WORK/out2" || \
  fail "--check should report the checkout is at the target after run 1 moved it"

echo 'update: post-run --check reports the moved checkout — OK'

# ---------------------------------------------------------------------------
# 3. --help must still work from the re-exec'd copy: it reads its own comment
#    header out of "$0", and "$0" is the copy.
# ---------------------------------------------------------------------------
run_updater --help
grep -q 'safe in-place upgrade' "$WORK/out" || \
  fail "--help lost its comment header (the re-exec copy must carry it)"

echo 'update: --help works from the private copy — OK'

# ---------------------------------------------------------------------------
# 4. The private copy must not be left behind.
# ---------------------------------------------------------------------------
leaked=$(find "${TMPDIR:-/tmp}" -maxdepth 1 -name 'vornik-update.*' -newer "$WORK/old.sh" 2>/dev/null | head -5)
[ -z "$leaked" ] || fail "the re-exec copy was left behind: $leaked"

echo 'update: the private copy is cleaned up — OK'

# ---------------------------------------------------------------------------
# 5. Stage 2 (design §S2.6 test 3). The updater builds exactly the rows
#    `vornik-images -obtain` hands it — no more, and no second opinion.
#
#    An image the obtain step already PULLED must not also be built. Before
#    Stage 2 this script carried its own revision-label comparison, which after
#    a pull compares a CE commit against an EE HEAD, never matches, and rebuilds
#    forever. Emitting nothing is how the obtain step says "I dealt with it".
# ---------------------------------------------------------------------------
OBTAIN_MODE=nothing run_updater --yes --no-build
if grep -q 'ghcr.io/easeit-cz/vornik-agent' "$BUILD_LOG"; then
  fail "the updater built an image the obtain step had already handled.
      A pulled image must not also be built — see design §S2.3."
fi
grep -q 'Images: 0 built locally' "$WORK/out" || \
  fail "the updater did not report an empty build set"
unset OBTAIN_MODE

echo 'update: an obtained image is not rebuilt — OK'

# A row that IS handed back must be built, or the fallback path is dead.
run_updater --yes --no-build
grep -q 'ghcr.io/easeit-cz/vornik-agent:latest' "$BUILD_LOG" || \
  fail "the updater did not build a row the obtain step handed back — the local-build
      fallback is contract C7 and must survive Stage 2"

echo 'update: a handed-back row is built — OK'

# ---------------------------------------------------------------------------
# 6. Issue #16 (2026-10-01): the rollback record took pre_upgrade_commit from
#    the checkout. After an interrupted run moved the checkout, the --force
#    recovery recorded the TARGET as the pre-upgrade commit and printed a
#    rollback to it. It must come from the installed binary (design §15.1).
# ---------------------------------------------------------------------------
installed() {
  printf '#!/usr/bin/env bash\necho "vornik %s (built 2026-09-27T16:46:04Z, community edition)"\n' "$1" > "$WORK/localbin/vornik"
  chmod +x "$WORK/localbin/vornik"
}
installed 2026.9.8
touch "$WORK/.attarget"
run_updater --yes --no-build --force
state=$(cat "$WORK/home"/vornik-upgrade-backup-*/STATE.txt 2>/dev/null || true)
grep -qx 'pre_upgrade_commit=ccccccc' <<<"$state" || \
  fail "STATE.txt must record the INSTALLED commit (ccccccc, from vornik 2026.9.8), got:
$state"
grep -qx 'checkout_head_at_start=bbbbbbb' <<<"$state" || fail "STATE.txt must record the checkout HEAD separately:
$state"
grep -q 'checkout ccccccc' "$WORK/out" || fail "the printed rollback must check out the installed commit"
grep -q 'checkout bbbbbbb' "$WORK/out" && fail "the printed rollback still checks out the target"
grep -qx 'pre_upgrade_version=2026.9.8' <<<"$state" || fail "STATE.txt must keep the version token itself:
$state"

# The form most installs carry between releases: git describe's -g<hex>,
# with and without -dirty (review 20261002-df9b F1, F6).
for v in 2026.10.1-3-g1a2b3c4 2026.10.1-3-g1a2b3c4-dirty; do
  installed "$v"
  run_updater --yes --no-build --force
  state=$(cat "$WORK/home"/vornik-upgrade-backup-*/STATE.txt 2>/dev/null || true)
  grep -qx 'pre_upgrade_commit=1a2b3c4' <<<"$state" || fail "$v must map to 1a2b3c4:
$state"
  grep -qx "pre_upgrade_version=$v" <<<"$state" || fail "STATE.txt must keep the token $v:
$state"
  grep -q 'checkout 1a2b3c4' "$WORK/out" || fail "the rollback for $v must check out 1a2b3c4"
done
echo 'update: the rollback record names the installed commit, not the checkout — OK'

# The checkout HAS a ref named dev (the git stub resolves it), so resolving the
# unstamped "dev" token would name an unrelated commit.
installed dev
run_updater --yes --no-build --force
state=$(cat "$WORK/home"/vornik-upgrade-backup-*/STATE.txt 2>/dev/null || true)
grep -qx 'pre_upgrade_commit=unknown' <<<"$state" || fail "an unmappable version must be recorded as unknown:
$state"
grep -qx 'pre_upgrade_version=dev' <<<"$state" || fail "STATE.txt must keep the dev token:
$state"
grep -q 'git -C .* checkout' "$WORK/out" && fail "no git checkout may be printed when the installed commit is unknown"
grep -q 'ddddddd' "$WORK/out" && fail "the dev token was resolved to the dev branch: a guessed rollback target"
echo 'update: an unknown installed commit prints no guessed rollback — OK'

# The early exit trusted the checkout too: at target, installed elsewhere,
# no --force, and it said "Nothing to do".
installed 2026.9.8
run_updater --yes --no-build
grep -q 'Nothing to do' "$WORK/out" && fail "the checkout is at target but the INSTALL is not; the run must proceed"
grep -q 'Rebuilding container images' "$WORK/out" || fail "the run did not proceed past the early exit"
echo 'update: an install behind its checkout is updated without --force — OK'
rm -f "$WORK/.attarget" "$WORK/localbin/vornik"

# ---------------------------------------------------------------------------
# 7. Issue #15 (2026-10-01): an interrupted build left buildah working
#    containers behind (~28 GB on a vfs host). The EXIT trap removes the ones
#    THIS run created, and never one that existed before (design §15.2).
# ---------------------------------------------------------------------------
echo old-working-container > "$WORK/external"
BUILD_FAILS=1 run_updater --yes --no-build
grep -qx 'new-working-container' "$WORK/podman-rm.log" 2>/dev/null || \
  fail "the working container this run left behind was not removed"
grep -q 'old-working-container' "$WORK/podman-rm.log" 2>/dev/null && \
  fail "a working container that predates the run was removed; it may be another build in progress"
grep -q 'old-working-container\|1 leftover' "$WORK/out" || fail "the pre-existing leftover is not reported"
echo 'update: a failed build cleans up only its own containers — OK'

# The reported trigger was an INTERRUPT, not a failed build (review 8a63 F4).
echo old-working-container > "$WORK/external"
BUILD_FAILS=int run_updater --yes --no-build
grep -qx 'new-working-container' "$WORK/podman-rm.log" 2>/dev/null || \
  fail "an interrupted (SIGINT) build left its working container behind"
grep -q 'old-working-container' "$WORK/podman-rm.log" 2>/dev/null && \
  fail "the interrupt cleanup removed a container that predates the run"
rm -f "$WORK/external"
echo 'update: an interrupted build cleans up only its own containers — OK'

# ---------------------------------------------------------------------------
# 8. Issue #15: with no skopeo, preflight says so (design §15.3). A PATH
#    holding only the stubs and the basic tools the script needs.
# ---------------------------------------------------------------------------
SANDBOX="$WORK/sandbox-bin"; mkdir -p "$SANDBOX"
for t in bash sh cat chmod cp cut date du env head id install mkdir mktemp rm sed sleep sort tr grep awk find wc dirname basename readlink realpath tail tee ls uname; do
  p=$(command -v "$t" 2>/dev/null) && ln -sf "$p" "$SANDBOX/$t"
done
TEST_PATH="$WORK/bin:$SANDBOX" run_updater --yes --no-build
grep -qi 'skopeo' "$WORK/out" || fail "a host without skopeo is not told that the agent image will be built instead of pulled"
echo 'update: a missing skopeo is named at preflight — OK'

# ---------------------------------------------------------------------------
# 9. Design §15.5: new config subtrees (agent-templates) never reached an
#    updated install. The cutover deploys them, preserve-existing, with the
#    service stopped, stamping the target revision.
# ---------------------------------------------------------------------------
run_updater --yes --no-build
[ -f "$WORK/deploy.args" ] || fail "the config assets were not deployed"
grep -q "^$WORK/cfg rev=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" "$WORK/deploy.args" || \
  fail "config-deploy got the wrong target or revision: $(cat "$WORK/deploy.args")"
[ "$(tr '\n' ' ' < "$WORK/order.log")" = "stop deploy start " ] || \
  fail "config deploy must run between stop and start, got: $(tr '\n' ' ' < "$WORK/order.log")"
echo 'update: the cutover deploys new config subtrees — OK'

echo 'update_test.sh: PASS'
