#!/usr/bin/env bash
# shellcheck disable=SC2016 # the grep patterns match the literal $_have, $AGENT_PIN and $AGENT_IMAGE in the reproduce script
# Unit tests for scripts/bench-agent-image-guard.sh (benchmark LLD §12.25 D1).
#
# audit 2026-10-12: GitHub #309. The 2026-10-08 slow-hardware arm journaled 44
# task runs with zero model calls because every bench swarm ran
# localhost/vornik-agent:bench, which did not exist, while the preflight only
# checked the ghcr bench tag it had built. These tests pin the refusal for a
# missing image, the refusal for an image that is not the pinned one, and the
# denominator the check prints, so "clean" cannot mean "examined nothing".
#
# podman is stubbed: a fake on PATH answers `podman image inspect --format
# {{.Id}} <name>` from a fixture table, so no test needs podman or images.
#
# Run: bash scripts/bench-agent-image-guard_test.sh
set -u

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/bench-agent-image-guard.sh
. "$HERE/bench-agent-image-guard.sh"

pass=0; fail=0
ok()  { pass=$((pass+1)); echo "PASS: $1"; }
bad() { fail=$((fail+1)); echo "FAIL: $1" >&2; }

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

# Fake podman: image ids come from $TMP/images (lines "name id").
mkdir -p "$TMP/bin"
cat > "$TMP/bin/podman" <<'FAKE'
#!/usr/bin/env bash
# Fake: `podman image exists <name>` (0 present, 1 absent) and
# `podman image inspect --format {{.Id}} <name>`. FAKE_PODMAN_BROKEN=1 makes
# every call fail the way a broken storage or socket does (exit 125).
[ "${FAKE_PODMAN_BROKEN:-0}" = "1" ] && { echo "Error: cannot connect to storage" >&2; exit 125; }
[ "$1" = "image" ] || exit 2
name="${*: -1}"
while read -r n id; do
    if [ "$n" = "$name" ]; then
        [ "$2" = "exists" ] && exit 0
        echo "sha256:$id"; exit 0
    fi
done < "$FAKE_PODMAN_IMAGES"
[ "$2" = "exists" ] && exit 1
echo "Error: $name: image not known" >&2
exit 125
FAKE
chmod +x "$TMP/bin/podman"
export PATH="$TMP/bin:$PATH"
export FAKE_PODMAN_IMAGES="$TMP/images"

PIN="aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
OTHER="bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

# swarm DIR FILE IMAGE... — write a swarm file whose roles name these images
# ("-" for a role with no runtime.image).
swarm() {
    local dir="$1" file="$2"; shift 2
    mkdir -p "$dir"
    {
        echo "---"
        echo "swarmId: ${file%.md}"
        echo "roles:"
        local i=0
        for img in "$@"; do
            i=$((i+1))
            echo "  - name: \"r$i\""
            if [ "$img" != "-" ]; then
                echo "    runtime:"
                echo "      image: \"$img\""
            fi
        done
        echo "---"
        echo "# Prose below the front matter must not be read: image: \"prose/not-an-image:1\""
    } > "$dir/$file"
}

run_guard() { bench_assert_swarm_images "$1" "$PIN" "ghcr.io/easeit-cz/vornik-agent:bench" 2>&1; }

# --- the incident: swarms name localhost/...:bench, which does not exist ---
D="$TMP/incident"; swarm "$D" dev-swarm.md localhost/vornik-agent:bench localhost/vornik-agent:bench
printf 'ghcr.io/easeit-cz/vornik-agent:bench %s\n' "$PIN" > "$TMP/images"
if out=$(run_guard "$D"); then
    bad "a swarm naming a missing image must refuse (allowed): $out"
else
    case "$out" in
        *"localhost/vornik-agent:bench"*"dev-swarm.md"*) ok "missing image refuses and names the image and its swarm file" ;;
        *) bad "missing-image refusal does not name the image and file: $out" ;;
    esac
fi

# --- present, but a different image than the pinned one ---
D="$TMP/mismatch"; swarm "$D" dev-swarm.md localhost/vornik-agent:bench
printf 'localhost/vornik-agent:bench %s\n' "$OTHER" > "$TMP/images"
if out=$(run_guard "$D"); then
    bad "an image that is not the pinned one must refuse (allowed): $out"
else
    case "$out" in
        *"localhost/vornik-agent:bench"*"podman tag"*) ok "a different image than the pin refuses and prints the tag remedy" ;;
        *) bad "mismatch refusal lacks the image or the remedy: $out" ;;
    esac
fi

