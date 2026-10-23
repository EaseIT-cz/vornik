#!/bin/sh
# Unit tests for the detection/selection helpers in quickstart.sh.
# Stubs command -v / package managers / podman so the helpers' decision
# trees run without touching the host. Mirrors postinstall_detect_test.sh:
# source only the functions (guarded main) and assert on their choices.
#
# PATH is isolated to a stub bin dir so the host's own brew/dnf/rpm-ostree
# can't leak into command -v. The one host-dependent case (is_immutable's
# "mutable" branch) is guarded: it can't be exercised on a host that
# already has /run/ostree-booted, so we skip it there — CI's ubuntu-latest
# runner has no ostree marker and exercises it.
#
# Run: sh deployments/podman/quickstart_test.sh
set -u

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

# Source only the helpers from quickstart.sh (guarded main). The guard
# returns before any uname/id/sudo/podman/git/build side effect.
VORNIK_QUICKSTART_SOURCED=1 . "$SCRIPT_DIR/quickstart.sh"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
STUB="$TMP/bin"
mkdir -p "$STUB"

# make_stub <name> <body> — write a fake executable to $STUB/<name>.
make_stub() {
  printf '%s\n' "$2" > "$STUB/$1"
  chmod +x "$STUB/$1"
}

# isolate PATH to the stub bin only, so command -v sees only our fakes.
# Subshell keeps the PATH change local to the call.
isolated() { ( PATH="$STUB"; "$@" ); }

pass=0; fail=0
ok()  { pass=$((pass+1)); echo "  ok: $*"; }
bad() { fail=$((fail+1)); echo "  FAIL: $*"; }

# --- is_immutable --------------------------------------------------------
echo "--- is_immutable ---"

# rpm-ostree on PATH ⇒ immutable (returns 0) regardless of host marker.
make_stub rpm-ostree 'exit 0'
if isolated is_immutable; then ok "rpm-ostree on PATH ⇒ immutable"; else bad "rpm-ostree on PATH not detected"; fi
rm -f "$STUB/rpm-ostree"

# No rpm-ostree and no /run/ostree-booted ⇒ mutable (returns 1). Skip on
# hosts that already boot via ostree — we can't fake the marker away.
if [ -f /run/ostree-booted ]; then
  ok "host is ostree-booted; negative is_immutable branch skipped (covered on CI ubuntu-latest)"
elif isolated is_immutable; then
  bad "mutable host misdetected as immutable"
else
  ok "no ostree marker ⇒ mutable"
fi

# --- install_sys package-manager selection -------------------------------
# install_sys picks the FIRST available manager in the brew > dnf >
# apt-get > zypper > pacman order. Stub each candidate to record its args.
echo "--- install_sys selection ---"
make_stub sudo '#!/bin/sh
"$@"
'
make_stub brew     '#!/bin/sh
echo "brew $*"
'
make_stub dnf      '#!/bin/sh
echo "dnf $*"
'
make_stub apt-get  '#!/bin/sh
echo "apt-get $*"
'
make_stub zypper   '#!/bin/sh
echo "zypper $*"
'
make_stub pacman   '#!/bin/sh
echo "pacman $*"
'

# brew wins over dnf when both present.
OUT=$(isolated install_sys pkg-a pkg-b)
case "$OUT" in *"brew install pkg-a pkg-b"*) ok "brew preferred over dnf";; *) bad "brew not preferred: $OUT";; esac
rm -f "$STUB/brew"

# dnf wins when brew absent.
OUT=$(isolated install_sys pkg-a)
case "$OUT" in *"dnf install -y pkg-a"*) ok "dnf chosen when brew absent";; *) bad "dnf not chosen: $OUT";; esac
rm -f "$STUB/dnf"

# apt-get wins when brew+dnf absent.
OUT=$(isolated install_sys pkg-a)
case "$OUT" in *"apt-get install -y pkg-a"*) ok "apt-get chosen when brew+dnf absent";; *) bad "apt-get not chosen: $OUT";; esac
rm -f "$STUB/apt-get" "$STUB/zypper" "$STUB/pacman"

# No manager ⇒ install_sys returns 1 (caller falls back / prints guidance).
if isolated install_sys pkg-a >/dev/null 2>&1; then bad "install_sys should fail with no manager"; else ok "install_sys returns 1 with no manager"; fi

# --- ensure_compose ------------------------------------------------------
echo "--- ensure_compose ---"

# have_compose true ⇒ ensure_compose returns 0 immediately. Stub `podman`
# so `podman compose version` succeeds.
make_stub podman '#!/bin/sh
[ "$1" = compose ] && [ "$2" = version ] && exit 0
exit 1
'
if isolated ensure_compose >/dev/null 2>&1; then ok "have_compose ⇒ ensure_compose short-circuits"; else bad "ensure_compose should short-circuit when compose present"; fi
rm -f "$STUB/podman"

# No podman/pipx/python3 ⇒ ensure_compose falls back to install_sys, then
# re-checks have_compose (still false) ⇒ returns 1. The real install_sys
# output is suppressed inside ensure_compose, so override install_sys to
# record the fallback call to a marker file we can grep.
: > "$TMP/fallback.called"
install_sys() { echo "install_sys $*" >> "$TMP/fallback.called"; }
isolated ensure_compose >/dev/null 2>&1 || true
if grep -q "install_sys podman-compose" "$TMP/fallback.called"; then
  ok "ensure_compose fell back to install_sys (then reported no compose)"
else
  bad "ensure_compose did not fall back to install_sys: $(cat "$TMP/fallback.called" 2>/dev/null)"
fi

# --- anonymous telemetry first-install choice ----------------------------
echo "--- anonymous telemetry first-install choice ---"
TELEMETRY_CFG="$TMP/telemetry-config.yaml"
printf 'server:\n  address: ":8080"\n' >"$TELEMETRY_CFG"
VORNIK_TELEMETRY=off configure_anonymous_telemetry "$TELEMETRY_CFG" 0 >/dev/null
if grep -q '^telemetry:' "$TELEMETRY_CFG" && grep -q 'enabled: false' "$TELEMETRY_CFG"; then
  ok "environment opt-out is persisted in config.yaml"
