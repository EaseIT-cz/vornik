#!/usr/bin/env bash
# managed-upgrade_test.sh — the managed upgrade command with fake rpm,
# systemctl, vornikctl, idle gate, rpm2cpio and cpio.
#
# GitHub #581 D2 (enterprise-packaging-design.md, amendment 2026-10-29, D2
# round 3): the rollback boundary is the unit becoming active; nothing is
# changed before the checks pass, the EULA is never accepted without the
# matching hash, a failed start rolls back by itself, and a started upgrade is
# final.
#
# GitHub #909 (amendment 2026-11-24): the new version starts held (#586); the
# upgrade verifies the held instance, then releases it, and is final at the
# release. Found at: review (#586 §13.8 item 7: the managed upgrade still
# started unheld). Regression test at: unit (this file, as every D2 case).
# Lower because: the CI-VM lane with a real RPM and systemd is the follow-up.
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
  -U) case "\$*" in *--test*)
        echo "rpm-test LC_ALL=\${LC_ALL:-}" >> "\$s/calls"
        if [ -f "\$s/deps_missing" ]; then
          printf 'error: Failed dependencies:\n\tgit is needed by vornik-enterprise-2.0-1.x86_64 \303\251\n' >&2
          exit 3
        fi
        exit 0 ;;
      esac
      [ -f "\$s/rpm_fail" ] && exit 1
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
  stop) rm -f "\$s/active" "\$s/held" ;;
  start) if [ -f "\$s/start_fail_\$(cat "\$s/installed")" ]; then exit 1; fi
      if [ -e "$work/state/start-held" ]; then
        cp "$work/state/start-held" "\$s/marker_seen"; stat -c %a "$work/state/start-held" > "\$s/marker_mode"; touch "\$s/held"
      fi
      touch "\$s/active" ;;
  *) exit 99 ;;
esac
STUB
cat > "$work/bin/vornikctl" <<STUB
#!/usr/bin/env bash
s="$s"
echo "vornikctl \$* config=\${VORNIK_CONFIG:-}" >> "\$s/calls"
case "\$1 \$2" in
  "backup --out") [ -f "\$s/backup_fail" ] && exit 1; echo snap > "\$3" ;;
  "restore --from") [ -f "\$3" ] || exit 1
      # The canary's restore error (#864), last line last.
      if [ -f "\$s/restore_fail" ]; then
        printf 'Error: psql restore failed: exit status 3\npsql:db.sql:26: ERROR:  permission denied to create extension vector\n' >&2
        exit 1
      fi ;;
  "daemon operations-credentials") printf 'url=http://10.0.0.5:8081\nkey=sk-current\nprevious=\n' ;;
  "doctor --json") [ "\$VORNIK_API_KEY" = sk-current ] || exit 1
      if [ -f "\$s/held" ]; then
        # indented, as the real --json prints it
        printf '{\n  "checks": [\n    {\n      "name": "start_held",\n      "status": "WARN",\n      "message": "held {x}"\n    }'
        [ -f "\$s/doctor_held_error" ] && printf ',\n    {\n      "name": "config_validation",\n      "status": "ERROR",\n      "message": "bad"\n    }'
        printf '\n  ],\n  "summary": "x"\n}\n'
        exit 0
      fi
      printf '{"checks":[{"name":"a","status":"OK"},{"name":"b","status": "ERROR"}]}\n'; exit 1 ;;
  "daemon release") [ "\$VORNIK_API_URL" = http://10.0.0.5:8081 ] && [ "\$VORNIK_API_KEY" = sk-current ] || exit 1
      case "\$(cat "\$s/release" 2>/dev/null)" in
        keep) exit 1 ;;                                    # refused: still held
        removed) rm -f "$work/state/start-held" "\$s/held"; exit 1 ;; # released, answer lost
        dead) rm -f "$work/state/start-held" "\$s/held" "\$s/active"; exit 1 ;; # failed, write-back failed
        late) ( sleep 1; rm -f "$work/state/start-held" "\$s/held" ) & exit 1 ;; # released during the settle wait
        dangling) rm -f "$work/state/start-held"; ln -s /nonexistent/x "$work/state/start-held"; exit 1 ;; # not a release
      esac
      rm -f "$work/state/start-held" "\$s/held"; echo "held: false" ;;
  *) echo "fake vornikctl: unexpected \$*" >&2; exit 99 ;;
esac
STUB
cat > "$work/bin/curl" <<STUB
#!/usr/bin/env bash
s="$s"
echo "curl \$*" >> "\$s/calls"
case "\$*" in *http://10.0.0.5:8081/readyz*) ;; *) exit 7 ;; esac
if [ -f "\$s/readyz" ]; then cat "\$s/readyz"; exit 0; fi
if [ -f "\$s/held" ]; then
  printf '{"checks":[{"name":"database","status":"ok"},{"name":"scheduler_heartbeat","status":"ok"},{"name":"chat_provider","status":"ok"},{"name":"retention_sweeper","status":"ok"}],"overdue":false,"status":"held"}'
