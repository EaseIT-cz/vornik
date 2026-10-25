#!/usr/bin/env bash
# with-env-file.sh <env-file> <command...> — run <command> with every
# variable in <env-file> exported OVER the caller's environment.
#
# Why (GitHub #515; trading-broker-design.md, amendment 2026-10-23, change 4):
# compose interpolation prefers a variable exported in the invoking shell over
# the same variable in --env-file. On 2026-10-01 broker-mcp was recreated from
# a shell that still exported the pre-rotation VORNIK_BROKER_DAEMON_API_KEY,
# and every audit POST was rejected for 22 days. Running compose through this
# makes the file win.
#
# The file is PARSED, never sourced (review 886c F1): `KEY=value` lines, an
# optional leading `export ` (whitespace tolerated), one pair of matching
# surrounding quotes stripped (not interpreted: no escape processing), an
# unquoted value's trailing ` # note` dropped, CR stripped, blank and `#`
# lines skipped. Nothing is expanded —
# `$`, backticks and spaces reach the command literally, as --env-file passes
# them. A line that is none of these fails closed, naming its line number but
# never its content (it may hold a secret).
#
# Scope (review 1fe1 L2): only variables the file sets are overridden; every
# other variable of the caller's environment passes through unchanged. This
# is a precedence fix, not an env-file-only sandbox.
set -uo pipefail
file="${1:?usage: with-env-file.sh <env-file> <command...>}"
shift
[[ $# -gt 0 ]] || { echo "with-env-file: no command given" >&2; exit 2; }
[[ -r "$file" ]] || { echo "with-env-file: cannot read $file" >&2; exit 2; }

n=0
while IFS= read -r line || [[ -n "$line" ]]; do
  n=$((n + 1))
  line="${line%$'\r'}"
  [[ "$line" =~ ^[[:space:]]*(#|$) ]] && continue
  # Leading whitespace and `export` padding are tolerated (review 8019 m1).
  [[ "$line" =~ ^[[:space:]]*(export[[:space:]]+)?(.*)$ ]] && line="${BASH_REMATCH[2]}"
  if [[ ! "$line" =~ ^([A-Za-z_][A-Za-z0-9_]*)=(.*)$ ]]; then
    echo "with-env-file: $file line $n is not KEY=value; refusing to run" >&2
    exit 2
  fi
  key="${BASH_REMATCH[1]}" val="${BASH_REMATCH[2]}"
  if [[ ${#val} -ge 2 && ( ( "${val:0:1}" == '"' && "${val: -1}" == '"' ) || ( "${val:0:1}" == "'" && "${val: -1}" == "'" ) ) ]]; then
    val="${val:1:${#val}-2}"
  else
    # An unquoted value ends at whitespace + `#`, as --env-file reads it; a
    # trailing note must not silently become part of the value (review 8019
    # m2). A `#` with no space before it stays (e.g. a token containing #).
    if [[ "$val" == *[[:space:]]#* ]]; then
      val="${val%%[[:space:]]#*}"          # cut at the FIRST whitespace+#
      val="${val%"${val##*[![:space:]]}"}" # then trim trailing whitespace
    fi
  fi
  export "$key=$val"
done < "$file"

exec "$@"