else
  bad "environment opt-out was not persisted"
fi

TELEMETRY_CFG_ON="$TMP/telemetry-config-on.yaml"
printf 'server:\n  address: ":8080"\n' >"$TELEMETRY_CFG_ON"
VORNIK_TELEMETRY=on configure_anonymous_telemetry "$TELEMETRY_CFG_ON" 0 >/dev/null
if grep -q '^telemetry:' "$TELEMETRY_CFG_ON"; then
  bad "enabled default should not be shipped into config.yaml"
else
  ok "enabled choice leaves telemetry absent from config.yaml"
fi

TELEMETRY_CFG_BAD="$TMP/telemetry-config-bad.yaml"
printf 'server:\n  address: ":8080"\n' >"$TELEMETRY_CFG_BAD"
VORNIK_TELEMETRY=invalid configure_anonymous_telemetry "$TELEMETRY_CFG_BAD" 0 >/dev/null 2>&1
if grep -q 'enabled: false' "$TELEMETRY_CFG_BAD"; then
  ok "invalid environment value fails closed"
else
  bad "invalid environment value did not fail closed"
fi

# --- require_safe_checkout_dir ------------------------------------------
echo "--- require_safe_checkout_dir ---"
if isolated require_safe_checkout_dir "$HOME/vornik"; then
  ok "dedicated checkout dir allowed"
else
  bad "safe checkout dir rejected"
fi

if isolated require_safe_checkout_dir "$TMP/vornik"; then
  ok "dedicated temp checkout dir allowed"
else
  bad "safe temp checkout dir rejected"
fi

for unsafe in "" "/" "/tmp" "." ".." "$HOME" "$HOME/" "$HOME/." "$HOME/.." "$TMP/.." "$TMP/../vornik" "$TMP/./vornik"; do
  if isolated require_safe_checkout_dir "$unsafe" >/dev/null 2>&1; then
    bad "unsafe checkout dir accepted: '$unsafe'"
  else
    ok "unsafe checkout dir rejected: '$unsafe'"
  fi
done

# --- final output advertises the setup guide -----------------------------
# Onboarding contract (setup-guide rollout slice 2, restored 2026-07-01):
# the closing "Connect / Run tasks" block must lead users to the first-run
# setup guide, not only to hand-editing vornik.env.
echo "--- final output mentions /ui/setup ---"
if grep -q '/ui/setup' "$SCRIPT_DIR/quickstart.sh"; then
  ok "quickstart output points at the /ui/setup guide"
else
  bad "quickstart.sh never mentions /ui/setup — the guided onboarding path is undiscoverable"
fi

echo "--- install-failure reporting: scrubber strips diverse secret formats ---"
SEEDED="Authorization: Bearer eyJhbGci.eyJzdWIi.SflKxwRJ token=sk-ABCDEFGHIJKLMNOPQR key AKIAIOSFODNN7EXAMPLE https://x?token=supersecretvalue password=hunter2hunter2 -----BEGIN PRIVATE KEY-----MIIabc-----END PRIVATE KEY----- QWxhZGRpbjpvcGVuc2VzYW1lMTIzNDU2Nzg5MA=="
SCRUBBED="$(printf '%s' "$SEEDED" | vornik_scrub)"
for leak in 'eyJhbGci.eyJzdWIi.SflKxwRJ' 'sk-ABCDEFGHIJKLMNOPQR' 'AKIAIOSFODNN7EXAMPLE' 'supersecretvalue' 'hunter2hunter2' 'MIIabc'; do
  case "$SCRUBBED" in
    *"$leak"*) bad "scrubber LEAKED '$leak': $SCRUBBED" ;;
    *) ok "scrubber stripped '$leak'" ;;
  esac
done

echo "--- scrubber strips identifiers (paths/email/IP), not just secrets ---"
# review-20260725-7530 #3: the installer scrubber must reach parity with the Go
# scrubber — home/tilde paths (whole tail, incl. project name), email, IPv4.
IDS="see /var/home/vadim/projects/vornik-marketing/logs and /Users/joe/secretproj/x and ~/work/acme-thing/y ping vadim@vornik.io at 192.0.2.10 or 8.8.4.4"
IDS_SCRUBBED="$(printf '%s' "$IDS" | vornik_scrub)"
for leak in 'vadim' 'vornik-marketing' 'secretproj' 'acme-thing' 'vadim@vornik.io' '192.0.2.10' '8.8.4.4' '/projects/'; do
  case "$IDS_SCRUBBED" in
    *"$leak"*) bad "scrubber LEAKED identifier '$leak': $IDS_SCRUBBED" ;;
    *) ok "scrubber stripped identifier '$leak'" ;;
  esac
done

echo "--- vornik_urlencode is POSIX and byte-exact ---"
# Regression: the encoder shipped as a C-style `for (( ))` bash loop, so
# sourcing quickstart.sh under dash died with "Bad for loop variable" and this
# whole file failed to parse (CI 2026-07-26). Reaching this line at all proves
# the parse; the assertions below pin the behaviour the rewrite must keep.
case "$(vornik_urlencode 'AZaz09._~-')" in
  'AZaz09._~-') ok "unreserved characters pass through unencoded" ;;
  *) bad "unreserved set mangled: $(vornik_urlencode 'AZaz09._~-')" ;;
esac
case "$(vornik_urlencode 'a b,c/d?e=f&g')" in
  'a%20b%2Cc%2Fd%3Fe%3Df%26g') ok "reserved characters percent-encoded" ;;
  *) bad "reserved encoding wrong: $(vornik_urlencode 'a b,c/d?e=f&g')" ;;
esac
# `awk -v` would expand the backslash escape and turn \n into a real newline.
case "$(vornik_urlencode 'C:\x\ny')" in
  'C%3A%5Cx%5Cny') ok "backslashes survive as literal bytes" ;;
  *) bad "backslash mangled: $(vornik_urlencode 'C:\x\ny')" ;;
