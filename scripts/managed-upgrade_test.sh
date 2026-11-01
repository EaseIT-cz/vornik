#!/usr/bin/env bash
# managed-upgrade_test.sh — the managed upgrade command with fake rpm,
# systemctl, vornikctl, idle gate, rpm2cpio and cpio.
#
# GitHub #581 D2 (enterprise-packaging-design.md, amendment 2026-10-29, D2
# round 3): the rollback boundary is the unit becoming active; nothing is
# changed before the checks pass, the EULA is never accepted without the
# matching hash, a failed start rolls back by itself, and a started upgrade is
# final.
set -u
here=$(cd "$(dirname "$0")" && pwd)
cmd="$here/managed-upgrade.sh"
work=$(mktemp -d "${TMPDIR:-/tmp}/managed-upgrade-test.XXXXXX")
trap 'rm -rf "$work"' EXIT
mkdir -p "$work/bin"
fail=0

# The fakes read their behaviour from files in $work/s and append every call
# to $work/s/calls.
s="$work/s"
cat > "$work/bin/rpm" <<STUB
#!/usr/bin/env bash
s="$s"
echo "rpm \$*" >> "\$s/calls"
case "\$1" in
  -K) case "\$(basename "\$2")" in *"signatures OK"*) echo "\$2: digests OK"; exit 0 ;; esac
      [ -f "\$s/unsigned_\$(basename "\$2")" ] && { echo "\$2: digests OK"; exit 0; }
      echo "\$2: digests signatures OK" ;;
  -qp) case "\$(basename "\$4")" in new.rpm|*"signatures OK"*.rpm) echo -n 2.0-1 ;; old.rpm) echo -n 1.0-1 ;; *) exit 1 ;; esac ;;
  -q) cat "\$s/installed" ;;
  --eval) v=\$(printf '%s' "\$2" | sed -n "s/.*vercmp('\\([^']*\\)', '\\([^']*\\)').*/\\1 \\2/p")
      set -- \$v
      if [ "\$1" = "\$2" ]; then echo 0; elif [ "\$(printf '%s\n%s\n' "\$1" "\$2" | sort -V | tail -n1)" = "\$1" ]; then echo 1; else echo -1; fi ;;
  -U) [ -f "\$s/rpm_fail" ] && exit 1
      case "\$*" in *new.rpm*) echo -n 2.0-1 > "\$s/installed" ;; *old.rpm*) echo -n 1.0-1 > "\$s/installed" ;; esac ;;
  *) echo "fake rpm: unexpected \$*" >&2; exit 99 ;;
esac
STUB
cat > "$work/bin/systemctl" <<STUB
#!/usr/bin/env bash
s="$s"
echo "systemctl \$*" >> "\$s/calls"
case "\$1" in
  is-active) [ -f "\$s/active" ] ;;
  stop) rm -f "\$s/active" ;;
  start) if [ -f "\$s/start_fail_\$(cat "\$s/installed")" ]; then exit 1; fi; touch "\$s/active" ;;
  *) exit 99 ;;
esac
STUB
cat > "$work/bin/vornikctl" <<STUB
#!/usr/bin/env bash
s="$s"
echo "vornikctl \$* config=\${VORNIK_CONFIG:-}" >> "\$s/calls"
case "\$1 \$2" in
  "backup --out") [ -f "\$s/backup_fail" ] && exit 1; echo snap > "\$3" ;;
  "restore --from") [ -f "\$3" ] || exit 1 ;;
  "daemon operations-credentials") printf 'url=http://10.0.0.5:8081\nkey=sk-current\nprevious=\n' ;;
  "doctor --json") [ "\$VORNIK_API_KEY" = sk-current ] || exit 1
      printf '{"checks":[{"name":"a","status":"OK"},{"name":"b","status": "ERROR"}]}\n'; exit 1 ;;
  *) echo "fake vornikctl: unexpected \$*" >&2; exit 99 ;;
