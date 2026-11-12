#!/usr/bin/env bash
# rpm-install-check.sh <rpm> — install the enterprise RPM in a clean Fedora
# container and assert what a host gets: the binary, unit, example and EULA,
# and (GitHub #726, #728) the registry and sign-up templates a managed VM
# needs, owned by the daemon's user, and after `provision` a config the daemon
# can replace in a directory whose root-owned files it cannot remove. A second
# container installs self-hosted (#731, #732): the same config layout from
# gen-config, and git pulled in as a declared dependency.
#
# The CI RPM job runs this; so can anyone with podman (GitHub Actions was
# unavailable when #726/#728 shipped, so it ran locally first). Found-at level
# for both issues: e2e-lane (vornik-cloud's image boot test, Fedora 44).
#
# Env: FEDORA_IMAGE (default fedora:41, the CI job's).
set -euo pipefail
RPM="${1:?usage: rpm-install-check.sh <path-to-rpm>}"
[ -f "$RPM" ] || { echo "rpm-install-check: $RPM not found" >&2; exit 2; }
DIST="$(cd "$(dirname "$RPM")" && pwd)"
BASE="$(basename "$RPM")"
IMAGE="${FEDORA_IMAGE:-fedora:41}"

# provision's input names the control plane's HPKE public key; any valid key
# does here (the sealed output is not opened).
GEN="$(mktemp -d)"
trap 'rm -rf "$GEN"' EXIT
cat > "$GEN/main.go" <<'EOF'
package main

import (
	"crypto/hpke"
	"encoding/base64"
	"fmt"
)

func main() {
	k, err := hpke.MLKEM768X25519().GenerateKey()
	if err != nil {
		panic(err)
	}
	fmt.Println(base64.StdEncoding.EncodeToString(k.PublicKey().Bytes()))
}
EOF
(cd "$GEN" && go mod init rpmcheck >/dev/null 2>&1)
RECIPIENT="$(cd "$GEN" && go run .)"

echo "rpm-install-check: installing $BASE in $IMAGE"
podman run --rm --security-opt label=disable -e RECIPIENT="$RECIPIENT" -v "$DIST:/dist:ro" "$IMAGE" bash -c '
set -euo pipefail
fail=0
check() { if eval "$2"; then echo "  ok   $1"; else echo "  FAIL $1"; fail=1; fi; }
# A managed image creates this before installing, so no config is generated.
mkdir -p /etc/vornik && : > /etc/vornik/managed-image
dnf install -y "/dist/'"$BASE"'" >/tmp/dnf.log 2>&1 || { echo DNF_FAILED; tail -25 /tmp/dnf.log; exit 1; }

echo "== the package"
check "binary" "test -x /usr/bin/vornik-enterprise"
check "unit" "test -f /usr/lib/systemd/system/vornik-enterprise.service"
check "example config" "test -f /etc/vornik/vornik.yaml.example"
check "EULA" "test -f /usr/share/doc/vornik-enterprise/EULA.txt"
/usr/bin/vornik-enterprise migrate-ce 2>&1 | sed -n 1p || true

