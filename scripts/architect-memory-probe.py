#!/usr/bin/env python3
"""Acceptance probe for the Vornik architect's memory (GitHub #550).

Calls the REST memory search exactly as the architect's consultant role does
(wrap=untrusted, its limit) with the twelve questions of
https://docs.vornik.io appendix A, and
reports per question how many hits came back and how many are design
documents the daemon checked against the trusted branch in the expected
repository scope.

An operator key is not given the role's pinned policy (only the role's task
key is), so the probe passes the policy as query parameters (--scope,
--strict, --authority). Run it without them to measure what an unfiltered
search returns.

Acceptance (design §2.1 C3, corrected 2026-10-25 after the first live run)
with the policy: every hit is provenance-verified in the scope, and every
question returns at least 3 hits. Whether a hit answers its question is the
consultant's judgment ("Not found in the design record"), not the search's:
a vector search returns nearest neighbours, so even a question no design
answers gets checked design hits. Exit 0 when accepted, 1 when not.

    scripts/architect-memory-probe.py --scope github.com/EaseIT-cz/vornik \\
        --strict --authority provenance

The key is read from --key-file (default ~/.config/vornik/secrets/admin-key.txt,
the SWARMD_ADMIN_KEY= line or the file's first line) and never printed.
"""

import argparse
import json
import os
import re
import sys
import urllib.parse
import urllib.request

QUESTIONS = [
    "How does the executor fall back to modelFallback when a model fails?",
    "How is a companion-delegated memory chunk verified?",
    "What does the A2A inbound publish gate require for a workflow to be published?",
    "How are broker write actions approved and executed?",
    "How does Vornik scope memory recall by repository?",
    "What retention horizon applies to chat_audit_log?",
    "How does the provenance sweep decide a chunk is stale?",
    "Which process may start a subprocess in the daemon (spawn law)?",
    "How are agent containers sandboxed and what memory limit do they get?",
    "How does the configuration assistant consult the architect over A2A?",
    "What happens when the Ollama Cloud weekly limit is hit?",
    "How does Vornik redact secrets before storing an upload?",
]
MIN_HITS = 3


def unwrap(value):
    """Strip the untrusted-content wrapper the model-facing response adds."""
    value = value or ""
    m = re.search(r">\s*(.*?)\s*</", value, re.S)
    return m.group(1) if m else value


def checked(hit, scope):
    """A hit counts when the daemon verified it by provenance in scope.

    memory.ProvenanceLabel emits "verified" (a clone) and "verified (forge
    API)" (a self-hosted forge); every other label ("unavailable: ...",
    "unverified: ...") does not begin with "verified".
    """
    prov = unwrap(hit.get("provenance", ""))
    return prov.startswith("verified") and unwrap(hit.get("repo_scope", "")) == scope


def judge(per_question, scope):
    """Return (accepted, lines). per_question: list of hit lists, 12 long."""
    ok, lines = True, []
    for n, hits in enumerate(per_question, 1):
        good = sum(1 for h in hits if checked(h, scope))
        bad = len(hits) - good
        passed = bad == 0 and len(hits) >= MIN_HITS
        ok = ok and passed
        lines.append(f"{'ok  ' if passed else 'FAIL'} Q{n:<2} hits={len(hits)} checked={good} other={bad}  {QUESTIONS[n - 1][:60]}")
    return ok, lines


def read_key(path):
    with open(os.path.expanduser(path), encoding="utf-8") as fh:
        text = fh.read()
    m = re.search(r"^SWARMD_ADMIN_KEY=(.+)$", text, re.M)
    return (m.group(1) if m else text.splitlines()[0]).strip()


def search(base, project, key, query, args):
    params = {"wrap": "untrusted", "limit": str(args.limit), "q": query}
    if args.scope:
        params["repo_scope"] = args.scope
    if args.strict:
        params["strict_scope"] = "true"
    if args.authority:
        params["authority"] = args.authority
    url = f"{base}/api/v1/projects/{project}/memory/search?" + urllib.parse.urlencode(params)
    req = urllib.request.Request(url, headers={"X-API-Key": key})
    with urllib.request.urlopen(req, timeout=30) as resp:
        body = json.load(resp)
    return body.get("results") or []


def selftest():
    scope = "github.com/x/y"
    design = {"provenance": "verified", "repo_scope": scope}
    review = {"provenance": "", "repo_scope": scope}
    wrapped = {"provenance": "<memory_hit>verified</memory_hit>", "repo_scope": f"<memory_repo_scope>{scope}</memory_repo_scope>"}
    good = [[design, design, wrapped] for _ in QUESTIONS]
    assert judge(good, scope)[0], "a clean probe must be accepted"
    leaky = [list(q) for q in good]
    leaky[0] = [design, design, review]
    assert not judge(leaky, scope)[0], "a review hit must fail the probe"
    thin = [list(q) for q in good]
    thin[1] = [design]
    assert not judge(thin, scope)[0], "a question with too few hits must fail"
    empty = [list(q) for q in good]
    empty[10] = []
    assert not judge(empty, scope)[0], "a question with no hits must fail"
    print("selftest ok")


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--url", default=os.environ.get("VORNIK_URL", "http://localhost:8080"))
    ap.add_argument("--project", default="companion-example")
    ap.add_argument("--key-file", default="~/.config/vornik/secrets/admin-key.txt")
    ap.add_argument("--scope", default="")
    ap.add_argument("--strict", action="store_true")
    ap.add_argument("--authority", default="")
    ap.add_argument("--limit", type=int, default=8)
    ap.add_argument("--selftest", action="store_true", help="check the acceptance rule on canned hits and exit")
    args = ap.parse_args()
    if args.selftest:
        selftest()
        return 0
    key = read_key(args.key_file)
    per_question = [search(args.url.rstrip("/"), args.project, key, q, args) for q in QUESTIONS]
    accepted, lines = judge(per_question, args.scope)
    print("\n".join(lines))
    print("ACCEPTED" if accepted else "NOT ACCEPTED")
    return 0 if accepted else 1


if __name__ == "__main__":
    sys.exit(main())
