#!/usr/bin/env bash
# Tests for scripts/agentbench-import-polyglot.py (benchmark design,
# Amendment 2026-11-16, GitHub #650 phase 1), on fixture repositories shaped
# like Aider's polyglot list and two Exercism tracks.
set -u
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
IMPORT="$HERE/agentbench-import-polyglot.py"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
pass=0
fail=0
ok() { echo "PASS: $1"; pass=$((pass + 1)); }
bad() { echo "FAIL: $1"; fail=$((fail + 1)); }
check() { # name command...
  local name=$1
  shift
  if "$@"; then ok "$name"; else bad "$name"; fi
}

repo() { # dir
  git -C "$1" init -q && git -C "$1" -c user.name=t -c user.email=t@e.c add -A &&
    git -C "$1" -c user.name=t -c user.email=t@e.c commit -q -m fixture
}
put() { # path content
  mkdir -p "$(dirname "$1")" && printf '%s' "$2" > "$1"
}

P="$TMP/polyglot"; G="$TMP/go"; Y="$TMP/python"
for ex in go/exercises/practice/leap go/exercises/practice/ledger python/exercises/practice/two-fer; do
  put "$P/$ex/.keep" ''
done
repo "$P"

L="$G/exercises/practice/leap"
put "$L/.meta/config.json" '{"files":{"solution":["leap.go"],"test":["leap_test.go"],"example":[".meta/example.go"],"editor":["defs.go"]}}'
put "$L/.docs/instructions.md" '# Instructions

Is the year a leap year?'
put "$L/leap.go" 'package leap

func IsLeapYear(year int) bool { panic("implement") }
'
put "$L/defs.go" 'package leap

type Year int
'
put "$L/leap_test.go" 'package leap

import "testing"

func TestLeapYears(t *testing.T) {}
'
put "$L/.meta/example.go" 'package leap

func IsLeapYear(year int) bool { return year%4 == 0 && (year%100 != 0 || year%400 == 0) }
'
put "$L/go.mod" 'module leap

go 1.27
'
put "$G/exercises/practice/ledger/.meta/config.json" '{"files":{"solution":["ledger.go"],"test":["ledger_test.go"],"example":[".meta/example.go"]}}'
put "$G/LICENSE" 'MIT License

Copyright (c) 2021 Exercism
'
repo "$G"

T="$Y/exercises/practice/two-fer"
put "$T/.meta/config.json" '{"files":{"solution":["two_fer.py"],"test":["two_fer_test.py"],"example":[".meta/example.py"]}}'
put "$T/.docs/instructions.md" '# Instructions

Say one for you.'
put "$T/two_fer.py" 'def two_fer(name="you"):
    pass
'
put "$T/two_fer_test.py" 'import unittest
from two_fer import two_fer
'
put "$T/.meta/example.py" 'def two_fer(name="you"):
    return f"One for {name}, one for me."
'
repo "$Y"

OUT="$TMP/out"
if python3 -I "$IMPORT" --polyglot "$P" --go "$G" --python "$Y" --out "$OUT" > /dev/null; then
  ok "the importer runs"
else
  bad "the importer failed"
fi

SET="$OUT/polyglot-go-python-v1.json"
n=$(python3 -c 'import json,sys; print(len(json.load(open(sys.argv[1]))))' "$SET" 2>/dev/null)
check "two tasks: the excluded ledger is left out" test "$n" = 2
check "no files for the excluded exercise" test ! -e "$OUT/testdata/polyglot/go/ledger"

S="$OUT/testdata/polyglot/go/leap"
check "the seed holds the stub" grep -q 'panic("implement")' "$S/seed/leap.go"
check "the seed holds the editor file" grep -q 'type Year int' "$S/seed/defs.go"
check "the suite restores the editor file" grep -q 'type Year int' "$S/suite/defs.go"
check "go.mod asks for the image's Go" grep -qx 'go 1.26' "$S/seed/go.mod"
check "Go tests are renamed for the grader" grep -q 'func TestAcceptance_LeapYears(' "$S/suite/zz_acceptance_leap_test.go"
check "the reference is the example under the stub's name" grep -q 'year%400' "$S/reference/leap.go"
check "the seed carries no test" test ! -e "$S/seed/leap_test.go"

Y2="$OUT/testdata/polyglot/python/two-fer"
check "the Python stub is in the seed" test -f "$Y2/seed/two_fer.py"
check "the Python test is in the suite" test -f "$Y2/suite/two_fer_test.py"
check "the Python test is not in the seed" test ! -e "$Y2/seed/two_fer_test.py"

fields() {
  python3 - "$SET" <<'PY'
import json, sys
for t in json.load(open(sys.argv[1])):
    tg = t["targets"][0]
    assert t["seed"] == "testdata/" + tg + "/seed" and t["acceptance"] == "testdata/" + tg + "/suite", t
    assert tg in t["prompt"] and t["tier"] == "exploratory" and t["workflow"] == "dev-pipeline", t
PY
}
check "each task names its target, seed and suite and pins the target in its prompt" fields

check "the MIT notice ships" grep -q 'Copyright (c) 2021 Exercism' "$OUT/testdata/polyglot/NOTICE"
source_pins() {
  python3 -c 'import json,sys; s=json.load(open(sys.argv[1])); assert len(s["go"]["commit"])==40 and "go/ledger" in s["excluded"]' \
    "$OUT/testdata/polyglot/SOURCE.json" 2>/dev/null
}
check "SOURCE.json pins the commits and the exclusions" source_pins

OUT2="$TMP/out2"
python3 -I "$IMPORT" --polyglot "$P" --go "$G" --python "$Y" --out "$OUT2" > /dev/null
check "a second run writes the same files" diff -r "$OUT" "$OUT2"

put "$P/python/exercises/practice/ghost/.keep" ''
git -C "$P" -c user.name=t -c user.email=t@e.c add -A
git -C "$P" -c user.name=t -c user.email=t@e.c commit -q -m ghost
if python3 -I "$IMPORT" --polyglot "$P" --go "$G" --python "$Y" --out "$TMP/out3" > /dev/null 2>&1; then
  bad "an exercise missing from the track was accepted"
else
  ok "an exercise missing from the track is refused"
fi

echo "agentbench-import-polyglot: $pass passed, $fail failed"
[ "$fail" = 0 ]
