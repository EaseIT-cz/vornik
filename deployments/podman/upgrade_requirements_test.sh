#!/usr/bin/env bash
# upgrade_requirements_test.sh — the updater shows every release's "Upgrade
# requirements" box between the installed release and the target, and will not
# proceed unattended without an acknowledgement naming the target (#610).
#
# THE INCIDENT (2026-11-01, production). An operator's agent upgraded to
# 2026.11.1 with `vornik-update.sh --yes` and never saw that release's box:
# back up the secret store key, spoken replies off by default, and more.
# vornik-update.sh did not read the release notes at all.
#
# Found at: production. Compared here at e2e-lane level: the helper runs
# against a fixture AND against the repository's own notes, and the updater
# runs end to end in a sandbox (a real git checkout with tags, stubbed
# systemctl/podman/binary) up to its acknowledgement gate.
#
# Design: https://docs.vornik.io §23.
set -uo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo="$(cd "$here/../.." && pwd)"
helper="$here/upgrade-requirements.sh"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

fails=0
pass() { echo "ok   - $1"; }
fail() { echo "FAIL - $1" >&2; fails=$((fails+1)); }

[ -x "$helper" ] || { echo "FAIL - $helper is missing or not executable" >&2; exit 1; }

cat >"$tmp/notes.md" <<'EOF'
# Release Notes

## 2026.11.2

Draft: not yet released.

!!! warning "Upgrade requirements"
    - **NEW-REQUIREMENT-ALPHA** spans
      two lines.

- A feature line that is not a requirement.

---

## 2026.11.1

!!! warning "Upgrade requirements"
    - **OLD-REQUIREMENT-BETA**

---

## 2026.10.7

- Nothing to do before this one.
EOF

# --- 1. Only releases ABOVE the installed one, with the denominator.
out="$("$helper" 2026.11.1 <"$tmp/notes.md" 2>"$tmp/err")"
if grep -q 'NEW-REQUIREMENT-ALPHA' <<<"$out" && grep -q 'two lines' <<<"$out"; then
  pass "prints the newer release's box, continuation lines included"
else
  fail "the newer release's box is missing: $out"
fi
if grep -q 'OLD-REQUIREMENT-BETA' <<<"$out"; then
  fail "printed the INSTALLED release's box, which is already applied"
else
  pass "does not print the installed release's box"
fi
grep -q '2026.11.2' <<<"$out" || fail "the box is not labelled with its release"
grep -q 'feature line' <<<"$out" && fail "printed a line outside the box"
if grep -q 'examined 1 release(s) newer than 2026.11.1, 1 with upgrade requirements' "$tmp/err"; then
  pass "states the denominator"
else
  fail "denominator missing or wrong: $(cat "$tmp/err")"
fi

# --- 2. Unknown installed release: every box, and says why.
out="$("$helper" 2025.1.0 <"$tmp/notes.md" 2>"$tmp/err")"
if grep -q 'NEW-REQUIREMENT-ALPHA' <<<"$out" && grep -q 'OLD-REQUIREMENT-BETA' <<<"$out"; then
  pass "an installed release missing from the notes shows every box"
else
  fail "an unknown installed release did not show every box: $out"
fi
grep -q 'not found in the release notes' "$tmp/err" || fail "does not say why it shows every box"
out="$("$helper" "" <"$tmp/notes.md" 2>/dev/null)"
grep -q 'OLD-REQUIREMENT-BETA' <<<"$out" || fail "an empty installed release did not show every box"

# --- 3. Nothing in range: no output, denominator still stated.
out="$("$helper" 2026.11.2 <"$tmp/notes.md" 2>"$tmp/err")"
if [ -z "$out" ]; then
  pass "nothing newer than the target prints nothing"
else
  fail "printed: $out"
fi
grep -q 'examined 0 release(s)' "$tmp/err" || fail "denominator missing when nothing is in range"