else
  printf '{"checks":[{"name":"database","status":"ok"}],"status":"ready"}'
fi
STUB
cat > "$work/bin/runuser" <<STUB
#!/usr/bin/env bash
# runuser -u USER -- CMD...: records the user, runs CMD as the test's user.
echo "runuser \$1 \$2" >> "$s/calls"
[ "\$1" = -u ] && [ "\$3" = -- ] || exit 99
shift 3; exec "\$@"
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
# The acceptance as it failed on the canary (#865): EULA text on stdout, the
# refusal on stderr, exit 1.
if [ "\$1" = --accept-eula ] && [ -f "$s/accept_fail" ]; then
  echo "EULA BODY TEXT"
  if [ -f "$s/accept_msg" ]; then cat "$s/accept_msg" >&2
  else echo "EULA not accepted (you did not type 'accept')" >&2; fi
  exit 1
fi
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
    MU_VORNIKCTL="$work/bin/vornikctl" MU_CURL="$work/bin/curl" MU_RUNUSER="$work/bin/runuser" \
    MU_HOLD_MARKER="$work/state/start-held" MU_VERIFY_SECONDS=2 MU_SETTLE_SECONDS=3 \
    bash "$cmd" "$@" >"$work/out" 2>"$work/err"
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
# Patterns anchor at the program name (^gate, not gate): a temp path such as
# ~/tmp-gotest/gate-tmp/... matched the bare word (gate stamp, 2026-11-18).
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
never "a refused version changes nothing" "^rpm -U\|^systemctl stop\|^gate "

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
never "the EULA refusal is before any change" "^rpm -U\|^systemctl stop\|^gate \|accept-eula"

reset; touch "$s/eula_needed"; rc=$(up --eula-hash wrong)
check "the EULA with a wrong hash is refused" 1 '"step":"eula"' "$rc"

reset; touch "$s/eula_needed"; rc=$(up --eula-hash abc123)
check "the EULA with the matching hash is accepted" 0 '"result":"upgraded"' "$rc"
ever "it accepts with the installed binary" "daemon --accept-eula"

reset; touch "$s/gate_fail"; rc=$(up)
check "a gate that never idles stops nothing" 1 '"step":"gate"' "$rc"
never "a gate failure stops nothing" "systemctl stop\|rpm -U --define"

reset; touch "$s/backup_fail"; rc=$(up)
check "a failed snapshot installs nothing and starts again" 1 '"step":"snapshot"' "$rc"
never "a failed snapshot installs nothing" "rpm -U --define"
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

# GitHub #775 (design: amendment 2026-11-14): a missing requirement is found
# by rpm -U --test before anything changes. Found at: production (the first
# managed upgrade, 2026.11.4 to 2026.11.5, lacked git). Regression test at:
# unit (this file, as every D2 case). Lower because no lane holds a managed
# image with an older release and a missing requirement.
reset; touch "$s/deps_missing"; rc=$(up)
check "a missing requirement is refused before any change" 1 '"ok":false,"release":"2.0-1","result":"failed","step":"deps"' "$rc"
check "the reason names what is missing" 1 'git is needed by vornik-enterprise-2.0-1' "$rc"
never "a missing requirement changes nothing" "^gate \|^systemctl stop\|^vornikctl backup\|^rpm -U --define"
ever "the check runs in the C locale" "rpm-test LC_ALL=C"
[ ! -e "$work/state/state.json" ] || { echo "FAIL: a deps refusal left state.json"; fail=1; }
if [ "$(wc -l < "$work/out")" != 1 ] || LC_ALL=C grep -q '[^ -~]' "$work/out"; then
  echo "FAIL: a deps refusal's stdout is not one printable-ASCII line:"; sed 's/^/  out: /' "$work/out"; fail=1
else
  echo "managed-upgrade: a deps refusal is one printable-ASCII line — OK"
fi
grep -q 'git is needed by' "$work/err" || { echo "FAIL: rpm's own output did not reach stderr"; fail=1; }
reset; rc=$(up)
t=$(grep -n -m1 '^rpm -U --test' "$s/calls" | cut -d: -f1); g=$(grep -n -m1 '^gate --drain' "$s/calls" | cut -d: -f1)
if [ -n "$t" ] && [ -n "$g" ] && [ "$t" -lt "$g" ]; then
  echo "managed-upgrade: the dependency check comes before the gate — OK"
else
  echo "FAIL: the dependency check (line ${t:-none}) is not before the gate (line ${g:-none})"; fail=1
