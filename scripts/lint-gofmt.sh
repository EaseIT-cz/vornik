#!/usr/bin/env bash
# lint-gofmt.sh — fail when any Go file git knows about is not gofmt-clean.
#
# "Knows about" is the tracked files (git ls-files) PLUS the untracked files
# that are not ignored (git ls-files -o --exclude-standard). GitHub #415 item 2
# (found 2026-10-16 while fixing #394): the target listed only tracked files,
# so a new Go file that was not gofmt-clean passed `make lint` until the
# moment it was committed, which is after the gate that was meant to stop it.
# Ignored files (build output, .claude/ worktrees, vendored scratch) stay out.
#
# Prints the denominator on success and the unclean files on failure (CI
# pipeline design, amendments 2026-10-07 T9 and 2026-10-17).
#
# LINT_GOFMT_DIR points it at another checkout (the test suite's fixtures);
# default: the current directory.
set -uo pipefail
dir="${LINT_GOFMT_DIR:-.}"
list="$(mktemp)" || exit 1
trap 'rm -f "$list"' EXIT
if ! git -C "$dir" ls-files -z --cached --others --exclude-standard -- '*.go' > "$list"; then
  echo "lint-gofmt: git ls-files failed (not a git checkout?)"
  exit 1
fi
# A tracked file deleted from the working tree is listed by --cached but has
# nothing to format; gofmt would fail on it. Keep only files that exist.
existing="$(mktemp)" || exit 1
trap 'rm -f "$list" "$existing"' EXIT
while IFS= read -r -d '' f; do
  [ -f "$dir/$f" ] || continue
  if [ "$dir" = . ]; then printf '%s\0' "$f"; else printf '%s\0' "$dir/$f"; fi
done < "$list" > "$existing"
n=$(tr -cd '\0' < "$existing" | wc -c)
if [ "$n" -eq 0 ]; then
  echo "lint-gofmt: no Go files found"
  exit 1
fi
unclean=$(xargs -0 gofmt -l < "$existing" 2>&1); rc=$?
if [ $rc -ne 0 ] && [ -z "$unclean" ]; then echo "lint-gofmt: gofmt failed"; exit 1; fi
if [ -n "$unclean" ]; then
  echo "lint-gofmt: not gofmt-clean:"
  if [ "$dir" = . ]; then echo "$unclean"; else echo "${unclean//"$dir/"/}"; fi
  echo "  fix: gofmt -w <files above>"
  echo "  if the list looks unrelated to your change, check your Go version"
  echo "  matches CI's GO_VERSION (.github/workflows/ci.yaml)"
  exit 1
fi
echo "lint-gofmt: OK — $n Go files gofmt-clean (tracked and untracked, not ignored)"
