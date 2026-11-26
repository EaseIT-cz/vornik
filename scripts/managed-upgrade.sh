#!/usr/bin/env bash
# managed-upgrade.sh — upgrade a managed Vornik instance in place, as root.
#
# Installed as /usr/libexec/vornik-enterprise/managed-upgrade and run by the
# control plane's Run Command (GitHub #581 D2). The daemon never runs it: the
# daemon spawns no process (internal/spawn law).
# see LLD § https://docs.vornik.io, amendment 2026-10-29 (#581) D2, amendment 2026-11-24 (#909) held start
#
# Usage:
#   managed-upgrade --package NEW.rpm --from-package CURRENT.rpm [--eula-hash SHA256] [--drain-ttl SECONDS]
#   managed-upgrade --rollback --from-package CURRENT.rpm
#
# The order: check (signatures, versions, no upgrade in progress, the EULA
# from the new package's own binary, then rpm -U --test: amendment
# 2026-11-14, #775) → the idle gate --drain, then stop →
# snapshot the stopped instance → install → start HELD → verify → release
# (amendment 2026-11-24, #909; deploy idle gate design §13.7). The new
# version starts with the start-held marker, so it serves its listeners but
# does no work until the release. A failed start or a held instance that
# fails verification rolls back by itself: the from-version reinstalled, the
# snapshot restored, started. The rollback boundary is the release. Once the
# release answers (or the marker says it happened), the upgrade is final:
# state.json is removed, and nothing after it restores over the new
# version's writes. A doctor error after that is reported, not rolled back.
#
# stdout is one JSON line, no secret: {"ok":true|false,"release":<to>,
# "result":"upgraded"|"rolled_back"|"failed",...}. "ok" and "release" are
# what vornik-cloud's vornik-upgrade document reads (its fleet design §3).
# Progress goes to stderr.
#
# Exit codes: 0 upgraded (or rolled back by --rollback); 1 failed or rolled
# back by itself; 64 bad usage.
#
# Every external command and path is overridable from the environment, for
# managed-upgrade_test.sh.
set -u

: "${MU_STATE_DIR:=/var/lib/vornik/upgrade}"
: "${MU_MARKER:=/etc/vornik/managed-image}"
: "${MU_CONFIG:=/etc/vornik/vornik.yaml}"
: "${MU_GATE:=/usr/libexec/vornik-enterprise/deploy-idle-gate}"
: "${MU_UNIT:=vornik-enterprise}"
: "${MU_PACKAGE_NAME:=vornik-enterprise}"
: "${MU_DAEMON:=/usr/bin/vornik-enterprise}"
: "${MU_DAEMON_IN_PACKAGE:=./usr/bin/vornik-enterprise}"
: "${MU_RPM:=rpm}"
: "${MU_RPM2CPIO:=rpm2cpio}"
: "${MU_CPIO:=cpio}"
: "${MU_SYSTEMCTL:=systemctl}"
: "${MU_VORNIKCTL:=vornikctl}"
: "${MU_CURL:=curl}"
: "${MU_RUNUSER:=runuser}"
# held_marker: the marker entry exists, a dangling link included.
held_marker() { [ -e "$MU_HOLD_MARKER" ] || [ -L "$MU_HOLD_MARKER" ]; }
# The held start (#909): the marker where the unit's VORNIK_DATA_DIR puts it,
# the daemon's user, and how long to wait for /readyz and for the unit to
# settle after a release that did not answer 0.
: "${MU_HOLD_MARKER:=/var/lib/vornik/start-held}"
: "${MU_DAEMON_USER:=vornik}"
: "${MU_VERIFY_SECONDS:=60}"
: "${MU_SETTLE_SECONDS:=300}"
state="$MU_STATE_DIR/state.json"