fi
ever "the check tests the private copy with the signature enforced" "rpm -U --test --define _pkgverify_level signature .*/new/new.rpm"

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

# GitHub #865: the acceptance's failure was sent to /dev/null, so the line
# said only "the install failed". Its stderr now reaches managed-upgrade's
# stderr and its last line the reason; the EULA text stays out.
reset; touch "$s/eula_needed" "$s/accept_fail"; rc=$(up --eula-hash abc123)
check "a failed acceptance rolls back" 1 '"result":"rolled_back","step":"install"' "$rc"
check "the reason names the acceptance's error" 1 "EULA acceptance: EULA not accepted (you did not type 'accept')" "$rc"
if grep -q "you did not type 'accept'" "$work/err" && ! grep -q "EULA BODY TEXT" "$work/err" "$work/out"; then
  echo "managed-upgrade: the acceptance's stderr is passed on, its stdout is not — OK"
else
  echo "FAIL: the acceptance's output: err:"; sed 's/^/  /' "$work/err"; fail=1
fi

# GitHub #864: a rollback whose restore failed said only that it failed.
reset; touch "$s/eula_needed" "$s/accept_fail" "$s/restore_fail"; rc=$(up --eula-hash abc123)
check "a failed restore's error reaches the reason" 1 '"result":"failed","step":"install","reason":"the install failed and so did the rollback (psql:db.sql:26: ERROR: permission denied to create extension vector)' "$rc"

# The reason stays one printable-ASCII line (#775's rule), whatever the
# command printed.
reset; touch "$s/eula_needed" "$s/accept_fail"
printf 'first\nbad\tline \303\251 \033[31m%0600d\n' 0 > "$s/accept_msg"
rc=$(up --eula-hash abc123)
reason=$(sed -n 's/.*"reason":"\([^"]*\)".*/\1/p' "$work/out")
if [ "$(wc -l < "$work/out")" = 1 ] && ! LC_ALL=C grep -q '[^ -~]' "$work/out" && [ "${#reason}" -le 620 ] && case "$reason" in *"bad line"*) true ;; *) false ;; esac; then
  echo "managed-upgrade: a hostile acceptance error leaves one printable line, cut — OK"
else
  echo "FAIL: the reason is not one printable, capped line:"; od -c "$work/out" | head; fail=1
fi

# GitHub #909: the held start (amendment 2026-11-24).
marker="$work/state/start-held"
reset; rc=$(up)
check "#909: a held upgrade verifies, releases and is final" 0 '"result":"upgraded"' "$rc"
order=$(grep -E '^(systemctl start|curl |vornikctl doctor|vornikctl daemon release)' "$s/calls" | cut -d' ' -f1-3 | tr '\n' '|')
case "$order" in
  "systemctl start vornik-enterprise|curl"*"vornikctl doctor --json|vornikctl daemon release|vornikctl doctor --json|") echo "managed-upgrade: #909: start, readyz, doctor, release, then the report's doctor — OK" ;;
  *) echo "FAIL: #909: the order: $order"; fail=1 ;;
esac
ever "#909: the marker is written as the daemon's user, never by root" "^runuser -u vornik"
[ ! -e "$marker" ] || { echo "FAIL: #909: the marker outlived the release"; fail=1; }
if grep -Eq '^\{"hold_id":"1\.0-1-2\.0-1-[0-9]+","created_at":"[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9:]{8}Z","reason":"managed-upgrade 1\.0-1->2\.0-1","deadline_seconds":1800\}$' "$s/marker_seen" 2>/dev/null &&
   [ "$(cat "$s/marker_mode" 2>/dev/null)" = 600 ]; then
  echo "managed-upgrade: #909: the start found the marker in §13.1's format, mode 0600 — OK"
else
  echo "FAIL: #909: the marker at start: $(cat "$s/marker_seen" 2>/dev/null || echo none), mode $(cat "$s/marker_mode" 2>/dev/null)"; fail=1
fi

verify_case() { # name readyz-json-or-empty extra-touch
  reset; [ -n "$2" ] && printf '%s' "$2" > "$s/readyz"; [ -n "${3:-}" ] && touch "$s/$3"
  rc=$(up)
  check "#909: $1 rolls back at verify" 1 '"result":"rolled_back","step":"verify"' "$rc"
  never "#909: $1 never releases" "^vornikctl daemon release"
  [ ! -e "$marker" ] && [ "$(cat "$s/installed")" = 1.0-1 ] && [ -f "$s/active" ] ||
    { echo "FAIL: #909: after $1: marker $(ls "$marker" 2>/dev/null), installed $(cat "$s/installed")"; fail=1; }
}
ok4='{"name":"database","status":"ok"},{"name":"scheduler_heartbeat","status":"ok"},{"name":"chat_provider","status":"ok"},{"name":"retention_sweeper","status":"ok"}'
verify_case "an overdue hold" '{"checks":['"$ok4"'],"overdue":true,"status":"held"}'
verify_case "a failing check" '{"checks":[{"name":"database","status":"error"},{"name":"chat_provider","status":"ok"}],"overdue":false,"status":"held"}'
verify_case "an unknown check" '{"checks":['"$ok4"',{"name":"brand_new","status":"ok"}],"overdue":false,"status":"held"}'
verify_case "no chat_provider" '{"checks":[{"name":"database","status":"ok"}],"overdue":false,"status":"held"}'
verify_case "an unheld /readyz" '{"checks":['"$ok4"'],"status":"ready"}'
verify_case "a doctor ERROR" '' doctor_held_error