esac
# RFC 3986 encodes multibyte UTF-8 per byte: č = 0xC4 0x8D.
case "$(vornik_urlencode 'č')" in
  '%C4%8D') ok "multibyte UTF-8 encoded per byte" ;;
  *) bad "multibyte encoding wrong: $(vornik_urlencode 'č')" ;;
esac

echo "--- vornik_replace_all replaces literally, not as a regex ---"
case "$(vornik_replace_all 'a.c and abc' 'a.c' 'X')" in
  'X and abc') ok "needle treated as a literal, not a BRE" ;;
  *) bad "literal replace wrong: $(vornik_replace_all 'a.c and abc' 'a.c' 'X')" ;;
esac
case "$(vornik_replace_all 'h1 h1 h1' 'h1' '<host>')" in
  '<host> <host> <host>') ok "all occurrences replaced" ;;
  *) bad "replace-all missed occurrences: $(vornik_replace_all 'h1 h1 h1' 'h1' '<host>')" ;;
esac
case "$(vornik_replace_all 'keep me' '' 'X')" in
  'keep me') ok "empty needle is a no-op (no infinite loop)" ;;
  *) bad "empty needle mangled input" ;;
esac

echo "--- report_install_failure strips this machine's hostname ---"
# The failing command may embed the hostname; the hook replaces it with <host>.
HN="$(uname -n 2>/dev/null || hostname 2>/dev/null || echo UNKNOWNHOST)"
HOUT="$(REF=2026.7.4 BASH_COMMAND="ssh build@$HN /var/home/vadim/x" report_install_failure 9 2>&1)"
case "$HOUT" in
  *"$HN"*) [ "$HN" = UNKNOWNHOST ] && ok "no hostname to strip (skipped)" || bad "hostname '$HN' leaked: $HOUT" ;;
  *) ok "hostname stripped from failing command" ;;
esac

echo "--- report_install_failure emits a prefilled EaseIT-cz/vornik issue URL ---"
FOUT="$(REF=2026.7.4 report_install_failure 7 2>&1)"
case "$FOUT" in *"github.com/EaseIT-cz/vornik/issues/new?"*) ok "failure hook prints the EaseIT-cz/vornik issue URL" ;; *) bad "no issue URL in failure output: $FOUT" ;; esac
case "$FOUT" in *"labels=bug%2Cinstall"*) ok "issue carries bug,install labels" ;; *) bad "labels not encoded: $FOUT" ;; esac
# REGRESSION 2026-08-03 (operator report): a CE customer's report named neither the
# edition nor the build. This installer only ever clones EaseIT-cz/vornik, so an install
# failure is a CE failure by construction — say so in the body and the title, the same
# way `vornikctl report` now does.
case "$FOUT" in *"community%20%28CE%29"*) ok "install report marks the CE edition" ;; *) bad "no edition marking in install report: $FOUT" ;; esac
case "$FOUT" in *"title=%5BCE%5D"*) ok "install report title carries the CE tag" ;; *) bad "no [CE] title tag: $FOUT" ;; esac
# success (exit 0) must be a no-op
SOUT="$(report_install_failure 0 2>&1)"
case "$SOUT" in *"issues/new"*) bad "failure hook fired on exit 0" ;; *) ok "no-op on exit 0" ;; esac

echo "--- ensure_linger ---"
# Task 11 (F6): loginctl enable-linger used to be fire-and-forget (`|| warn`)
# — on hosts where the unprivileged call is denied, linger silently stayed
# off and the daemon (a systemctl --user unit) dies on logout. ensure_linger
# must verify Linger=yes and escalate to sudo when the unprivileged call
# doesn't stick.
: > "$TMP/linger_calls"
CALLS="$TMP/linger_calls"; export CALLS

# Already-enabled host: no enable-linger call at all (idempotent no-op).
make_stub id '#!/bin/sh
echo testuser
'
make_stub loginctl '#!/bin/sh
case "$1" in
  show-user) echo "Linger=yes" ;;
  enable-linger) echo "enable-linger $*" >> "$CALLS" ;;
esac
'
isolated ensure_linger
if grep -q "enable-linger" "$CALLS"; then
  bad "ensure_linger called enable-linger when already Linger=yes"
else
  ok "ensure_linger short-circuits when linger is already enabled"
fi
rm -f "$STUB/loginctl"

# Unprivileged enable denied ⇒ escalate to sudo.
: > "$CALLS"
make_stub loginctl '#!/bin/sh
case "$1" in
  show-user) echo "Linger=no" ;;
  enable-linger) echo "enable-linger $*" >> "$CALLS"; exit 1 ;;
esac
'
make_stub sudo '#!/bin/sh
echo "sudo $*" >> "$CALLS"
"$@"
'
# Invoke it BARE (no `|| true` around the call) under `set -e`, matching the
# real call site (quickstart.sh runs the non-sourced script under
# `set -euo pipefail` and calls `ensure_linger` unguarded). Regression:
# ensure_linger's own unprivileged `loginctl enable-linger` line used to lack
# `|| true`, so under set -e it aborted the whole installer right here —
# before the sudo escalation or the warn ever ran — which is worse than the
# pre-Task-11 fire-and-forget behavior. `isolated ensure_linger || true`
# (the old form of this test) masked that bug by swallowing the abort.
( PATH="$STUB"; set -e; ensure_linger )
rc=$?
if [ "$rc" -eq 0 ]; then
  ok "ensure_linger tolerates set -e (unprivileged enable-linger failure doesn't abort the caller)"
else
  bad "ensure_linger aborted under set -e (exit $rc) instead of falling through to sudo escalation"
fi
if grep -q "sudo loginctl enable-linger" "$CALLS"; then
  ok "ensure_linger escalates to sudo when unprivileged enable is denied"
else
  bad "ensure_linger did not escalate to sudo: $(cat "$CALLS")"
fi
rm -f "$STUB/id" "$STUB/loginctl" "$STUB/sudo"