# --- 3b. Format drift: an OLDER helper meets a NEWER notes format. A section
# that mentions the requirements in a shape the helper does not parse must be
# shown whole, never dropped (review-20261103-9b8d F2/F3).
cat >"$tmp/drift.md" <<'EOF2'
## 2026.12.0

### Upgrade requirements

- **DRIFTED-REQUIREMENT-GAMMA** in a heading-and-list shape.

## 2026.11.2

- nothing here
EOF2
out="$("$helper" 2026.11.2 <"$tmp/drift.md" 2>"$tmp/err")"
if grep -q 'DRIFTED-REQUIREMENT-GAMMA' <<<"$out" && grep -q 'format not recognised' <<<"$out"; then
  pass "an unrecognised requirements shape is shown whole and flagged"
else
  fail "an unrecognised requirements shape was dropped: $out"
fi
grep -q '1 with upgrade requirements' "$tmp/err" || fail "the drifted section was not counted"

# --- 4. The repository's own notes: the format the helper parses is the real one.
out="$("$helper" 2026.10.7 <"$repo/docs/public/release-notes/index.md" 2>/dev/null)"
if grep -q 'store.key' <<<"$out"; then
  pass "the real 2026.11.1 box is found in the repository's notes"
else
  fail "the real 2026.11.1 box was not found; has the notes' format drifted?"
fi

# --- 5. The updater, end to end in a sandbox, up to the acknowledgement gate.
mk_stub() { printf '#!/usr/bin/env bash\n%s\n' "$2" >"$tmp/bin/$1"; chmod +x "$tmp/bin/$1"; }
mkdir -p "$tmp/bin" "$tmp/home/.local/bin" "$tmp/home/.config/vornik"
# The stubs log every call, so the suite can prove no mutating step ran.
mk_stub systemctl "echo \"\$*\" >>'$tmp/calls.log'; exit 0"
mk_stub podman "echo \"podman \$*\" >>'$tmp/calls.log'; case \"\$1\" in ps) echo vornik-postgres ;; exec) echo 5 ;; esac; exit 0"
mk_stub skopeo 'exit 0'
: >"$tmp/home/.config/vornik/config.yaml"

up="$tmp/upstream"; co="$tmp/home/vornik"
git init -q "$up"
git -C "$up" config user.email t@example.com; git -C "$up" config user.name t
mkdir -p "$up/docs/public/release-notes" "$up/deployments/podman"
cp "$here/vornik-update.sh" "$here/upgrade-requirements.sh" "$up/deployments/podman/"
sed -n '/^## 2026.11.1$/,$p' "$tmp/notes.md" | sed '1i # Release Notes\n' >"$up/docs/public/release-notes/index.md"
git -C "$up" add -A; git -C "$up" commit -qm one; git -C "$up" tag 2026.11.1
cp "$tmp/notes.md" "$up/docs/public/release-notes/index.md"
git -C "$up" commit -qam two; git -C "$up" tag 2026.11.2
git clone -q "$up" "$co"; git -C "$co" checkout -q 2026.11.1
installed="$(git -C "$co" rev-parse --short HEAD)"
mk_stub vornik "echo 'vornik 2026.11.1-0-g$installed'"
cp "$tmp/bin/vornik" "$tmp/home/.local/bin/vornik"

run_update() {
  (cd "$tmp" && HOME="$tmp/home" PATH="$tmp/bin:$PATH" VORNIK_DIR="$co" \
    VORNIK_BIN_DIR="$tmp/home/.local/bin" VORNIK_CONFIG="$tmp/home/.config/vornik/config.yaml" \
    timeout 60 bash "$co/deployments/podman/vornik-update.sh" "$@" </dev/null 2>&1)
}

out="$(run_update --ref 2026.11.2 --yes)"; rc=$?
if [ "$rc" -ne 0 ] && grep -q 'NEW-REQUIREMENT-ALPHA' <<<"$out" \
    && grep -q -- '--ack-upgrade-requirements=2026.11.2' <<<"$out" \
    && ! grep -q 'Backup' <<<"$out"; then
  pass "--yes without an acknowledgement shows the box and refuses before any backup"