reset; printf '{\n  "checks": [\n    {"name": "database", "status": "ok"},\n    {"name": "chat_provider", "status": "ok"}\n  ],\n  "overdue": false,\n  "status": "held"\n}\n' > "$s/readyz"; rc=$(up)
check "#909: an indented held /readyz verifies (review 37da #2)" 0 '"result":"upgraded"' "$rc"
cp "$work/bin/curl" "$work/curl.orig"; cp "$work/bin/runuser" "$work/runuser.orig"
reset; printf '#!/usr/bin/env bash\necho "curl $*" >> "%s/calls"\nexit 7\n' "$s" > "$work/bin/curl"; rc=$(up)
check "#909: a /readyz that never answers rolls back at verify" 1 '"result":"rolled_back","step":"verify"' "$rc"
check "#909: and says so" 1 "/readyz did not answer" "$rc"
never "#909: an unanswered /readyz is never released" "^vornikctl daemon release"
cp "$work/curl.orig" "$work/bin/curl"
reset; printf '#!/usr/bin/env bash\necho "runuser $*" >> "%s/calls"\nexit 1\n' "$s" > "$work/bin/runuser"; rc=$(up)
check "#909: a marker that cannot be written rolls back at start" 1 '"result":"rolled_back","step":"start"' "$rc"
[ "$(cat "$s/installed")" = 1.0-1 ] && [ -f "$s/active" ] && [ ! -e "$marker" ] ||
  { echo "FAIL: #909: after an unwritable marker: installed $(cat "$s/installed")"; fail=1; }
cp "$work/runuser.orig" "$work/bin/runuser"
reset; echo keep > "$s/release"; rc=$(up)
check "#909: a refused release with the marker kept rolls back" 1 '"result":"rolled_back","step":"release"' "$rc"
[ ! -e "$marker" ] && [ "$(cat "$s/installed")" = 1.0-1 ] || { echo "FAIL: #909: after a refused release"; fail=1; }
reset; echo removed > "$s/release"; rc=$(up)
check "#909: a lost answer with the marker gone and the unit active is final" 0 '"result":"upgraded"' "$rc"
never "#909: a released upgrade restores nothing" "^vornikctl restore"
reset; echo dead > "$s/release"; rc=$(up)
check "#909: the marker gone and the unit dead rolls back" 1 '"result":"rolled_back","step":"release"' "$rc"
reset; echo dangling > "$s/release"; rc=$(up)
check "#909: a dangling link in place of the marker is not a release (review 37da #1)" 1 '"result":"rolled_back","step":"release"' "$rc"
reset; echo late > "$s/release"; rc=$(up)
check "#909: a release that lands during the settle wait is final" 0 '"result":"upgraded"' "$rc"
never "#909: a late release is not rolled back" "^vornikctl restore"

reset; echo '{"hold_id":"other"}' > "$marker"; rc=$(up)
check "#909: a marker from an earlier run fails the check" 1 '"step":"check"' "$rc"
never "#909: and changes nothing" "^gate \|^systemctl stop\|^rpm -U --define"
[ -e "$marker" ] || { echo "FAIL: #909: the earlier marker was removed"; fail=1; }

reset; touch "$s/held"; printf '{"phase":"installed","from":"1.0-1","to":"2.0-1","snapshot":"%s"}\n' "$work/state/snap.tgz" > "$work/state/state.json"
echo x > "$marker"; echo -n 2.0-1 > "$s/installed"; echo snap > "$work/state/snap.tgz"
rc=$(run --rollback --from-package "$work/old.rpm")
check "#909: --rollback on a held unit rolls back" 0 '"result":"rolled_back","step":"rollback"' "$rc"
[ ! -e "$marker" ] || { echo "FAIL: #909: --rollback left the marker"; fail=1; }

reset; touch "$s/start_fail_2.0-1"; rc=$(up)
[ ! -e "$marker" ] || { echo "FAIL: #909: a failed start left the marker"; fail=1; }

[ "$fail" = 0 ] && echo "managed-upgrade_test.sh: PASS"
exit "$fail"
