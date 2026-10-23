#!/usr/bin/env bash
# dev-db-password.sh — the database password for the dev helpers
# (scripts/postgres-init.sh, scripts/infra.sh). Sourced, not run.
#
# GitHub #412: both helpers used to start Postgres on a constant password
# (vornik_dev_password, vornik) published on every interface — the shape of the
# reference host's incident. They now share this one resolver.
# see LLD § https://docs.vornik.io §2.1 D3
#
# resolve_dev_db_password <deps_env> prints the password on stdout (for capture
# into a variable; callers never echo it). In order:
#   1. POSTGRES_PASSWORD from the environment;
#   2. POSTGRES_PASSWORD in <deps_env> (deployments/podman/.env, the file the
#      deps compose and the quickstart use);
#   3. when podman lists no vornik-postgres-data volume, a new random value,
#      written to <deps_env> (created 0600, or the key appended; the file is
#      tightened to 0600);
#   4. otherwise it fails: an existing volume holds a password this script does
#      not know, and guessing a constant is what #412 removes.

resolve_dev_db_password() {
  _rd_env="$1"
  if [ -n "${POSTGRES_PASSWORD:-}" ]; then
    printf '%s\n' "$POSTGRES_PASSWORD"
    return 0
  fi
  if [ -r "$_rd_env" ]; then
    _rd_val="$(sed -n 's/^POSTGRES_PASSWORD=//p' "$_rd_env" | tail -n 1)"
    if [ -n "$_rd_val" ]; then
      printf '%s\n' "$_rd_val"
      return 0
    fi
  fi
  if ! _rd_vols="$(podman volume ls --format '{{.Name}}' 2>/dev/null)"; then
    echo "dev-db-password: podman cannot list volumes, so an existing database may hold a password this script does not know. Set POSTGRES_PASSWORD." >&2
    return 1
  fi
  case "$_rd_vols" in
    *vornik-postgres-data*)
      echo "dev-db-password: the vornik-postgres-data volume exists but no password is recorded in $_rd_env or POSTGRES_PASSWORD. Set POSTGRES_PASSWORD to the password that volume was created with." >&2
      return 1 ;;
  esac
  _rd_val="$(head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n')"
  if [ "${#_rd_val}" -ne 64 ]; then
    echo "dev-db-password: could not generate a password" >&2
    return 1
  fi
  # Replace an existing (empty) POSTGRES_PASSWORD= line or append one, keeping
  # the other lines; the same awk-through-ENVIRON write the quickstart's
  # env_set uses (the quickstart is a single curl|bash file, so it cannot
  # source this one). The value is hex, so it needs no escaping either way.
  ( umask 077
    [ -e "$_rd_env" ] || : > "$_rd_env"
    RD_VAL="$_rd_val" awk '
      BEGIN { v = ENVIRON["RD_VAL"]; done = 0 }
      index($0, "POSTGRES_PASSWORD=") == 1 { if (!done) { print "POSTGRES_PASSWORD=" v; done = 1 }; next }
      { print }
      END { if (!done) print "POSTGRES_PASSWORD=" v }
    ' "$_rd_env" > "$_rd_env.tmp.$$" && mv "$_rd_env.tmp.$$" "$_rd_env" ) || { rm -f "$_rd_env.tmp.$$"; return 1; }
  chmod 600 "$_rd_env" || return 1
  printf '%s\n' "$_rd_val"
}