package='' from_package='' eula_hash='' rollback=0 drain_ttl=300
usage() {
  echo "managed-upgrade: $1" >&2
  echo "usage: managed-upgrade --package NEW.rpm --from-package CURRENT.rpm [--eula-hash SHA256] [--drain-ttl SECONDS]" >&2
  echo "       managed-upgrade --rollback --from-package CURRENT.rpm" >&2
  exit 64
}
while [ $# -gt 0 ]; do
  case "$1" in
    --package|--from-package|--eula-hash|--drain-ttl)
      [ $# -ge 2 ] && [ -n "$2" ] || usage "$1 needs a value"
      case "$1" in
        --package) package=$2 ;;
        --from-package) from_package=$2 ;;
        --drain-ttl) drain_ttl=$2 ;;
        *) eula_hash=$2 ;;
      esac
      shift 2 ;;
    --rollback) rollback=1; shift ;;
    -h|--help) sed -n '2,30p' "$0"; exit 0 ;;
    *) usage "unknown argument: $1" ;;
  esac
done
[ -n "$from_package" ] || usage "--from-package is required"
case "$drain_ttl" in *[!0-9]*) usage "--drain-ttl takes whole seconds, 1 to 3600" ;; esac
[ "$drain_ttl" -ge 1 ] && [ "$drain_ttl" -le 3600 ] || usage "--drain-ttl takes whole seconds, 1 to 3600"
if [ "$rollback" = 1 ]; then
  [ -z "$package" ] && [ -z "$eula_hash" ] || usage "--rollback takes --from-package only"
else
  [ -n "$package" ] || usage "--package is required"
fi

log() { echo "managed-upgrade: $*" >&2; }

# Every step works on private copies of the packages, never on the paths the
# caller passed: a path could be swapped between the signature check and the
# install (commit security review of 2b7a928c3).
priv=$(mktemp -d) || { echo "managed-upgrade: mktemp failed" >&2; exit 1; }
chmod 0700 "$priv"
trap 'rm -rf "$priv"' EXIT
private_copy() { # path subdir -> prints the copy's path
  mkdir -p "$priv/$2" && cp -- "$1" "$priv/$2/$(basename -- "$1")" &&
    chmod 0600 "$priv/$2/$(basename -- "$1")" && printf '%s\n' "$priv/$2/$(basename -- "$1")"
}
# json_str quotes a value for the JSON line.
json_str() { printf '"%s"' "$(printf '%s' "$1" | sed 's/\\/\\\\/g; s/"/\\"/g' | tr -d '\n\r')"; }
# finish prints the result line and exits.
finish() { # code result step reason
  local ok=false
  [ "$1" = 0 ] && ok=true
  printf '{"ok":%s,"release":%s,"result":%s,"step":%s,"reason":%s,"from":%s,"to":%s}\n' \
    "$ok" "$(json_str "${to:-}")" "$(json_str "$2")" "$(json_str "$3")" "$(json_str "$4")" "$(json_str "${from:-}")" "$(json_str "${to:-}")"
  exit "$1"
}
fail() { finish 1 failed "$1" "$2"; }
# one_line holds text for the reason: one line, printable ASCII, at most 512
# characters, because vornik-cloud's parser (internal/provision/upgrade.go)
# refuses a control character (GitHub #775).
one_line() { printf '%s' "$1" | tr '\t\n\r' '   ' | LC_ALL=C tr -cd ' -~' | tr -s ' ' | cut -c1-512; }
# last_err passes a command's captured stderr on to ours and prints its last
# non-blank line, held to one_line (GitHub #865: a failure with no cause).
last_err() { # file
  [ -s "$1" ] || return 0
  cat "$1" >&2
  one_line "$(grep -v '^[[:space:]]*$' "$1" | tail -n 1)"
}

# write_state records the phase. "installed" is written BEFORE rpm -U, so a
# crash during the install is rolled back as an install.
write_state() { # phase
  local tmp="$state.tmp"
  printf '{"phase":%s,"from":%s,"to":%s,"snapshot":%s}\n' \
    "$(json_str "$1")" "$(json_str "$from")" "$(json_str "$to")" "$(json_str "$snapshot")" > "$tmp" &&
    mv "$tmp" "$state"
}
state_field() { # name
  sed -n "s/.*\"$1\":\"\\([^\"]*\\)\".*/\\1/p" "$state"
}

