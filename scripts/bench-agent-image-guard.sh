#!/usr/bin/env bash
# bench-agent-image-guard.sh — refuse to start a benchmark pass when the images
# the bench swarms run are missing, or are not the image the pass pinned.
#
# Sourced by scripts/agentbench-reproduce.sh (preflight 0c); exercised directly
# by scripts/bench-agent-image-guard_test.sh. Design: benchmark LLD §12.25 D1.
#
# WHY THIS EXISTS (GitHub #309, 2026-10-08).
#
# Preflight 0b builds and pins ghcr.io/easeit-cz/vornik-agent:bench. Every bench
# swarm runs localhost/vornik-agent:bench, and that tag did not exist. Podman
# tried to pull it from a registry called "localhost", every step failed at
# container start, and the arm journaled 44 task runs with zero model calls in
# 30 minutes. Nothing in the preflight read the image names the swarms use.
#
# It refuses rather than tagging. Tagging the built image under every name the
# swarms reference would, for a swarm naming ghcr.io/...:latest, overwrite the
# tag production's agents run — what 0b's "bench tag only" rule forbids. The
# remedy is printed for the operator to run.
#
# It prints its denominator. A check that reports "ok" without saying how many
# images it examined cannot be told apart from one that examined none, so zero
# swarm files, or swarms that name no image, refuse.

# bench_swarm_image_rows SWARM_DIR — print one "<image>\t<file>" row per role
# that names runtime.image, and one "-\t<file>" row per role that does not,
# across every live *.md swarm file (backups like *.md.bak-* are not *.md).
# Only the YAML front matter is read: prose below it may mention an image.
# Prints "!\t<file>" for a file whose front matter does not parse.
bench_swarm_image_rows() {
    python3 - "$1" <<'PY'
import glob, os, sys
import yaml
d = sys.argv[1]
for path in sorted(glob.glob(os.path.join(d, "*.md"))):
    name = os.path.basename(path)
    try:
        text = open(path, encoding="utf-8").read()
        parts = text.split("\n---", 1) if text.startswith("---") else None
        if not parts:
            raise ValueError("no front matter")
        doc = yaml.safe_load(parts[0][3:]) or {}
        roles = doc.get("roles") or []
    except Exception:
        print(f"!\t{name}")
        continue
    for r in roles:
        img = (((r or {}).get("runtime") or {}).get("image") or "").strip()
        # "noop:" is the registry's sentinel for a role that never launches a
        # container (internal/registry/patch.go; hostdoctor skips it the same
        # way). It is counted, never inspected.
        if img.startswith("noop:"):
            img = "~"
        print(f"{img or '-'}\t{name}")
PY
}

# bench_assert_swarm_images SWARM_DIR PINNED_ID AGENT_IMAGE — succeed only when
# every image the swarms name exists locally with image id PINNED_ID (the id
# preflight 0b pinned for AGENT_IMAGE). Messages go to stderr on refusal and to
# stdout on success.
bench_assert_swarm_images() {
    local dir="$1" pinned="${2#sha256:}" agent="$3"
    if [ ! -d "$dir" ]; then
        echo "no swarm directory at $dir: the images the agents will run cannot be checked" >&2
        return 1
    fi
    local rows
    rows=$(bench_swarm_image_rows "$dir") || {
        echo "could not read the swarm files in $dir (python3 + PyYAML are required)" >&2
        return 1
    }
    local nfiles nroles nnone nnoop bad_parse
    nfiles=$(printf '%s\n' "$rows" | awk -F'\t' 'NF==2{print $2}' | sort -u | grep -c . || true)
    bad_parse=$(printf '%s\n' "$rows" | awk -F'\t' '$1=="!"{print $2}')
    nroles=$(printf '%s\n' "$rows" | awk -F'\t' 'NF==2 && $1!="!"' | grep -c . || true)
    nnone=$(printf '%s\n' "$rows" | awk -F'\t' '$1=="-"' | grep -c . || true)
    nnoop=$(printf '%s\n' "$rows" | awk -F'\t' '$1=="~"' | grep -c . || true)
    if [ -n "$bad_parse" ]; then
        echo "swarm file(s) whose front matter does not parse, so their images are unknown: $(echo "$bad_parse" | tr '\n' ' ')" >&2
        return 1
    fi
    if [ "${nfiles:-0}" = "0" ]; then
        echo "no swarm files (*.md) in $dir: zero images examined is not a clean check" >&2
        return 1
    fi
    # The registry loader refuses a role without runtime.image
    # (internal/registry/swarm.go, "runtime image is required"). Reading one
    # means this guard does not see the swarm the daemon sees, so whatever it
    # would call clean was never examined (review-20261012-16a3 M1/M2).
    if [ "${nnone:-0}" != "0" ]; then
        local nofiles
        nofiles=$(printf '%s\n' "$rows" | awk -F'\t' '$1=="-"{print $2}' | sort -u | tr '\n' ' ')
        echo "$nnone role(s) in ${nofiles% } name no runtime.image as this guard reads them, but the" >&2
        echo "  daemon refuses such a role. The guard's reading of the swarm is not the daemon's," >&2
        echo "  so the images were not examined. Check the front matter of those files." >&2
        return 1
    fi
    local images
    images=$(printf '%s\n' "$rows" | awk -F'\t' 'NF==2 && $1!="-" && $1!="!" && $1!="~"{print $1}' | sort -u)
    if [ -z "$images" ]; then
        echo "no role in the $nfiles swarm file(s) in $dir names runtime.image; the image the agents" >&2
        echo "  will run (the daemon default) cannot be checked here. Name it in the bench swarms." >&2
        return 1
    fi
    local nimages failed=0 img id files rc
    nimages=$(printf '%s\n' "$images" | grep -c .)
    while IFS= read -r img; do
        files=$(printf '%s\n' "$rows" | awk -F'\t' -v i="$img" '$1==i{print $2}' | sort -u | tr '\n' ' ')
        # `image exists` separates "absent" (exit 1) from "podman could not
        # answer" (anything else), which inspect alone reports alike.
        rc=0; podman image exists "$img" >/dev/null 2>&1 || rc=$?
        if [ "$rc" = "1" ]; then
            echo "agent image $img is not present locally (named by: ${files% }). Every step would fail" >&2
            echo "  at container start and no model would be called. Remedy: podman tag $agent $img" >&2
            failed=1
            continue
        elif [ "$rc" != "0" ]; then
            echo "could not check agent image $img (podman image exists exited $rc; named by: ${files% })." >&2
            echo "  This is podman failing, not the image being absent. Fix podman and re-run." >&2
            failed=1
            continue
        fi
        id=$(podman image inspect --format '{{.Id}}' "$img" 2>/dev/null | tr -d '\n' | sed 's/^sha256://')
        if [ -z "$id" ]; then
            echo "could not check agent image $img: podman reports it present but gives no image id." >&2
            failed=1
        elif [ "$id" != "$pinned" ]; then
            echo "agent image $img is ${id:0:12}, but this pass pinned ${pinned:0:12} ($agent) (named by:" >&2
            echo "  ${files% }). The arm would run a different agent than it pins and grades with." >&2
            echo "  Remedy: podman tag $agent $img" >&2
            failed=1
        fi
    done <<< "$images"
    [ "$failed" = "0" ] || return 1
    echo "$nimages distinct image(s) across $nroles role(s) in $nfiles swarm file(s), all the pinned ${pinned:0:12}; $nnoop role(s) are non-containerised (noop:)"
}