echo "--- enable_podman_restart ---"
# Task 12 (F7): deps.compose.yaml sets `restart: always`, but for rootless
# podman that only recovers crashes — NOT a host reboot. Boot autostart needs
# podman-restart.service enabled for the user. Guard on the unit existing so
# hosts without it warn instead of hard-failing.
: > "$TMP/restart_calls"
CALLS="$TMP/restart_calls"; export CALLS
make_stub systemctl '#!/bin/sh
echo "systemctl $*" >> "$CALLS"
case "$1 $2" in
  "--user list-unit-files") exit 0 ;;
  "--user enable") exit 0 ;;
esac
exit 0
'
isolated enable_podman_restart
if grep -q "systemctl --user enable podman-restart.service" "$CALLS"; then
  ok "enable_podman_restart enables podman-restart.service when the unit exists"
else
  bad "did not enable podman-restart.service: $(cat "$CALLS")"
fi
rm -f "$STUB/systemctl"

# Unit not present on this host (e.g. no podman-restart.service shipped) ⇒
# warn, no hard failure, and no enable call attempted.
: > "$CALLS"
make_stub systemctl '#!/bin/sh
echo "systemctl $*" >> "$CALLS"
case "$1 $2" in
  "--user list-unit-files") exit 1 ;;
esac
exit 0
'
if isolated enable_podman_restart >/tmp/enable_podman_restart.$$ 2>&1; then :; fi
if grep -q "systemctl --user enable podman-restart.service" "$CALLS"; then
  bad "enable_podman_restart called enable when list-unit-files reported no unit"
else
  ok "enable_podman_restart skips enable when the unit is absent"
fi
rm -f "$STUB/systemctl" "/tmp/enable_podman_restart.$$"

echo "--- print_success_footer names doctor + report ---"
# Task 8 (F5): the post-install success message must point at both
# `vornikctl doctor` (diagnostics) and `vornikctl report` (anonymized issue
# filing) so a failing first task doesn't strand the user.
FOOTER="$(print_success_footer 2>&1)"
case "$FOOTER" in *"vornikctl doctor"*) ok "footer names vornikctl doctor";; *) bad "footer missing 'vornikctl doctor': $FOOTER";; esac
case "$FOOTER" in *"vornikctl report"*) ok "footer names vornikctl report";; *) bad "footer missing 'vornikctl report': $FOOTER";; esac

echo "--- ensure_api_key (hardened-by-default seed, 2026-10-01) ---"
# The seeded config requires an API key (vornik.host.yaml auth_enabled: true);
# the external scan of 2026-10-01 found the old seed served the API to the LAN
# unauthenticated. The key lands in secrets/api.env, the one file both the
# daemon's loader and shell-env.sh source.
K="$TMP/kcfg"; mkdir -p "$K"
( ensure_api_key "$K" ) >/dev/null 2>&1
if [ -f "$K/secrets/api.env" ]; then ok "key file created"; else bad "secrets/api.env not created"; fi
mode="$(stat -c %a "$K/secrets/api.env" 2>/dev/null || stat -f %Lp "$K/secrets/api.env")"
[ "$mode" = 600 ] && ok "key file is 0600" || bad "key file mode $mode, want 600"
key1="$(sed -n 's/^VORNIK_API_KEY=//p' "$K/secrets/api.env")"
case "$key1" in *[!0-9a-f]*|"") bad "key is not hex: '$key1'";; *) [ "${#key1}" -eq 64 ] && ok "key is 64 hex chars" || bad "key length ${#key1}, want 64";; esac
( ensure_api_key "$K" ) >/dev/null 2>&1
key2="$(sed -n 's/^VORNIK_API_KEY=//p' "$K/secrets/api.env")"
[ "$key1" = "$key2" ] && ok "re-run preserves the key" || bad "re-run rotated the key"
K2="$TMP/kcfg2"; mkdir -p "$K2"; printf 'VORNIK_API_KEY=operator-chosen\n' > "$K2/vornik.env"
( ensure_api_key "$K2" ) >/dev/null 2>&1
[ ! -e "$K2/secrets/api.env" ] && ok "an operator-set key in vornik.env is respected" || bad "generated a second key despite vornik.env"
K3="$TMP/kcfg3"; mkdir -p "$K3"; printf 'VORNIK_API_KEY=\n' > "$K3/vornik.env"
( ensure_api_key "$K3" ) >/dev/null 2>&1
[ -f "$K3/secrets/api.env" ] && ok "an EMPTY key line does not count as set" || bad "empty VORNIK_API_KEY= treated as set"
OUT="$(ensure_api_key "$TMP/kcfg4" 2>&1)"
case "$OUT" in *"$(sed -n 's/^VORNIK_API_KEY=//p' "$TMP/kcfg4/secrets/api.env")"*) bad "ensure_api_key printed the key";; *) ok "key is never printed";; esac

echo "--- fresh_db_install ---"
# Randomise the DB password only when nothing could already hold the old one:
# Postgres fixes it when the volume is initialised.
make_stub podman 'echo podman_unrelated; exit 0'
if isolated fresh_db_install "$TMP/no-such.env"; then ok "no deps .env + no volume ⇒ fresh"; else bad "fresh install not detected"; fi
make_stub podman 'echo podman_vornik-postgres-data; exit 0'
if isolated fresh_db_install "$TMP/no-such.env"; then bad "existing postgres volume treated as fresh"; else ok "existing volume ⇒ keep password"; fi
make_stub podman 'exit 1'
if isolated fresh_db_install "$TMP/no-such.env"; then bad "podman failure treated as fresh"; else ok "podman unusable ⇒ keep password (fail safe)"; fi
make_stub podman 'exit 0'
: > "$TMP/deps.env"
if isolated fresh_db_install "$TMP/deps.env"; then bad "existing deps .env treated as fresh"; else ok "existing deps .env ⇒ keep password"; fi
rm -f "$STUB/podman"

