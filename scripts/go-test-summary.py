#!/usr/bin/env python3
"""Summarise a `go test -json` stream: the denominator, the failures, the skips.

CE export runbook, amendment 2026-10-01 (D4). 2026.9.7's e2e_migrate SKIPPED
on the release host and `go test` printed ok, so a lane that never ran read
as a pass. This prints passed, failed and skipped counts, names every failed
and skipped test, and exits non-zero when:
  - a test, a package or a build failed;
  - the stream counted no tests at all (a lane that ran nothing has not passed);
  - a line is not JSON (fail closed: the stream is not what we think it is);
  - --fail-on-skip is given and a test skipped that --allow-skips does not
    list (each entry: "<package>.<Test><TAB><reason>"; an entry with no
    reason fails closed, and a listed test that did not skip is reported as
    stale so the list shrinks).

The caller keeps `go test`'s own exit status as well; this script reads the
stream from a FILE, so nothing depends on a pipe's exit status.

Usage: go-test-summary.py [--fail-on-skip] [--allow-skips FILE] [--lane NAME] STREAM.json
"""
import argparse
import json
import sys


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--fail-on-skip", action="store_true")
    ap.add_argument("--allow-skips")
    ap.add_argument("--lane", default="tests")
    ap.add_argument("stream")
    a = ap.parse_args()

    allowed = {}
    if a.allow_skips:
        try:
            with open(a.allow_skips, encoding="utf-8") as fh:
                for n, raw in enumerate(fh, 1):
                    line = raw.strip()
                    if not line or line.startswith("#"):
                        continue
                    name, _, reason = line.partition("\t")
                    if not reason.strip():
                        print(f"{a.lane}: {a.allow_skips}:{n}: {name!r} has no reason; every "
                              "allowed skip says why it is by design", file=sys.stderr)
                        return 1
                    allowed[name.strip()] = reason.strip()
        except OSError as e:
            print(f"{a.lane}: cannot read {a.allow_skips}: {e}", file=sys.stderr)
            return 1

    try:
        with open(a.stream, encoding="utf-8") as fh:
            lines = fh.read().splitlines()
    except OSError as e:
        print(f"{a.lane}: cannot read {a.stream}: {e}", file=sys.stderr)
        return 1

    passed, failed, skipped = [], [], []
    failed_pkgs, build_failures, garbage = [], [], 0
    pkg_has_test_result = set()
    for line in lines:
        if not line.strip():
            continue
        try:
            ev = json.loads(line)
        except ValueError:
            garbage += 1
            continue
        if not isinstance(ev, dict):
            garbage += 1
            continue
        action, pkg, test = ev.get("Action"), ev.get("Package", ""), ev.get("Test", "")
        if action == "build-fail":
            build_failures.append(ev.get("ImportPath", "?"))
            continue
        if test:
            # Subtests report too; count only top-level tests, so the
            # denominator is the tests the lane ran, not their parts.
            if "/" in test:
                if action == "fail":
                    pkg_has_test_result.add(pkg)
                continue
            name = f"{pkg}.{test}"
            if action == "pass":
                passed.append(name)
            elif action == "fail":
                failed.append(name)
            elif action == "skip":
                skipped.append(name)
            if action in ("pass", "fail", "skip"):
                pkg_has_test_result.add(pkg)
        elif action == "fail":
            if ev.get("FailedBuild"):
                build_failures.append(ev["FailedBuild"])
            failed_pkgs.append(pkg)

    print(f"{a.lane}: {len(passed)} passed, {len(failed)} failed, {len(skipped)} skipped")
    for name in failed:
        print(f"  FAIL {name}")
    for name in sorted(set(build_failures)):
        print(f"  BUILD FAILED {name}")
    for pkg in failed_pkgs:
        print(f"  PACKAGE FAILED {pkg}")
    for name in skipped:
        why = allowed.get(name)
        print(f"  SKIP {name}" + (f"  (allowed: {why})" if why else ""))
    for name in sorted(set(allowed) - set(skipped)):
        print(f"  STALE allow-list entry {name}: it did not skip; remove it")

    bad = False
    if failed or failed_pkgs or build_failures:
        bad = True
    if garbage:
        print(f"{a.lane}: {garbage} line(s) are not go test -json events; failing closed", file=sys.stderr)
        bad = True
    if not (passed or failed or skipped):
        print(f"{a.lane}: no tests ran; a lane that ran nothing has not passed", file=sys.stderr)
        bad = True
    unlisted = [n for n in skipped if n not in allowed]
    if a.fail_on_skip and unlisted:
        print(f"{a.lane}: {len(unlisted)} test(s) skipped that no allow-list entry explains, and a "
              "skipped test did not run", file=sys.stderr)
        bad = True
    return 1 if bad else 0


if __name__ == "__main__":
    sys.exit(main())