# signed_version prints a package's version-release after checking that its
# signature verifies against the image's keys; an unsigned package fails.
# The verdict is read after the path, exactly, so a file named like the OK
# line cannot pass.
signed_version() { # file
  local out
  out=$("$MU_RPM" -K "$1" 2>&1) || return 1
  [ "${out##*: }" = "digests signatures OK" ] || return 1
  "$MU_RPM" -qp --qf '%{VERSION}-%{RELEASE}' "$1" 2>/dev/null
}
installed_version() {
  "$MU_RPM" -q --qf '%{VERSION}-%{RELEASE}' "$MU_PACKAGE_NAME" 2>/dev/null
}
# vercmp prints 1, 0 or -1 for a newer, equal or older first version. The
# versions come from signed packages; they are still held to RPM's own
# version characters before they reach the Lua string (review fc53 #1).
vercmp() {
  case "$1$2" in *[!A-Za-z0-9._+~^-]*|'') echo invalid; return 0 ;; esac
  "$MU_RPM" --eval "%{lua: print(rpm.vercmp('$1', '$2'))}"
}
active() { "$MU_SYSTEMCTL" is-active --quiet "$MU_UNIT"; }

# ops_env reads the operations listener's URL and key once (#909: verify and
# release need them before the report does). Never printed.
url='' key=''
ops_env() {
  [ -n "$url" ] && return 0
  local creds
  creds=$("$MU_VORNIKCTL" daemon operations-credentials --config "$MU_CONFIG" 2>/dev/null) || return 1
  url=$(printf '%s\n' "$creds" | sed -n 's/^url=//p')
  key=$(printf '%s\n' "$creds" | sed -n 's/^key=//p')
  [ -n "$url" ] && [ -n "$key" ]
}

# write_hold writes the start-held marker (deploy idle gate design §13.1)
# into the daemon's own directory AS THE DAEMON'S USER: root performs no
# operation on a path in a directory that user controls, so a link planted
# there can only reach what that user may already write (a root chown of a
# swapped-in link would hand it any file on the host). The user writes a
# .tmp with noclobber and renames it, so the daemon never reads a
# half-written marker.
write_hold() {
  local id body
  id=$(printf '%s-%s-%s' "$from" "$to" "$(date -u +%s)" | tr '+~^' '...' | cut -c1-64)
  body=$(printf '{"hold_id":%s,"created_at":%s,"reason":%s,"deadline_seconds":1800}' \
    "$(json_str "$id")" "$(json_str "$(date -u +%Y-%m-%dT%H:%M:%SZ)")" "$(json_str "managed-upgrade $from->$to")")
  # shellcheck disable=SC2016 # the inner shell expands $1 and $2
  printf '%s\n' "$body" | "$MU_RUNUSER" -u "$MU_DAEMON_USER" -- sh -c \
    'umask 077; rm -f "$1" && set -C && cat > "$1" && mv -f "$1" "$2"' write-hold "$MU_HOLD_MARKER.tmp" "$MU_HOLD_MARKER"
}