# --- all match: allowed, with the denominator printed ---
D="$TMP/clean"
swarm "$D" dev-swarm.md localhost/vornik-agent:bench localhost/vornik-agent:bench
# noop:dispatcher is the registry's sentinel for a role that never launches a
# container (internal/registry/patch.go). The live bench assistant-swarm has
# one; a dry run against it on 2026-10-12 refused on it as a missing image.
swarm "$D" assistant-swarm.md localhost/vornik-agent:bench noop:dispatcher
# A backup is not a live swarm file and must not be read (it names a missing image).
swarm "$D/tmpbak" x.md ghcr.io/missing/old:1; mv "$D/tmpbak/x.md" "$D/dev-swarm.md.bak-20260926T064643Z"; rmdir "$D/tmpbak"
printf 'localhost/vornik-agent:bench %s\n' "$PIN" > "$TMP/images"
if out=$(run_guard "$D"); then
    case "$out" in
        *"1 distinct image(s) across 4 role(s) in 2 swarm file(s)"*"1 role(s) are non-containerised (noop:)"*) ok "matching images pass, skip noop: roles, and print the denominator" ;;
        *) bad "clean pass does not print the denominator: $out" ;;
    esac
else
    bad "matching images must pass (refused): $out"
fi
case "$out" in
    *"prose/not-an-image"*) bad "prose below the front matter was read as an image" ;;
    *) ok "prose below the front matter is not read" ;;
esac

# --- zero examined is not clean ---
D="$TMP/empty"; mkdir -p "$D"
if out=$(run_guard "$D"); then bad "an empty swarm directory must refuse (allowed): $out"; else ok "an empty swarm directory refuses"; fi
if out=$(run_guard "$TMP/does-not-exist"); then bad "a missing swarm directory must refuse (allowed): $out"; else ok "a missing swarm directory refuses"; fi

# --- every role without an image: nothing checked is not clean either ---
D="$TMP/noimages"; swarm "$D" dev-swarm.md - -
if out=$(run_guard "$D"); then bad "swarms naming no image at all must refuse (allowed): $out"; else ok "swarms naming no image refuse"; fi

# --- ONE role without an image refuses (review-20261012-16a3 M1/M2): the
# registry rejects such a role, so reading one means the guard's picture of
# the swarm is not the daemon's, and "clean" would mean "not examined" ---
D="$TMP/onenoimage"; swarm "$D" dev-swarm.md localhost/vornik-agent:bench -
printf 'localhost/vornik-agent:bench %s\n' "$PIN" > "$TMP/images"
if out=$(run_guard "$D"); then
    bad "a role with no runtime.image must refuse (allowed): $out"
else
    case "$out" in
        *"dev-swarm.md"*"runtime.image"*) ok "a role with no image refuses and names the file" ;;
        *) bad "no-image refusal does not name the file and the field: $out" ;;
    esac
fi

# --- podman cannot answer: refuse, but do not claim the image is absent ---
D="$TMP/broken"; swarm "$D" dev-swarm.md localhost/vornik-agent:bench
if out=$(FAKE_PODMAN_BROKEN=1 run_guard "$D"); then
    bad "a failing podman must refuse (allowed): $out"
else
    case "$out" in
        *"could not check"*) ok "a failing podman refuses with 'could not check', not 'missing'" ;;
        *) bad "a failing podman was reported as something else: $out" ;;
    esac
fi

# --- the reproduce script runs the check, after 0b pins the image ---
# A guard nobody calls is the "documented behaviour nothing implements" case.
REPRO="$HERE/agentbench-reproduce.sh"
pin_line=$(grep -n 'printf .%s\\n. "\$_have" > "\$AGENT_PIN"' "$REPRO" | tail -1 | cut -d: -f1)
call_line=$(grep -n 'bench_assert_swarm_images ' "$REPRO" | head -1 | cut -d: -f1)
if grep -q 'bench-agent-image-guard.sh' "$REPRO" && [ -n "$call_line" ] && [ -n "$pin_line" ] && [ "$call_line" -gt "$pin_line" ]; then
    ok "agentbench-reproduce.sh sources the guard and calls it after the image is pinned"
else
    bad "agentbench-reproduce.sh does not call bench_assert_swarm_images after 0b (pin line ${pin_line:-none}, call line ${call_line:-none})"
fi
# The id it is handed must be the PINNED id, read back from the pin file, on
# every path through 0b (resume, skip-build, build).
if grep -qE 'bench_assert_swarm_images .*/configs/swarms" "\$\(cat "\$AGENT_PIN"\)" "\$AGENT_IMAGE"' "$REPRO"; then
    ok "the guard is handed the pinned id and the agent image"
else
    bad "the guard is not handed \$(cat \"\$AGENT_PIN\") and \$AGENT_IMAGE"
fi

echo
echo "bench-agent-image-guard: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