else
  fail "--yes without an acknowledgement did not refuse as designed (rc=$rc): $(tail -5 <<<"$out")"
fi

out="$(run_update --ref 2026.11.2 --yes --ack-upgrade-requirements=2026.11.1)"; rc=$?
if [ "$rc" -ne 0 ] && grep -q 'does not name the target' <<<"$out"; then
  pass "an acknowledgement for another target refuses"
else
  fail "an acknowledgement for another target was accepted (rc=$rc): $(tail -3 <<<"$out")"
fi

# Without --yes, so the run stops at the ordinary y/N prompt (stdin is empty)
# and the sandbox never reaches the backup or the cutover.
out="$(run_update --ref 2026.11.2 --ack-upgrade-requirements=2026.11.2)"
if grep -q 'Upgrade requirements acknowledged for 2026.11.2' <<<"$out" && ! grep -q 'Backup' <<<"$out"; then
  pass "the matching acknowledgement passes the gate (and the run stops at the prompt)"
else
  fail "the matching acknowledgement did not pass the gate: $(tail -5 <<<"$out")"
fi

# Interactive, no flag: the typed answer must be the target's name; "y" is not.
out="$(printf 'y\n' | (cd "$tmp" && HOME="$tmp/home" PATH="$tmp/bin:$PATH" VORNIK_DIR="$co" \
  VORNIK_BIN_DIR="$tmp/home/.local/bin" VORNIK_CONFIG="$tmp/home/.config/vornik/config.yaml" \
  timeout 60 bash "$co/deployments/podman/vornik-update.sh" --ref 2026.11.2 2>&1))"; rc=$?
if [ "$rc" -ne 0 ] && ! grep -q 'Backup' <<<"$out"; then
  pass "interactively, answering y instead of the target's name refuses"
else
  fail "interactively, a bare y passed the gate (rc=$rc): $(tail -3 <<<"$out")"
fi

# Interactive, typing the target's name: the gate passes (then the y/N prompt
# reads EOF and the run stops before any change).
out="$(printf '2026.11.2\n' | (cd "$tmp" && HOME="$tmp/home" PATH="$tmp/bin:$PATH" VORNIK_DIR="$co" \
  VORNIK_BIN_DIR="$tmp/home/.local/bin" VORNIK_CONFIG="$tmp/home/.config/vornik/config.yaml" \
  timeout 60 bash "$co/deployments/podman/vornik-update.sh" --ref 2026.11.2 2>&1))"
if grep -q 'Upgrade requirements acknowledged for 2026.11.2' <<<"$out" && ! grep -q 'Backup' <<<"$out"; then
  pass "interactively, typing the target's name passes the gate"
else
  fail "interactively, typing the target's name did not pass: $(tail -3 <<<"$out")"
fi

out="$(run_update --ref 2026.11.2 --check)"; rc=$?
if [ "$rc" -eq 0 ] && grep -q 'NEW-REQUIREMENT-ALPHA' <<<"$out"; then
  pass "--check prints the box and exits 0"
else
  fail "--check did not print the box (rc=$rc): $(tail -3 <<<"$out")"
fi

# --check with nothing newer than the installed release: no box, exit 0.
out="$(run_update --ref 2026.11.1 --check)"; rc=$?
if [ "$rc" -eq 0 ] && ! grep -q 'UPGRADE REQUIREMENTS' <<<"$out"; then
  pass "--check with nothing in range prints no box"
else
  fail "--check with nothing in range misbehaved (rc=$rc): $(tail -3 <<<"$out")"
fi

# The installed release comes from the BINARY's version, not the checkout
# (§15.1): with the checkout already at the target (an interrupted run, §13.2)
# and the binary still old, the gate must still fire.
git -C "$co" checkout -q 2026.11.2
out="$(run_update --ref 2026.11.2 --yes)"; rc=$?
if [ "$rc" -ne 0 ] && grep -q 'NEW-REQUIREMENT-ALPHA' <<<"$out" && ! grep -q 'Backup' <<<"$out"; then
  pass "checkout at the target, binary old: the gate still fires"