# verify_held answers 0 when the held instance is what §13.7 step 2 calls
# verified; otherwise it sets verify_err.
verify_held() {
  local ready='' end names n report bad
  ops_env || { verify_err="no operations credentials"; return 1; }
  # A deadline, not a count: each curl may take its own 5 s.
  end=$((SECONDS + MU_VERIFY_SECONDS))
  while :; do
    ready=$("$MU_CURL" -s -m 5 "$url/readyz" 2>/dev/null) && [ -n "$ready" ] && break
    [ "$SECONDS" -lt "$end" ] || { verify_err="/readyz did not answer in ${MU_VERIFY_SECONDS}s"; return 1; }
    sleep 1
  done
  # Whitespace-tolerant: the daemon answers minified today, but the
  # verification must not depend on it.
  ready=$(printf '%s' "$ready" | tr -d '\n' | sed 's/": */":/g')
  case "$ready" in *'"status":"held"'*) ;; *) verify_err="/readyz is not held: $(one_line "$ready")"; return 1 ;; esac
  case "$ready" in *'"overdue":false'*) ;; *) verify_err="the hold is overdue"; return 1 ;; esac
  case "$ready" in *'"status":"error"'*) verify_err="a readiness check failed: $(one_line "$ready")"; return 1 ;; esac
  # The daemon's five checks (deploy idle gate design §13.3). A check the
  # daemon adds later fails here until this list ships with it: the two
  # change together, in one release.
  names=$(printf '%s' "$ready" | grep -o '"name":"[^"]*"' | sed 's/"name":"\(.*\)"/\1/')
  for n in $names; do
    case "$n" in database|scheduler_heartbeat|chat_provider|retention_sweeper|relay_upstream) ;;
      *) verify_err="unknown readiness check $n"; return 1 ;; esac
  done
  for n in database chat_provider; do
    printf '%s\n' "$names" | grep -qx "$n" || { verify_err="readiness check $n is missing"; return 1; }
  done
  report=$(VORNIK_API_URL="$url" VORNIK_API_KEY="$key" "$MU_VORNIKCTL" doctor --json 2>/dev/null)
  [ -n "$report" ] || { verify_err="doctor gave no report"; return 1; }
  # One segment per check, from its "name" on; any ERROR fails. start_held
  # is WARN while held and ERROR only when overdue, which /readyz refused.
  bad=$(printf '%s' "$report" | tr -d '\n' | sed 's/"name": *"/\n/g' | sed 1d |
    grep '"status": *"ERROR"' | sed 's/".*//' | tr '\n' ' ')
  [ -z "$bad" ] || { verify_err="doctor reports ERROR: $bad"; return 1; }
}

# restore_from_version puts the from-version back: package, then (unless
# the crash came before the install) the snapshot, then starts. It answers
# 0 when the unit is active again.
# A failure leaves the failing command's last stderr line in rollback_err,
# for the reason (GitHub #864: "the rollback failed" said nothing more).
rollback_err=''
restore_from_version() { # phase
  "$MU_SYSTEMCTL" stop "$MU_UNIT" >/dev/null 2>&1
  # A marker here is this upgrade's (step 1 refuses an earlier one): the
  # from-version starts unheld.
  rm -f "$MU_HOLD_MARKER"
  if [ "$1" != stopped ]; then
    if ! "$MU_RPM" -U --define '_pkgverify_level signature' --oldpackage --replacepkgs "$from_package" >&2 2>"$priv/rollback.err"; then
      rollback_err=$(last_err "$priv/rollback.err")
      log "reinstalling $from_package failed"; return 1
    fi
    last_err "$priv/rollback.err" >/dev/null
    if ! VORNIK_CONFIG="$MU_CONFIG" "$MU_VORNIKCTL" restore --from "$snapshot" --force --clean >&2 2>"$priv/rollback.err"; then
      rollback_err=$(last_err "$priv/rollback.err")
      log "restoring $snapshot failed"; return 1
    fi
    last_err "$priv/rollback.err" >/dev/null
  fi
  "$MU_SYSTEMCTL" start "$MU_UNIT" >&2 || return 1
  rm -f "$state"
}

from_package=$(private_copy "$from_package" from) || { echo "managed-upgrade: cannot copy --from-package" >&2; exit 1; }
if [ -n "$package" ]; then
  package=$(private_copy "$package" new) || { echo "managed-upgrade: cannot copy --package" >&2; exit 1; }
fi

if [ "$rollback" = 1 ]; then
  [ -f "$state" ] || { from=''; to=''; fail rollback "no upgrade is in progress (no $state): nothing to roll back; a finished upgrade is a restore"; }
  from=$(state_field from) to=$(state_field to) snapshot=$(state_field snapshot) phase=$(state_field phase)
  # A held unit did no work (#909): it may be rolled back. Held means our
  # marker is still there; an active unit without it may have served.
  if active && ! held_marker; then
    fail rollback "the unit is active: the new version may have served, so this is a restore, not a rollback"
  fi
  [ "$(signed_version "$from_package")" = "$from" ] || fail rollback "--from-package is not a signed $from package"
  restore_from_version "$phase" || fail rollback "the rollback did not complete${rollback_err:+ ($rollback_err)}; the instance is stopped"
  finish 0 rolled_back rollback "rolled back to $from"