echo "--- seed_db_password (#412: one value, never the shipped constant) ---"
# GitHub #412 (found at: production, 2026-10-16): the reference host ran on a
# shared, guessable database password. Every non-fresh quickstart branch used
# to keep vornik_dev_password silently, and the two files could disagree.
# Design: 2026-10-21-installation-credential-hygiene-design.md §2.1 D1.
EX="$TMP/env.example"; printf '# example\nPOSTGRES_DB=vornik\nPOSTGRES_USER=vornik\nPOSTGRES_PASSWORD=\n' > "$EX"
envval() { sed -n "s/^$2=//p" "$1" | tail -n 1; }
fmode() { stat -c %a "$1" 2>/dev/null || stat -f %Lp "$1"; }
# new_cfg <name> [vornik.env password] — a config dir with a freshly seeded vornik.env.
new_cfg() {
  mkdir -p "$TMP/$1"
  printf 'POSTGRES_DB=vornik\nVORNIK_DATABASE_PASSWORD=%s\nVORNIK_DATABASE_PORT=5432\n' "${2:-}" > "$TMP/$1/vornik.env"
  chmod 600 "$TMP/$1/vornik.env"
}
# with_podman <stub body> <cmd...> — run with a podman stub ahead of the host's.
with_podman() { _b="$1"; shift; make_stub podman "$_b"; ( PATH="$STUB:$PATH"; "$@" ); _rc=$?; rm -f "$STUB/podman"; return $_rc; }
NOVOL='echo other-volume; exit 0'
VOL='echo vornik-postgres-data; exit 0'

new_cfg s1; new_cfg s2
O1="$(with_podman "$NOVOL" seed_db_password "$TMP/s1" "$TMP/s1/deps.env" "$EX" 2>&1)"
with_podman "$NOVOL" seed_db_password "$TMP/s2" "$TMP/s2/deps.env" "$EX" >/dev/null 2>&1
p1="$(envval "$TMP/s1/vornik.env" VORNIK_DATABASE_PASSWORD)"; p2="$(envval "$TMP/s2/vornik.env" VORNIK_DATABASE_PASSWORD)"
case "$p1" in *[!0-9a-f]*|"") bad "fresh password is not hex: '$p1'";; *) [ "${#p1}" -eq 64 ] && ok "fresh install: 64-hex password" || bad "fresh password length ${#p1}";; esac
[ -n "$p1" ] && [ "$p1" != "$p2" ] && ok "two fresh installs get different passwords" || bad "two installs share a password"
[ "$p1" = "$(envval "$TMP/s1/deps.env" POSTGRES_PASSWORD)" ] && ok "vornik.env and deps .env hold one value" || bad "vornik.env and deps .env disagree"
[ "$(fmode "$TMP/s1/deps.env")" = 600 ] && ok "deps .env is 0600" || bad "deps .env mode $(fmode "$TMP/s1/deps.env")"
[ "$(fmode "$TMP/s1/vornik.env")" = 600 ] && ok "vornik.env stays 0600" || bad "vornik.env mode changed"
case "$O1" in *"$p1"*) bad "seed_db_password printed the password";; *) ok "password never printed";; esac
case "$p1" in vornik_dev_password|vornik|swarmd) bad "fresh install got a constant";; *) ok "fresh install is not a constant";; esac
grep -q '^POSTGRES_USER=vornik$' "$TMP/s1/deps.env" && ok "deps .env keeps the example's other keys" || bad "deps .env lost the example's keys"

# Re-run: nothing changes.
with_podman "$VOL" seed_db_password "$TMP/s1" "$TMP/s1/deps.env" "$EX" >/dev/null 2>&1
[ "$(envval "$TMP/s1/vornik.env" VORNIK_DATABASE_PASSWORD)" = "$p1" ] && [ "$(envval "$TMP/s1/deps.env" POSTGRES_PASSWORD)" = "$p1" ] \
  && ok "re-run keeps the pair" || bad "re-run changed the password"

# Kept volume + an existing deps .env: vornik.env adopts its value (an odd one survives byte-exact).
new_cfg s3
ODD='a|b&c\d/e$f'
printf 'POSTGRES_USER=vornik\nPOSTGRES_PASSWORD=%s\n' "$ODD" > "$TMP/s3/deps.env"
with_podman "$VOL" seed_db_password "$TMP/s3" "$TMP/s3/deps.env" "$EX" >/dev/null 2>&1
[ "$(envval "$TMP/s3/vornik.env" VORNIK_DATABASE_PASSWORD)" = "$ODD" ] && ok "vornik.env adopts the deps .env value byte-exact" || bad "adopted value differs: '$(envval "$TMP/s3/vornik.env" VORNIK_DATABASE_PASSWORD)'"
[ "$(fmode "$TMP/s3/deps.env")" = 600 ] && ok "an existing deps .env is tightened to 0600" || bad "existing deps .env mode $(fmode "$TMP/s3/deps.env")"

# An operator-set vornik.env value is never overwritten; the deps .env adopts it.
new_cfg s4 'operator-chosen-db-password-0001'
with_podman "$VOL" seed_db_password "$TMP/s4" "$TMP/s4/deps.env" "$EX" >/dev/null 2>&1
[ "$(envval "$TMP/s4/vornik.env" VORNIK_DATABASE_PASSWORD)" = operator-chosen-db-password-0001 ] && ok "operator value kept" || bad "operator value overwritten"
[ "$(envval "$TMP/s4/deps.env" POSTGRES_PASSWORD)" = operator-chosen-db-password-0001 ] && ok "deps .env adopts the vornik.env value" || bad "deps .env did not adopt"

# Both set and different: the daemon cannot log in, so the install STOPS
# (code review round 1, F2: a warning followed by a "successful" bring-up hid
# the lockout). Neither value is rewritten or printed.
new_cfg s5 'one-value-aaaaaaaaaaaaaaaaaaaa'
printf 'POSTGRES_PASSWORD=other-value-bbbbbbbbbbbbbbbbbbb\n' > "$TMP/s5/deps.env"
if O5="$(with_podman "$VOL" seed_db_password "$TMP/s5" "$TMP/s5/deps.env" "$EX" 2>&1)"; then
  bad "differing values did not stop the install"
else
  ok "differing values stop the install"
fi
[ "$(envval "$TMP/s5/vornik.env" VORNIK_DATABASE_PASSWORD)" = one-value-aaaaaaaaaaaaaaaaaaaa ] && [ "$(envval "$TMP/s5/deps.env" POSTGRES_PASSWORD)" = other-value-bbbbbbbbbbbbbbbbbbb ] \
  && ok "two different set values are both kept" || bad "a set value was rewritten"