echo "== #726: the registry and the sign-up templates"
C=/etc/vornik/configs
check "registry layout (projects, swarms, workflows)" "test -d $C/projects && test -d $C/swarms && test -d $C/workflows"
check "engineering template (companion)" "test -f $C/project-templates/companion/template.yaml"
check "assistant template (personal-assistant)" "test -f $C/project-templates/personal-assistant/template.yaml"
check "the workflows they name" "test -f $C/workflows/adaptive.md && test -f $C/workflows/companion-architectural-review.md"
check "drift baselines stamped with the version" "test -s $C/.templates/.stamp"
check "the tree is the daemon user'"'"'s" "[ \"\$(stat -c %U $C)\" = vornik ] && [ \"\$(stat -c %U $C/workflows/adaptive.md)\" = vornik ] && [ \"\$(stat -c %U $C/projects)\" = vornik ]"
check "the daemon can write a seeded project" "runuser -u vornik -- sh -c \": > $C/projects/.probe && rm $C/projects/.probe\""

echo "== #728: provision, then the daemon replaces its own config"
printf "{\"schema\":1,\"instance_id\":\"rpm-check\",\"operations_address\":\"10.0.1.5:9443\",\"trusted_proxies\":[\"10.0.2.0/24\"],\"model_hosts\":[],\"recipient\":\"%s\"}" "$RECIPIENT" \
  | /usr/bin/vornik-enterprise provision --input - >/tmp/provision.out 2>/tmp/provision.err || { echo "PROVISION_FAILED"; cat /tmp/provision.err; exit 1; }
check "/etc/vornik is root:vornik 1770" "[ \"\$(stat -c \"%a %U %G\" /etc/vornik)\" = \"1770 root vornik\" ]"
check "config is vornik:vornik 0600" "[ \"\$(stat -c \"%a %U %G\" /etc/vornik/vornik.yaml)\" = \"600 vornik vornik\" ]"
check "marker is root:vornik 0640" "[ \"\$(stat -c \"%a %U %G\" /etc/vornik/provisioned)\" = \"640 root vornik\" ]"
check "vornik replaces the config by temp-and-rename, as setup does" "runuser -u vornik -- sh -c \"t=\\\$(mktemp /etc/vornik/.config-XXXX.yaml.tmp) && cat /etc/vornik/vornik.yaml > \\\$t && cp /etc/vornik/vornik.yaml /etc/vornik/vornik.yaml.bak-probe && mv \\\$t /etc/vornik/vornik.yaml\""
check "vornik cannot remove the marker" "! runuser -u vornik -- rm -f /etc/vornik/provisioned 2>/dev/null && test -f /etc/vornik/provisioned"
check "vornik cannot replace the example" "! runuser -u vornik -- sh -c \": > /etc/vornik/.x && mv /etc/vornik/.x /etc/vornik/vornik.yaml.example\" 2>/dev/null"

[ "$fail" = 0 ] && echo "rpm-install-check: OK (managed)" || { echo "rpm-install-check: FAILED (managed)"; exit 1; }
'

# A self-hosted install (no managed-image marker), in a second clean container
# (GitHub #731, #732; design C5, C6): gen-config writes the config, and the
# postinstall gives it to the daemon in a root:vornik 1770 /etc/vornik. git
# arrives as a declared dependency (the base image has none).
echo "rpm-install-check: self-hosted install of $BASE in $IMAGE"
podman run --rm --security-opt label=disable -v "$DIST:/dist:ro" "$IMAGE" bash -c '
set -euo pipefail
fail=0
check() { if eval "$2"; then echo "  ok   $1"; else echo "  FAIL $1"; fail=1; fi; }
had_git=no; command -v git >/dev/null 2>&1 && had_git=yes
dnf install -y "/dist/'"$BASE"'" >/tmp/dnf.log 2>&1 || { echo DNF_FAILED; tail -25 /tmp/dnf.log; exit 1; }

echo "== #732: git is a declared dependency"
check "the package requires git" "rpm -q --requires vornik-enterprise | grep -qx git"
check "git is installed (the base image had it: $had_git)" "command -v git >/dev/null"

echo "== #731: the daemon owns its config"
check "config generated" "test -s /etc/vornik/vornik.yaml"
check "/etc/vornik is root:vornik 1770" "[ \"\$(stat -c \"%a %U %G\" /etc/vornik)\" = \"1770 root vornik\" ]"
check "config is vornik:vornik 0600" "[ \"\$(stat -c \"%a %U %G\" /etc/vornik/vornik.yaml)\" = \"600 vornik vornik\" ]"
check "vornik replaces the config by temp-and-rename, as setup does" "runuser -u vornik -- sh -c \"t=\\\$(mktemp /etc/vornik/.config-XXXX.yaml.tmp) && cat /etc/vornik/vornik.yaml > \\\$t && cp /etc/vornik/vornik.yaml /etc/vornik/vornik.yaml.bak-probe && mv \\\$t /etc/vornik/vornik.yaml\""
check "vornik cannot replace the example" "! runuser -u vornik -- sh -c \": > /etc/vornik/.x && mv /etc/vornik/.x /etc/vornik/vornik.yaml.example\" 2>/dev/null"
check "an upgrade (reinstall) keeps the layout" "dnf reinstall -y /dist/'"$BASE"' >/tmp/dnf2.log 2>&1 && [ \"\$(stat -c \"%a %U %G\" /etc/vornik)\" = \"1770 root vornik\" ] && [ \"\$(stat -c %U /etc/vornik/vornik.yaml)\" = vornik ]"

[ "$fail" = 0 ] && echo "rpm-install-check: OK (self-hosted)" || { echo "rpm-install-check: FAILED (self-hosted)"; exit 1; }
'