esac
STUB
cat > "$work/bin/gate" <<STUB
#!/usr/bin/env bash
echo "gate \$*" >> "$s/calls"
[ -f "$s/gate_fail" ] && exit 3
exit 0
STUB
# The extracted daemon: eula check passes unless eula_needed exists.
cat > "$work/bin/cpio" <<STUB
#!/usr/bin/env bash
cat >/dev/null
echo "cpio \$*" >> "$s/calls"
mkdir -p usr/bin
cat > usr/bin/vornik-enterprise <<'BIN'
#!/usr/bin/env bash
case "\$1" in
  eula) [ -f "$s/eula_needed" ] && exit 1; exit 0 ;;
  --eula-version) echo "EULA version 3 (sha256 abc123)" ;;
esac
BIN
chmod +x usr/bin/vornik-enterprise
STUB
cat > "$work/bin/rpm2cpio" <<STUB
#!/usr/bin/env bash
echo "rpm2cpio \$*" >> "$s/calls"
echo archive
STUB
cat > "$work/bin/daemon" <<STUB
#!/usr/bin/env bash
echo "daemon \$*" >> "$s/calls"
STUB
chmod +x "$work/bin/"*

reset() {
  rm -rf "$s" "$work/state"; mkdir -p "$s" "$work/state"
  echo -n 1.0-1 > "$s/installed"; touch "$s/active" "$work/managed-image" "$work/new.rpm" "$work/old.rpm"
}
run() { # args...
  env MU_STATE_DIR="$work/state" MU_MARKER="$work/managed-image" MU_CONFIG="$work/vornik.yaml" \
    MU_GATE="$work/bin/gate" MU_DAEMON="$work/bin/daemon" MU_RPM="$work/bin/rpm" \
    MU_RPM2CPIO="$work/bin/rpm2cpio" MU_CPIO="$work/bin/cpio" MU_SYSTEMCTL="$work/bin/systemctl" \
    MU_VORNIKCTL="$work/bin/vornikctl" bash "$cmd" "$@" >"$work/out" 2>"$work/err"
  echo $?
}
up() { run --package "$work/new.rpm" --from-package "$work/old.rpm" "$@"; }
check() { # name want_rc want_stdout_substring rc
  if [ "$4" != "$2" ] || ! grep -q -- "$3" "$work/out"; then
    echo "FAIL: $1: exit $4 (want $2)"; sed 's/^/  out: /' "$work/out"; sed 's/^/  err: /' "$work/err"; fail=1
  else
    echo "managed-upgrade: $1 — OK"
  fi
}
never() { # name pattern: no call matches
  if grep -q -- "$2" "$s/calls" 2>/dev/null; then
    echo "FAIL: $1: a call matched [$2]:"; sed 's/^/  /' "$s/calls"; fail=1
  else
    echo "managed-upgrade: $1 — OK"
  fi
}
ever() { # name pattern
  if ! grep -q -- "$2" "$s/calls" 2>/dev/null; then
    echo "FAIL: $1: no call matched [$2]:"; sed 's/^/  /' "$s/calls" 2>/dev/null; fail=1
  else
    echo "managed-upgrade: $1 — OK"
  fi
}

reset; rc=$(up)
check "an upgrade upgrades" 0 '"ok":true,"release":"2.0-1","result":"upgraded","from":"1.0-1","to":"2.0-1","doctor_errors":"1"' "$rc"
[ -e "$work/state/state.json" ] && { echo "FAIL: state.json left after a successful start"; fail=1; }
ever "it drains with the config and the default TTL before it stops" "gate --drain --drain-ttl 300 --config $work/vornik.yaml"
reset; rc=$(up --drain-ttl 900)
ever "--drain-ttl reaches the gate (vornik-cloud's drainTtl)" "gate --drain --drain-ttl 900 --config"
ever "the snapshot reads the instance's config" "vornikctl backup --out $work/state/pre-1.0-1-2.0-1.tgz config=$work/vornik.yaml"
never "a started upgrade is final: no restore" "vornikctl restore"
order=$(grep -n -E '^(gate|systemctl stop|vornikctl backup|rpm -U|systemctl start)' "$s/calls" | cut -d' ' -f1-2 | tr '\n' ' ')
case "$order" in
  *"gate --drain"*"systemctl stop"*"vornikctl backup"*"rpm -U"*"systemctl start"*) echo "managed-upgrade: the order gate, stop, snapshot, install, start — OK" ;;
  *) echo "FAIL: the order: $order"; fail=1 ;;