else
  fail "with the checkout at the target the gate did not fire (rc=$rc): $(tail -3 <<<"$out")"
fi
git -C "$co" checkout -q 2026.11.1

# An installed build whose commit is unknown: every box, the installed
# release's included, and the gate fires.
mk_stub vornik "echo 'vornik dev'"
cp "$tmp/bin/vornik" "$tmp/home/.local/bin/vornik"
out="$(run_update --ref 2026.11.2 --check)"
if grep -q 'OLD-REQUIREMENT-BETA' <<<"$out" && grep -q 'NEW-REQUIREMENT-ALPHA' <<<"$out"; then
  pass "an unknown installed build shows every box"
else
  fail "an unknown installed build did not show every box: $(tail -3 <<<"$out")"
fi
out="$(run_update --ref 2026.11.2 --yes)"; rc=$?
if [ "$rc" -ne 0 ] && grep -q -- '--ack-upgrade-requirements=2026.11.2' <<<"$out" && ! grep -q 'Backup' <<<"$out"; then
  pass "an unknown installed build: the gate fires under --yes"
else
  fail "an unknown installed build passed the gate under --yes (rc=$rc): $(tail -3 <<<"$out")"
fi
mk_stub vornik "echo 'vornik 2026.11.1-0-g$installed'"
cp "$tmp/bin/vornik" "$tmp/home/.local/bin/vornik"

# A target that predates the notes file: a warning naming the gap, and the
# run continues (here: --check exits 0).
git -C "$up" checkout -q 2026.11.2
git -C "$up" rm -q docs/public/release-notes/index.md
git -C "$up" commit -qm three; git -C "$up" tag 2026.12.0
git -C "$co" fetch -q --tags origin
out="$(run_update --ref 2026.12.0 --check)"; rc=$?
if [ "$rc" -eq 0 ] && grep -q 'has no docs/public/release-notes/index.md' <<<"$out"; then
  pass "a target without release notes warns and continues"
else
  fail "a target without release notes misbehaved (rc=$rc): $(tail -3 <<<"$out")"
fi

# A notes file that EXISTS but cannot be read must refuse, never read as "no
# requirements" (review-20261103-5dce N1). A git wrapper fails only that read.
real_git="$(command -v git)"
# shellcheck disable=SC2016  # the stub's own script text: expands when the stub runs
printf '#!/usr/bin/env bash\nfor a in "$@"; do case "$a" in *release-notes/index.md) [ "$1" = "-C" ] && [ "$3" = show ] && exit 128 ;; esac; done\nexec %q "$@"\n' "$real_git" >"$tmp/bin/git"
chmod +x "$tmp/bin/git"
out="$(run_update --ref 2026.11.2 --yes)"; rc=$?
rm -f "$tmp/bin/git"
if [ "$rc" -ne 0 ] && grep -q 'could not be read' <<<"$out" && ! grep -q 'Backup' <<<"$out"; then
  pass "unreadable notes refuse the run before any change"
else
  fail "unreadable notes did not refuse (rc=$rc): $(tail -3 <<<"$out")"
fi

# Across every updater run above: no service start/stop and no image or
# container work. Every run stopped at --check, the gate, or the y/N prompt.
if grep -qE '(^|--user )(start|stop|restart)|podman (build|run|pull|rm|tag|compose)' "$tmp/calls.log"; then
  fail "a sandbox run reached a mutating step: $(grep -E '(start|stop|restart)|podman (build|run|pull|rm|tag|compose)' "$tmp/calls.log" | sed -n 1,3p | tr '\n' ';')"
else
  pass "no sandbox run reached a mutating step ($(wc -l <"$tmp/calls.log") stub call(s) examined)"
fi

[ "$fails" -eq 0 ] || { echo "$fails failure(s)" >&2; exit 1; }
echo "all cases passed"