fi

# 1. Checks, before anything changes.
from='' to='' snapshot=''
[ -f "$MU_MARKER" ] || fail check "not a managed image ($MU_MARKER is absent)"
to=$(signed_version "$package") || fail check "the package's signature does not verify"
from=$(installed_version) || fail check "$MU_PACKAGE_NAME is not installed"
[ "$(vercmp "$to" "$from")" = 1 ] || fail check "$to is not newer than the installed $from; a downgrade is --rollback or a restore"
[ "$(signed_version "$from_package")" = "$from" ] || fail check "--from-package is not a signed $from package"
if [ -f "$state" ]; then
  if active && [ "$(state_field to)" = "$from" ]; then
    log "removing a stale $state: the upgrade to $from finished, the run that made it did not remove it"
    rm -f "$state"
  else
    fail check "an upgrade is in progress ($state exists and the unit is not active at its to-version); run --rollback"
  fi
fi
if held_marker; then
  fail check "$MU_HOLD_MARKER exists: a held start from another run; it is left in place (release or remove it first)"
fi
work="$priv/extract"
mkdir -p "$work" || fail check "cannot create the extraction directory"
accept_eula=0
if ! ( cd "$work" && "$MU_RPM2CPIO" "$package" | "$MU_CPIO" -idm --quiet "$MU_DAEMON_IN_PACKAGE" ) 2>/dev/null; then
  fail check "could not extract the new binary from the package"
fi
newbin="$work/${MU_DAEMON_IN_PACKAGE#./}"
if ! "$newbin" eula check >/dev/null 2>&1; then
  hash=$("$newbin" --eula-version 2>/dev/null | sed -n 's/.*(sha256 \([0-9a-f]*\)).*/\1/p')
  [ -n "$hash" ] || fail eula "the new binary did not print its EULA hash"
  [ -n "$eula_hash" ] && [ "$eula_hash" = "$hash" ] ||
    fail eula "the new version's EULA (sha256 $hash) is not accepted; pass --eula-hash with that hash once the customer has accepted it"
  accept_eula=1
fi
# The transaction check, last: a missing requirement (or a conflict, or a
# held RPM lock) is found before anything stops, not at step 4 as a
# rollback (GitHub #775). LC_ALL=C pins RPM's messages. The full output goes
# to stderr; 512 printable-ASCII characters go into the reason, so the JSON
# line stays one line with no control character, which vornik-cloud's
# parser (internal/provision/upgrade.go) refuses.
if ! deps_out=$(LC_ALL=C "$MU_RPM" -U --test --define '_pkgverify_level signature' "$package" 2>&1); then
  printf '%s\n' "$deps_out" >&2
  fail deps "the transaction check refused the package: $(one_line "$deps_out")"
fi
[ -z "$deps_out" ] || printf '%s\n' "$deps_out" >&2

# 2. Wait for idle, drain, stop: nothing is cut, then nothing writes.
"$MU_GATE" --drain --drain-ttl "$drain_ttl" --config "$MU_CONFIG" >&2 || fail gate "the idle gate did not reach idle (see stderr)"
"$MU_SYSTEMCTL" stop "$MU_UNIT" >&2 || fail stop "systemctl stop failed; the drain expires by itself"

# 3. Snapshot the stopped instance; earlier snapshots are dropped.
install -d -m 0700 "$MU_STATE_DIR" || fail snapshot "cannot create $MU_STATE_DIR"
find "$MU_STATE_DIR" -maxdepth 1 -name 'pre-*.tgz' -delete
snapshot="$MU_STATE_DIR/pre-$from-$to.tgz"
if ! VORNIK_CONFIG="$MU_CONFIG" "$MU_VORNIKCTL" backup --out "$snapshot" >&2; then
  "$MU_SYSTEMCTL" start "$MU_UNIT" >&2
  fail snapshot "the snapshot failed; nothing was installed and the unit was started again"