case "$O5" in *differ*) ok "the stop says the values differ";; *) bad "no message for differing values: $O5";; esac
case "$O5" in *one-value-aaaa*|*other-value-bbbb*) bad "the message printed a password";; *) ok "the message names no value";; esac

# A stale deps .env still on the old shipped constant, over NO volume, with an
# empty vornik.env: nothing holds that value, so it is replaced by a generated
# one in both files (code review round 1, F1: the install used to adopt it).
new_cfg s8
printf 'POSTGRES_USER=vornik\nPOSTGRES_PASSWORD=vornik_dev_password\n' > "$TMP/s8/deps.env"
with_podman "$NOVOL" seed_db_password "$TMP/s8" "$TMP/s8/deps.env" "$EX" >/dev/null 2>&1
p8="$(envval "$TMP/s8/vornik.env" VORNIK_DATABASE_PASSWORD)"
[ "${#p8}" -eq 64 ] && [ "$p8" = "$(envval "$TMP/s8/deps.env" POSTGRES_PASSWORD)" ] \
  && ok "stale constant over no volume: regenerated in both files" || bad "stale constant adopted over no volume: '$p8'"
# The same stale constant over a KEPT volume is that install's real password:
# adopted (so it keeps working), with a warning naming the runbook.
new_cfg s9
printf 'POSTGRES_PASSWORD=vornik_dev_password\n' > "$TMP/s9/deps.env"
O9="$(with_podman "$VOL" seed_db_password "$TMP/s9" "$TMP/s9/deps.env" "$EX" 2>&1)"
[ "$(envval "$TMP/s9/vornik.env" VORNIK_DATABASE_PASSWORD)" = vornik_dev_password ] && ok "stale constant over a kept volume: adopted" || bad "kept-volume constant not adopted"
case "$O9" in *retrofit*) ok "adopting a known default warns with the runbook";; *) bad "adopting a known default is silent: $O9";; esac

# Code review round 2, G1 (found at: review; a user observes it at e2e-lane, a
# fresh quickstart): a known default held in vornik.env (alone, or equal in
# both files) over NO volume was kept, so a brand-new database was created on
# the shared constant; the both-equal case did not even warn. Nothing can hold
# a password when no volume exists, so the value is regenerated in both files.
new_cfg s10 vornik_dev_password
printf 'POSTGRES_PASSWORD=vornik_dev_password\n' > "$TMP/s10/deps.env"
with_podman "$NOVOL" seed_db_password "$TMP/s10" "$TMP/s10/deps.env" "$EX" >/dev/null 2>&1
p10="$(envval "$TMP/s10/vornik.env" VORNIK_DATABASE_PASSWORD)"
[ "${#p10}" -eq 64 ] && [ "$p10" = "$(envval "$TMP/s10/deps.env" POSTGRES_PASSWORD)" ] \
  && ok "known default in both files over no volume: regenerated in both" || bad "both-file known default kept over no volume: '$p10'"
new_cfg s11 vornik_dev_password
with_podman "$NOVOL" seed_db_password "$TMP/s11" "$TMP/s11/deps.env" "$EX" >/dev/null 2>&1
p11="$(envval "$TMP/s11/vornik.env" VORNIK_DATABASE_PASSWORD)"
[ "${#p11}" -eq 64 ] && [ "$p11" = "$(envval "$TMP/s11/deps.env" POSTGRES_PASSWORD)" ] \
  && ok "known default only in vornik.env over no volume: regenerated in both" || bad "vornik.env known default kept over no volume: '$p11'"
# Case-insensitive, like the doctor.
new_cfg s12 Postgres
with_podman "$NOVOL" seed_db_password "$TMP/s12" "$TMP/s12/deps.env" "$EX" >/dev/null 2>&1
p12="$(envval "$TMP/s12/vornik.env" VORNIK_DATABASE_PASSWORD)"
[ "${#p12}" -eq 64 ] && ok "known default matched case-insensitively" || bad "mixed-case known default kept: '$p12'"
# vornik.env names a database on another host: the local volume says nothing
# about that server's password, so the value is kept (the daemon must still log
# in) and the operator is warned to rotate it there.
new_cfg s13 vornik_dev_password
printf 'VORNIK_DATABASE_HOST=db.example.net\n' >> "$TMP/s13/vornik.env"
O13="$(with_podman "$NOVOL" seed_db_password "$TMP/s13" "$TMP/s13/deps.env" "$EX" 2>&1)"
[ "$(envval "$TMP/s13/vornik.env" VORNIK_DATABASE_PASSWORD)" = vornik_dev_password ] && ok "known default for an external database host: kept" || bad "external-host password replaced"
case "$O13" in *retrofit*) ok "keeping an external host's known default warns";; *) bad "external-host known default kept silently: $O13";; esac
# The same both-equal known default over a KEPT volume is kept (it opens that
# database), but no longer silently.
new_cfg s14 vornik_dev_password
printf 'POSTGRES_PASSWORD=vornik_dev_password\n' > "$TMP/s14/deps.env"
O14="$(with_podman "$VOL" seed_db_password "$TMP/s14" "$TMP/s14/deps.env" "$EX" 2>&1)"
[ "$(envval "$TMP/s14/vornik.env" VORNIK_DATABASE_PASSWORD)" = vornik_dev_password ] && [ "$(envval "$TMP/s14/deps.env" POSTGRES_PASSWORD)" = vornik_dev_password ] \
  && ok "both-equal known default over a kept volume: kept" || bad "both-equal known default over a kept volume was changed"
case "$O14" in *retrofit*) ok "both-equal known default over a kept volume warns";; *) bad "both-equal known default kept silently: $O14";; esac
# A local host name is still local: regenerated over no volume.
new_cfg s15 vornik_dev_password
printf 'VORNIK_DATABASE_HOST=127.0.0.1\n' >> "$TMP/s15/vornik.env"
with_podman "$NOVOL" seed_db_password "$TMP/s15" "$TMP/s15/deps.env" "$EX" >/dev/null 2>&1
[ "$(envval "$TMP/s15/vornik.env" VORNIK_DATABASE_PASSWORD | wc -c)" -eq 65 ] && ok "loopback host: known default regenerated" || bad "loopback host treated as external"

