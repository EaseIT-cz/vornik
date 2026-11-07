#!/usr/bin/env bash
# architect-brief-input_test.sh — the brief input refuses an unmarked design
# and drops an excluded section (#614 C1).
set -uo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
failures=0
ok() { echo "  ok   $1"; }
bad() { echo "  FAIL $1"; failures=$((failures + 1)); }

printf '# U\n\n## Residual risks\n\nSECRET\n' >"$work/unmarked.md"
printf -- '---\ndisclosure: customer\ndisclosureExclude:\n    - "## 5. Residual risks"\n---\n# C\n\n## 1. Flow\n\nThe flow.\n\n## 5. Residual risks\n\nSECRET gap.\n\n## 6. Tests\n' >"$work/customer.md"

if "$here/architect-brief-input.sh" "$work/unmarked.md" >"$work/out1" 2>"$work/err1"; then
  bad "an unmarked design was accepted"
else
  if grep -q "not marked disclosure: customer" "$work/err1"; then ok "an unmarked design is refused, with the reason"; else bad "the refusal does not say why"; fi
fi
if "$here/architect-brief-input.sh" "$work/customer.md" >"$work/out2" 2>"$work/err2"; then
  if grep -q SECRET "$work/out2"; then bad "the excluded section reached the output"; else ok "the excluded section is dropped"; fi
  if grep -q "## 6. Tests" "$work/out2"; then ok "the rest of the design is kept"; else bad "the rest of the design was lost"; fi
else
  bad "a customer design was refused: $(cat "$work/err2")"
fi

if [ "$failures" -ne 0 ]; then
  echo "architect-brief-input_test: $failures failure(s)" >&2
  exit 1
fi
echo "architect-brief-input_test: OK"
