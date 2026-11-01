#!/usr/bin/env bash
# managed-upgrade.sh — upgrade a managed Vornik instance in place, as root.
#
# Installed as /usr/libexec/vornik-enterprise/managed-upgrade and run by the
# control plane's Run Command (GitHub #581 D2). The daemon never runs it: the
# daemon spawns no process (internal/spawn law).
# see LLD § https://docs.vornik.io, amendment 2026-10-29 (#581) D2
#
# Usage:
#   managed-upgrade --package NEW.rpm --from-package CURRENT.rpm [--eula-hash SHA256] [--drain-ttl SECONDS]
#   managed-upgrade --rollback --from-package CURRENT.rpm
#
# The order: check (signatures, versions, no upgrade in progress, the EULA
# from the new package's own binary) → the idle gate --drain, then stop →
# snapshot the stopped instance → install → start. The rollback boundary is
# the unit becoming active (Type=notify: READY=1 after the listeners serve).
# A failed start rolls back by itself: the from-version reinstalled, the
# snapshot restored, started. Once the unit is active the upgrade is final:
# state.json is removed at once, and nothing after it restores over writes
# the new version accepted. A doctor error after that is reported, not
# rolled back.
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

# restore_from_version puts the from-version back: package, then (unless
# the crash came before the install) the snapshot, then starts. It answers
# 0 when the unit is active again.
restore_from_version() { # phase
  "$MU_SYSTEMCTL" stop "$MU_UNIT" >/dev/null 2>&1
  if [ "$1" != stopped ]; then
    if ! "$MU_RPM" -U --define '_pkgverify_level signature' --oldpackage --replacepkgs "$from_package" >&2; then
      log "reinstalling $from_package failed"; return 1
    fi
    if ! VORNIK_CONFIG="$MU_CONFIG" "$MU_VORNIKCTL" restore --from "$snapshot" --force --clean >&2; then
      log "restoring $snapshot failed"; return 1
    fi
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
  if active; then
    fail rollback "the unit is active: the new version may have served, so this is a restore, not a rollback"
  fi
  [ "$(signed_version "$from_package")" = "$from" ] || fail rollback "--from-package is not a signed $from package"
  restore_from_version "$phase" || fail rollback "the rollback did not complete; the instance is stopped"
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
install_ok=1
"$MU_RPM" -U --define '_pkgverify_level signature' "$package" >&2 || install_ok=0
if [ "$install_ok" = 1 ] && [ "$accept_eula" = 1 ]; then
  "$MU_DAEMON" --accept-eula </dev/null >/dev/null 2>&1 || install_ok=0
fi
if [ "$install_ok" = 0 ]; then
  restore_from_version installed || fail install "the install failed and so did the rollback; the instance is stopped, $state kept for --rollback"
  finish 1 rolled_back install "the install failed; rolled back to $from"
fi

# 5. Start. Active means served: from here the upgrade is final.
if ! "$MU_SYSTEMCTL" start "$MU_UNIT" >&2; then
  restore_from_version installed || fail start "the new version did not start and the rollback failed; the instance is stopped, $state kept for --rollback"
  finish 1 rolled_back start "the new version did not start; rolled back to $from"
fi
rm -f "$state"

# 6. Report. A doctor finding is the control plane's to act on.
doctor_errors=unknown
if creds=$("$MU_VORNIKCTL" daemon operations-credentials --config "$MU_CONFIG" 2>/dev/null); then
  url=$(printf '%s\n' "$creds" | sed -n 's/^url=//p')
  key=$(printf '%s\n' "$creds" | sed -n 's/^key=//p')
  if report=$(VORNIK_API_URL="$url" VORNIK_API_KEY="$key" "$MU_VORNIKCTL" doctor --json 2>/dev/null) || [ -n "$report" ]; then
    doctor_errors=$(printf '%s' "$report" | grep -o '"status": *"ERROR"' | wc -l | tr -d ' ')
  fi
fi
printf '{"ok":true,"release":%s,"result":"upgraded","from":%s,"to":%s,"doctor_errors":%s,"snapshot":%s}\n' \
  "$(json_str "$to")" "$(json_str "$from")" "$(json_str "$to")" "$(json_str "$doctor_errors")" "$(json_str "$snapshot")"
exit 0