# config.yaml naming an external database host (vornik.env silent): kept.
new_cfg s16 vornik_dev_password
printf 'server:\n  port: 8080\ndatabase:\n  host: "db.example.net"\n  port: 5432\n' > "$TMP/s16/config.yaml"
with_podman "$NOVOL" seed_db_password "$TMP/s16" "$TMP/s16/deps.env" "$EX" >/dev/null 2>&1
[ "$(envval "$TMP/s16/vornik.env" VORNIK_DATABASE_PASSWORD)" = vornik_dev_password ] && ok "config.yaml external host: known default kept" || bad "config.yaml external host: password replaced"
# The quickstart's own seeded config (database.host 127.0.0.1) is local.
new_cfg s17 vornik_dev_password
cp "$SCRIPT_DIR/config/vornik.host.yaml" "$TMP/s17/config.yaml"
with_podman "$NOVOL" seed_db_password "$TMP/s17" "$TMP/s17/deps.env" "$EX" >/dev/null 2>&1
[ "$(envval "$TMP/s17/vornik.env" VORNIK_DATABASE_PASSWORD | wc -c)" -eq 65 ] && ok "seeded config.yaml host is local: known default regenerated" || bad "seeded config.yaml host read as external"

# A quoted known default (systemd's EnvironmentFile and compose both strip the
# quotes, so the password IS the constant) is still a known default.
new_cfg s18 '"vornik_dev_password"'
with_podman "$NOVOL" seed_db_password "$TMP/s18" "$TMP/s18/deps.env" "$EX" >/dev/null 2>&1
[ "$(envval "$TMP/s18/vornik.env" VORNIK_DATABASE_PASSWORD | wc -c)" -eq 65 ] && ok "quoted known default regenerated" || bad "quoted known default kept: $(envval "$TMP/s18/vornik.env" VORNIK_DATABASE_PASSWORD)"
known_default_db_password "'postgres'" && ok "single-quoted known default matched" || bad "single-quoted known default missed"

# Code review round 3, F1: a config.yaml whose database host this script cannot
# read must count as external (keep and warn), never as local. An absent
# database block or host key is the daemon's compiled default (localhost), so it
# stays local.
new_cfg s19 vornik_dev_password
printf 'database: {host: db.example.net, port: 5432}\n' > "$TMP/s19/config.yaml"
with_podman "$NOVOL" seed_db_password "$TMP/s19" "$TMP/s19/deps.env" "$EX" >/dev/null 2>&1
[ "$(envval "$TMP/s19/vornik.env" VORNIK_DATABASE_PASSWORD)" = vornik_dev_password ] && ok "flow-style database block: unreadable host kept" || bad "flow-style database block read as local"
new_cfg s20 vornik_dev_password
printf 'database:\n  host:\n  port: 5432\n' > "$TMP/s20/config.yaml"
with_podman "$NOVOL" seed_db_password "$TMP/s20" "$TMP/s20/deps.env" "$EX" >/dev/null 2>&1
[ "$(envval "$TMP/s20/vornik.env" VORNIK_DATABASE_PASSWORD)" = vornik_dev_password ] && ok "empty host value: kept" || bad "empty host value read as local"
new_cfg s21 vornik_dev_password
printf 'database:\n  host: db.example.net\n' > "$TMP/s21/config.yaml"; chmod 000 "$TMP/s21/config.yaml"
if [ -r "$TMP/s21/config.yaml" ]; then ok "unreadable config.yaml (skipped: running as root)"; else
with_podman "$NOVOL" seed_db_password "$TMP/s21" "$TMP/s21/deps.env" "$EX" >/dev/null 2>&1
[ "$(envval "$TMP/s21/vornik.env" VORNIK_DATABASE_PASSWORD)" = vornik_dev_password ] && ok "unreadable config.yaml: kept" || bad "unreadable config.yaml read as local"; fi
chmod 600 "$TMP/s21/config.yaml"
new_cfg s22 vornik_dev_password
printf 'server:\n  port: 8080\ndatabase:\n  name: vornik\n' > "$TMP/s22/config.yaml"
with_podman "$NOVOL" seed_db_password "$TMP/s22" "$TMP/s22/deps.env" "$EX" >/dev/null 2>&1
[ "$(envval "$TMP/s22/vornik.env" VORNIK_DATABASE_PASSWORD | wc -c)" -eq 65 ] && ok "no host key (daemon default localhost): regenerated" || bad "absent host key read as external"

new_cfg s23 vornik_dev_password
printf 'database:\n  host: ""\n' > "$TMP/s23/config.yaml"
with_podman "$NOVOL" seed_db_password "$TMP/s23" "$TMP/s23/deps.env" "$EX" >/dev/null 2>&1
[ "$(envval "$TMP/s23/vornik.env" VORNIK_DATABASE_PASSWORD)" = vornik_dev_password ] && ok "quoted empty host value: kept" || bad "quoted empty host value read as local"

# env_set refuses a value with a line break (code review round 1, F4).
printf 'A=1\n' > "$TMP/nl.env"
if env_set "$TMP/nl.env" B "$(printf 'x\ny')" 2>/dev/null; then bad "env_set wrote a multi-line value"; else ok "env_set refuses a multi-line value"; fi
[ "$(cat "$TMP/nl.env")" = "A=1" ] && ok "refused write left the file unchanged" || bad "refused write changed the file"

# Kept volume, no value anywhere: STOP, write nothing (design review round 1,
# a1: writing the old constant would lock a post-fix volume out while
# reporting success).
new_cfg s6
if O6="$(with_podman "$VOL" seed_db_password "$TMP/s6" "$TMP/s6/deps.env" "$EX" 2>&1)"; then
  bad "kept volume with no known value did not stop"
else
  ok "kept volume with no known value stops the install"