esac

reset; echo -n 2.0-1 > "$s/installed"; rc=$(up)
check "a same or older version is refused" 1 '"step":"check"' "$rc"
never "a refused version changes nothing" "rpm -U\|systemctl stop\|gate"

reset; echo -n "1.0-1'x" > "$s/installed"; rc=$(up)
check "a version outside RPM's characters never reaches vercmp" 1 '"step":"check"' "$rc"
never "a bad version reaches no lua" "rpm --eval"

reset; touch "$s/unsigned_new.rpm"; rc=$(up)
check "an unsigned package is refused" 1 "signature does not verify" "$rc"

# Commit security review of 2b7a928c3: a file NAMED like the OK line is not
# a signed package, and every step works on a private copy, never the path
# the caller passed (which it could swap between the check and the install).
reset; : > "$work/x signatures OK.rpm"
rc=$(run --package "$work/x signatures OK.rpm" --from-package "$work/old.rpm")
check "an unsigned package named like the OK line is refused" 1 "signature does not verify" "$rc"
reset; rc=$(up)
never "rpm -U never installs the caller's path" "rpm -U $work/new.rpm"
ever "rpm -U enforces the signature itself" "rpm -U --define _pkgverify_level signature"

reset; rm -f "$work/managed-image"; rc=$(up)
check "not a managed image is refused" 1 "not a managed image" "$rc"

reset; touch "$s/eula_needed"; rc=$(up)
check "the EULA without the hash is refused" 1 '"step":"eula"' "$rc"
never "the EULA refusal is before any change" "rpm -U\|systemctl stop\|gate\|accept-eula"

reset; touch "$s/eula_needed"; rc=$(up --eula-hash wrong)
check "the EULA with a wrong hash is refused" 1 '"step":"eula"' "$rc"

reset; touch "$s/eula_needed"; rc=$(up --eula-hash abc123)
check "the EULA with the matching hash is accepted" 0 '"result":"upgraded"' "$rc"
ever "it accepts with the installed binary" "daemon --accept-eula"

reset; touch "$s/gate_fail"; rc=$(up)
check "a gate that never idles stops nothing" 1 '"step":"gate"' "$rc"
never "a gate failure stops nothing" "systemctl stop\|rpm -U"

reset; touch "$s/backup_fail"; rc=$(up)
check "a failed snapshot installs nothing and starts again" 1 '"step":"snapshot"' "$rc"
never "a failed snapshot installs nothing" "rpm -U"
[ -f "$s/active" ] || { echo "FAIL: a failed snapshot left the unit stopped"; fail=1; }

reset; touch "$s/start_fail_2.0-1"; rc=$(up)
check "a failed start rolls back by itself" 1 '"ok":false,"release":"2.0-1","result":"rolled_back","step":"start"' "$rc"
ever "the rollback reinstalls a private copy of the from-version, signature enforced" "rpm -U --define _pkgverify_level signature --oldpackage --replacepkgs .*/from/old.rpm"
ever "the rollback restores the snapshot" "vornikctl restore --from $work/state/pre-1.0-1-2.0-1.tgz --force --clean"
[ "$(cat "$s/installed")" = 1.0-1 ] && [ -f "$s/active" ] && [ ! -e "$work/state/state.json" ] ||
  { echo "FAIL: after the rollback: installed $(cat "$s/installed"), state.json $(ls "$work/state")"; fail=1; }

