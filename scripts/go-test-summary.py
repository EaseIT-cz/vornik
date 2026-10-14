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
    stale so the list shrinks). An entry "<package>.<Test><TAB>@<ci-job-id>
    <TAB><reason>" is host-conditional: it skips only where the host lacks a
    prerequisite, and <ci-job-id> is the EE CI job that runs it; one that ran
    here is reported RAN, not stale (CE export runbook, amendment 2026-10-12);
  - a test named by --require (repeatable; "<Test>" or "<package>.<Test>")
    did not pass: a lane that selects named tests with -run must not stay
    green when one of them is renamed and silently stops matching (Hermes
    lane design, amendment 2026-10-12, GitHub #337).

The caller keeps `go test`'s own exit status as well; this script reads the
stream from a FILE, so nothing depends on a pipe's exit status.

Usage: go-test-summary.py [--fail-on-skip] [--allow-skips FILE] [--require TEST]... [--lane NAME] STREAM.json
       go-test-summary.py --print-output STREAM.json   (the test log only; exit 0)
"""
import argparse
import json
import sys


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--fail-on-skip", action="store_true")
    ap.add_argument("--allow-skips")
    ap.add_argument("--lane", default="tests")
    ap.add_argument("--require", action="append", default=[])
    ap.add_argument("--print-output", action="store_true")
    ap.add_argument("stream")
    a = ap.parse_args()

    allowed = {}
    conditional = {}  # host-conditional entries: test -> the CI job that runs it
    if a.allow_skips:
        try:
            with open(a.allow_skips, encoding="utf-8") as fh:
                for n, raw in enumerate(fh, 1):
                    line = raw.strip()
                    if not line or line.startswith("#"):
                        continue
                    fields = raw.rstrip("\r\n").split("\t")
                    name, job = fields[0].strip(), ""
                    if len(fields) == 3:
                        # Host-conditional: <name> TAB @<ci-job-id> TAB <reason>.
                        job = fields[1].strip()
                        if not job.startswith("@") or not job[1:].strip():
                            print(f"{a.lane}: {a.allow_skips}:{n}: {name!r}: the middle field must be "
                                  "@<ci-job-id>, the EE CI job that runs this test", file=sys.stderr)
                            return 1
                        job = job[1:].strip()
                    elif len(fields) > 3:
                        print(f"{a.lane}: {a.allow_skips}:{n}: {name!r} has {len(fields)} fields; "
                              "an entry is <test> TAB <reason>, or <test> TAB @<ci-job-id> TAB <reason> "
                              "(a reason holds no TAB)", file=sys.stderr)
                        return 1
                    reason = fields[-1].strip() if len(fields) > 1 else ""
                    if not reason:
                        print(f"{a.lane}: {a.allow_skips}:{n}: {name!r} has no reason; every "
                              "allowed skip says why it is by design", file=sys.stderr)
                        return 1
                    allowed[name] = reason
                    if job:
                        conditional[name] = job
        except OSError as e:
            print(f"{a.lane}: cannot read {a.allow_skips}: {e}", file=sys.stderr)
            return 1

    try:
        with open(a.stream, encoding="utf-8") as fh:
            lines = fh.read().splitlines()
    except OSError as e:
        print(f"{a.lane}: cannot read {a.stream}: {e}", file=sys.stderr)
        return 1

    if a.print_output:
        # The test log the stream carries, for a failed lane's reader; a
        # line that is not an event is skipped (the summary run reports it).
        for line in lines:
            try:
                ev = json.loads(line)
            except ValueError:
                continue
            if isinstance(ev, dict) and ev.get("Action") == "output":
                sys.stdout.write(ev.get("Output", ""))
        return 0

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
    seen = set(passed) | set(failed) | set(skipped)
    for name in sorted(set(allowed) - set(skipped)):
        job = conditional.get(name)
        if job and name in seen:
            # Ran here because this host has the prerequisite; that is not
            # evidence the entry is wrong (runbook, amendment 2026-10-12).
            print(f"  RAN {name}  (host-conditional; covered by {job})")
        elif job:
            print(f"  STALE allow-list entry {name}: it neither skipped nor ran; renamed or gone?")
        else:
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
    for req in a.require:
        if not any(n == req or n.endswith("." + req) for n in passed):
            print(f"{a.lane}: required test {req} did not pass (not run, skipped or failed)", file=sys.stderr)
            bad = True
    return 1 if bad else 0


if __name__ == "__main__":
    sys.exit(main())