fi
[ -z "$(envval "$TMP/s6/vornik.env" VORNIK_DATABASE_PASSWORD)" ] && [ ! -e "$TMP/s6/deps.env" ] \
  && ok "nothing written when stopping" || bad "stopping still wrote a password"
case "$O6" in *VORNIK_DATABASE_PASSWORD*retrofit*|*retrofit*VORNIK_DATABASE_PASSWORD*) ok "the stop says what to set and names the runbook";; *) bad "stop message: $O6";; esac

# podman cannot list volumes: a volume may exist, so the same stop (fail safe).
new_cfg s7
if with_podman 'exit 1' seed_db_password "$TMP/s7" "$TMP/s7/deps.env" "$EX" >/dev/null 2>&1; then
  bad "podman failure did not stop"
else
  ok "podman failure stops rather than generate over a possible volume"
fi
[ -z "$(envval "$TMP/s7/vornik.env" VORNIK_DATABASE_PASSWORD)" ] && ok "podman failure wrote nothing" || bad "podman failure wrote a password"

# seed_db_password owns the deps .env: no later step of the main body may copy
# the example over it or sed a password into it (review round 1, a2).
if awk '/^seed_db_password\(\)/{f=1} f&&/^}/{f=0;next} !f' "$SCRIPT_DIR/quickstart.sh" | grep -Eq 'cp \.env\.example \.env|POSTGRES_PASSWORD=\$\{DB_PASSWORD'; then
  bad "the main body still writes the deps .env outside seed_db_password"
else
  ok "only seed_db_password writes the deps .env"
fi
grep -q '^ *seed_db_password "\$CONFIG_DIR"' "$SCRIPT_DIR/quickstart.sh" && ok "the main body calls seed_db_password" || bad "seed_db_password is never called"

# The shipped examples carry no password at all.
for f in "$SCRIPT_DIR/vornik.env.example" "$SCRIPT_DIR/.env.example"; do
  if grep -Eq '^(VORNIK_DATABASE_PASSWORD|POSTGRES_PASSWORD)=.+' "$f"; then bad "$(basename "$f") ships a password"; else ok "$(basename "$f") ships no password"; fi
done

echo "--- warn_auth_disabled ---"
printf 'api:\n  auth_enabled: false\n' > "$TMP/old.yaml"
W="$(warn_auth_disabled "$TMP/old.yaml" 2>&1)"
case "$W" in *auth_enabled*) ok "auth-off config warns";; *) bad "no warning for auth-off config: $W";; esac
printf 'api:\n  auth_enabled: true\n' > "$TMP/new.yaml"
W="$(warn_auth_disabled "$TMP/new.yaml" 2>&1)"
[ -z "$W" ] && ok "auth-on config is silent" || bad "warned on auth-on config: $W"

echo "--- shipped host seed is hardened ---"
SEED="$SCRIPT_DIR/config/vornik.host.yaml"
grep -Eq '^  auth_enabled: true' "$SEED" && ok "seed requires an API key" || bad "vornik.host.yaml does not set api.auth_enabled: true"
grep -Eq '^[[:space:]]+- "\$\{VORNIK_API_KEY\}"' "$SEED" && ok "seed takes the key from VORNIK_API_KEY" || bad "seed does not reference VORNIK_API_KEY"
grep -Eq '^  writes: "?insecure' "$SEED" && bad "seed enables insecure web writes" || ok "seed leaves web writes off"
grep -Eq 'insecure_http: true' "$SEED" && bad "seed enables plaintext A2A" || ok "seed has no plaintext A2A peer"

echo "--- seed_configs deploys what the release added, on every run ---"
# Design §15.5: before 2026-10-02 only a first run copied configs/, so a re-run
# never delivered a new subtree such as agent-templates. Run in subshells: the
# helper's own ok/warn would otherwise move this file's counters.
R="$TMP/sc-repo"; C="$TMP/sc-cfg"
mkdir -p "$R/configs/workflows" "$R/scripts" "$C/configs/workflows"
echo seed > "$R/configs/workflows/a.md"
cat > "$R/scripts/config-deploy.sh" <<'DEPLOY'
#!/bin/sh
printf '%s\n' "$1" >> "$DEPLOY_LOG"
DEPLOY
chmod +x "$R/scripts/config-deploy.sh"
echo tuned > "$C/configs/workflows/mine.md"
( DEPLOY_LOG="$TMP/sc.log" seed_configs "$R" "$C" ) >/dev/null 2>&1
[ "$(cat "$TMP/sc.log" 2>/dev/null)" = "$C" ] && ok "an existing install runs the config deploy" \
  || bad "an existing install did not run the config deploy (log: $(cat "$TMP/sc.log" 2>/dev/null))"
[ -f "$C/configs/workflows/a.md" ] && bad "an existing install was re-seeded wholesale" || ok "an existing install is not re-seeded"
rm -rf "$C" "$TMP/sc.log"; mkdir -p "$C"
( DEPLOY_LOG="$TMP/sc.log" seed_configs "$R" "$C" ) >/dev/null 2>&1
[ -f "$C/configs/workflows/a.md" ] && ok "a fresh install is seeded" || bad "a fresh install was not seeded"
[ "$(cat "$TMP/sc.log" 2>/dev/null)" = "$C" ] && ok "a fresh install also runs the config deploy" \
  || bad "a fresh install did not run the config deploy"

echo "--- the real config deploy delivers agent-templates ---"
# The outcome, not the call (review 20261002-8b42 F3): the stubs above prove
# seed_configs invokes the deploy; this proves the deploy it invokes puts the
# subtree agent administration needs on disk.
REAL="$TMP/real-cfg"; mkdir -p "$REAL"
if "$SCRIPT_DIR/../../scripts/config-deploy.sh" "$REAL" >/dev/null 2>&1 \
   && [ -n "$(ls -A "$REAL/configs/agent-templates" 2>/dev/null)" ]; then
  ok "config-deploy.sh lands configs/agent-templates"
else
  bad "config-deploy.sh did not land configs/agent-templates"
fi

echo "---"
echo "PASS: $pass passed, $fail failed"
[ "$fail" -eq 0 ] || exit 1