reset; touch "$s/start_fail_2.0-1" "$s/start_fail_1.0-1"; rc=$(up)
check "a failed rollback keeps the state for --rollback" 1 '"result":"failed","step":"start"' "$rc"
[ -e "$work/state/state.json" ] || { echo "FAIL: state.json removed after a failed rollback"; fail=1; }
rm -f "$s/start_fail_1.0-1"; : > "$s/calls"; rc=$(run --rollback --from-package "$work/old.rpm")
check "--rollback completes it" 0 '"result":"rolled_back","step":"rollback"' "$rc"
ever "--rollback at phase installed restores" "vornikctl restore"

reset; mkdir -p "$work/state"; printf '{"phase":"installed","from":"1.0-1","to":"2.0-1","snapshot":"x"}\n' > "$work/state/state.json"
rc=$(run --rollback --from-package "$work/old.rpm")
check "--rollback is refused while the unit is active" 1 "the unit is active" "$rc"
never "a refused rollback changes nothing" "rpm -U\|restore\|systemctl stop"

reset; rc=$(run --rollback --from-package "$work/old.rpm")
check "--rollback with no state file is refused" 1 "no upgrade is in progress" "$rc"

reset; rm -f "$s/active"; printf '{"phase":"stopped","from":"1.0-1","to":"2.0-1","snapshot":"x"}\n' > "$work/state/state.json"
rc=$(run --rollback --from-package "$work/old.rpm")
check "--rollback at phase stopped just starts" 0 '"result":"rolled_back"' "$rc"
never "phase stopped needs no restore or reinstall" "vornikctl restore\|rpm -U"

reset; rm -f "$s/active"; printf '{"phase":"stopped","from":"1.0-1","to":"2.0-1","snapshot":"x"}\n' > "$work/state/state.json"
rc=$(up)
check "an upgrade is refused while one is in progress" 1 "an upgrade is in progress" "$rc"

reset; echo -n 2.0-1 > "$s/installed"; printf '{"phase":"installed","from":"1.0-1","to":"2.0-1","snapshot":"x"}\n' > "$work/state/state.json"
rc=$(run --package "$work/new.rpm" --from-package "$work/old.rpm")
check "a stale state file at the to-version is removed (then the same version is refused)" 1 "is not newer" "$rc"

reset; touch "$s/rpm_fail"; rc=$(up)
check "a failed install rolls back" 1 '"result":"failed","step":"install"' "$rc"

for args in "" "--package" "--package x" "--bogus" "--rollback --package x --from-package y" \
  "--package x --from-package y --drain-ttl 0" "--package x --from-package y --drain-ttl 3601" "--package x --from-package y --drain-ttl 1m"; do
  # shellcheck disable=SC2086
  rc=$(run $args)
  if [ "$rc" != 64 ] || ! grep -q '^usage: managed-upgrade' "$work/err"; then
    echo "FAIL: bad usage [$args]: exit $rc"; sed 's/^/  /' "$work/err"; fail=1
  else
    echo "managed-upgrade: bad usage [$args] refused — OK"
  fi
done

# The package ships both commands where the control plane calls them.
gr="$here/../.goreleaser.enterprise.yaml"
if [ -f "$gr" ]; then
  for pair in "managed-upgrade.sh /usr/libexec/vornik-enterprise/managed-upgrade" \
    "deploy-idle-gate.sh /usr/libexec/vornik-enterprise/deploy-idle-gate"; do
    # shellcheck disable=SC2086 # two words by design
    set -- $pair
    if grep -A1 -- "- src: scripts/$1" "$gr" | grep -q -- "dst: $2\$"; then
      echo "managed-upgrade: the package installs scripts/$1 as $2 — OK"
    else
      echo "FAIL: .goreleaser.enterprise.yaml does not install scripts/$1 as $2"; fail=1
    fi
  done
fi

[ "$fail" = 0 ] && echo "managed-upgrade_test.sh: PASS"
exit "$fail"