fi
write_state stopped || fail snapshot "cannot write $state"

# 4. Install. The phase says installed before rpm -U runs.
write_state installed || fail install "cannot write $state"
install_ok=1 install_err=''
"$MU_RPM" -U --define '_pkgverify_level signature' "$package" >&2 || install_ok=0
# The acceptance's stdout is the EULA text, discarded; its stderr is passed on
# and its last line goes into the reason (GitHub #865).
if [ "$install_ok" = 1 ] && [ "$accept_eula" = 1 ]; then
  if ! "$MU_DAEMON" --accept-eula </dev/null >/dev/null 2>"$priv/accept.err"; then
    install_ok=0 install_err=" (EULA acceptance: $(last_err "$priv/accept.err"))"
  else
    last_err "$priv/accept.err" >/dev/null
  fi
fi
if [ "$install_ok" = 0 ]; then
  restore_from_version installed || fail install "the install failed and so did the rollback${rollback_err:+ ($rollback_err)}; the instance is stopped, $state kept for --rollback"
  finish 1 rolled_back install "the install failed$install_err; rolled back to $from"
fi

# 5. Start held: active means serving, not working (#909).
write_hold || {
  restore_from_version installed || fail start "the start-held marker could not be written and the rollback failed${rollback_err:+ ($rollback_err)}; the instance is stopped, $state kept for --rollback"
  finish 1 rolled_back start "the start-held marker could not be written; rolled back to $from"
}
if ! "$MU_SYSTEMCTL" start "$MU_UNIT" >&2; then
  restore_from_version installed || fail start "the new version did not start and the rollback failed${rollback_err:+ ($rollback_err)}; the instance is stopped, $state kept for --rollback"
  finish 1 rolled_back start "the new version did not start; rolled back to $from"
fi

# 6. Verify the held instance. It did no work, so a rollback erases nothing.
verify_err=''
if ! verify_held; then
  restore_from_version installed || fail verify "the held version failed verification ($verify_err) and the rollback failed${rollback_err:+ ($rollback_err)}; the instance is stopped, $state kept for --rollback"
  finish 1 rolled_back verify "the held version failed verification: $(one_line "$verify_err"); rolled back to $from"
fi

# 7. Release: final from here. Any answer but 0 is decided by the marker and
# the unit AFTER the settle wait, never by retrying (§13.7 step 4).
if ! VORNIK_API_URL="$url" VORNIK_API_KEY="$key" "$MU_VORNIKCTL" daemon release >&2; then
  end=$((SECONDS + MU_SETTLE_SECONDS))
  while held_marker && active && [ "$SECONDS" -lt "$end" ]; do sleep 1; done
  if held_marker || ! active; then
    restore_from_version installed || fail release "the release failed and so did the rollback${rollback_err:+ ($rollback_err)}; the instance is stopped, $state kept for --rollback"
    finish 1 rolled_back release "the release failed and nothing was released; rolled back to $from"
  fi
  log "the release did not answer, but the marker is gone and the unit is active: released"
fi
rm -f "$state"

# 8. Report. A doctor finding is the control plane's to act on.
doctor_errors=unknown
if ops_env; then
  if report=$(VORNIK_API_URL="$url" VORNIK_API_KEY="$key" "$MU_VORNIKCTL" doctor --json 2>/dev/null) || [ -n "$report" ]; then
    doctor_errors=$(printf '%s' "$report" | grep -o '"status": *"ERROR"' | wc -l | tr -d ' ')
  fi
fi
printf '{"ok":true,"release":%s,"result":"upgraded","from":%s,"to":%s,"doctor_errors":%s,"snapshot":%s}\n' \
  "$(json_str "$to")" "$(json_str "$from")" "$(json_str "$to")" "$(json_str "$doctor_errors")" "$(json_str "$snapshot")"
exit 0
