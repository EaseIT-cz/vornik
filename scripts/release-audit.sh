#!/usr/bin/env bash
#
# release-audit.sh — for the last N EE releases, did the publication actually
# produce the artifacts the release promised?
#
# Gaps 5 and 6 of enterprise-packaging-design.md's 2026-09-22 amendment. Both
# are OUTCOMES that the 2026-09-06 and 2026-09-22 work mechanised the TRIGGER
# for and left unverified:
#
#   Gap 5 — a CE tag fires publish-agent-image, but nothing asserts the image
#           :<tag> ever reached the registry. A build failure, a registry push
#           failure or an exhausted quota leaves no versioned image and the
#           release is still declared complete.
#   (Gap 6, the EE mirror's sync PR, is retired with the mirror: EaseIT-cz
#   migration design §6.)
#
# It also checks the CE publish token's recorded expiry (migration plan T6.6).
#
# Usage: scripts/release-audit.sh [count]
#        scripts/release-audit.sh --check-token   (the token expiry only)
set -uo pipefail

EE="${RELEASE_AUDIT_EE_REPO:-EaseIT-cz/vornik}"
CE="${RELEASE_AUDIT_CE_REPO:-EaseIT-cz/vornik}"
IMAGE="${RELEASE_AUDIT_IMAGE:-ghcr.io/easeit-cz/vornik-agent}"

problems=0
# The `return 0` is not decoration. Without it the function's status is the
# `[ -n ... ]` test, which is FALSE whenever GITHUB_STEP_SUMMARY is unset — so
# the last note in the success path made the whole script exit 1 while printing
# "All audited releases produced what they promised". A control that reports
# success and exits failure is the tenet-4 failure in its purest form, and this
# one was in the script written to catch that class.
note() {
	printf '%s\n' "$1"
	[ -n "${GITHUB_STEP_SUMMARY:-}" ] && printf '%s\n' "$1" >> "$GITHUB_STEP_SUMMARY"
	return 0
}
bad()  { note "- **$1**"; problems=$((problems + 1)); }

# The CE publish token (CE_PUBLISH_TOKEN, a fine-grained PAT) expires, and
# GitHub does not expose the expiry to Actions; RELEASE.md's rotation step
# records it in the CE_PUBLISH_TOKEN_EXPIRES repository variable. Nothing binds
# the variable to the token, so a missing, empty or unparseable value FAILS:
# a silent pass would hide exactly the rotation-without-update this exists
# for. 30 days or fewer fails too: this audit is weekly and not on the release
# path, so failing is a month's alarm, never a blocked release.
check_token() {
	local expires="${CE_PUBLISH_TOKEN_EXPIRES:-}" today="${RELEASE_AUDIT_TODAY:-$(date -u +%F)}" days
	if [ -z "$expires" ]; then
		bad "CE_PUBLISH_TOKEN_EXPIRES is not set — record the publish token's expiry (YYYY-MM-DD) as a repository variable when it is created or rotated"
		return
	fi
	local exp_s now_s
	if ! [[ "$expires" =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}$ ]] || ! exp_s=$(date -u -d "$expires" +%s 2>/dev/null); then
		bad "CE_PUBLISH_TOKEN_EXPIRES='$expires' is not a date (YYYY-MM-DD)"
		return
	fi
	now_s=$(date -u -d "$today" +%s)
	days=$(( (exp_s - now_s) / 86400 ))
	if [ "$days" -le 30 ]; then
		bad "the CE publish token expires on $expires ($days day(s) left) — rotate it and update CE_PUBLISH_TOKEN_EXPIRES (RELEASE.md)"
	else
		note "- CE publish token expires on $expires ($days days left)"
	fi
}

if [ "${1:-}" = "--check-token" ]; then
	check_token
	[ "$problems" -eq 0 ] || exit 1
	exit 0
fi
COUNT="${1:-5}"

tags=$(gh release list -R "$EE" -L "$COUNT" 2>/dev/null | cut -f1)
[ -n "$tags" ] || { echo "release-audit: no releases found on $EE"; exit 1; }

note "## Release audit — last $COUNT releases of $EE"
check_token

for tag in $tags; do
	note ""
	note "### $tag"

	# --- Gap 5: the versioned agent image ---------------------------------
	#
	# ANONYMOUS. ghcr.io/easeit-cz/vornik-agent is public, so reading its
	# manifest needs no token and no new secret — the same reasoning
	# wait-ce-ci.sh already uses for the public CE run.
	#
	# A MISS IS NOT IMMEDIATELY A FAILURE. GHCR is eventually consistent and
	# a freshly pushed manifest can be unresolvable for tens of seconds. But
	# EXHAUSTING the attempts IS a failure — a "poll until it resolves" with
	# no terminal state turns a missing image into a slow green, which is the
	# whole defect class this audit exists for.
	found=0
	for attempt in 1 2 3; do
		if token=$(curl -fsS "https://ghcr.io/token?scope=repository:${IMAGE#ghcr.io/}:pull&service=ghcr.io" 2>/dev/null | \
		           python3 -c 'import json,sys;print(json.load(sys.stdin).get("token",""))' 2>/dev/null) && [ -n "$token" ]; then
			if curl -fsS -o /dev/null -H "Authorization: Bearer $token" \
			   -H 'Accept: application/vnd.oci.image.index.v1+json,application/vnd.docker.distribution.manifest.list.v2+json' \
			   "https://ghcr.io/v2/${IMAGE#ghcr.io/}/manifests/$tag" 2>/dev/null; then
				found=1; break
			fi
		fi
		[ "$attempt" -lt 3 ] && sleep 10
	done
	if [ "$found" -eq 1 ]; then
		note "- agent image \`$IMAGE:$tag\` resolves"
	else
		# MISSING vs STILL BUILDING. The image build takes ~18 minutes and this
		# retry loop waits 30 seconds, so an audit run shortly after a release
		# would report a red for an image that is on its way. A control that
		# cannot tell "absent" from "not finished yet" reports the first and
		# means the second — the same tenet-4 shape this audit exists to catch,
		# so it asks the build instead of guessing from the registry alone.
		#
		# PENDING is not a pass: it is reported and does NOT count as a
		# problem, because there is nothing for an operator to do yet. A build
		# that has FAILED or that never started is a problem, and says which.
		build=$(gh run list -R "${CE_REPO_FOR_IMAGE:-$CE}" \
			--workflow publish-agent-image.yml -L 20 \
			--json status,conclusion,headBranch 2>/dev/null | \
			python3 -c "
import json,sys
tag=sys.argv[1]
try:
    for r in json.load(sys.stdin):
        if r.get('headBranch')==tag:
            print(r['status'], r.get('conclusion') or ''); break
    else: print('none')
except Exception: print('unknown')" "$tag" 2>/dev/null)
		case "$build" in
			in_progress*|queued*|requested*|waiting*)
				note "- agent image \`$IMAGE:$tag\` PENDING — publish-agent-image is still running for this tag" ;;
			completed*success*)
				bad "NO agent image \`$IMAGE:$tag\` although publish-agent-image SUCCEEDED for this tag — the build reported success and the registry has nothing" ;;
			none*)
				bad "NO agent image \`$IMAGE:$tag\` and publish-agent-image never ran for it — the CE tag should fire it; check the tag exists on the CE repo" ;;
			*)
				bad "NO agent image \`$IMAGE:$tag\` — publish-agent-image for this tag is \`$build\`" ;;
		esac
	fi

	# --- the CE release object -------------------------------------------
	if gh release view "$tag" -R "$CE" >/dev/null 2>&1; then
		note "- $CE has a release for \`$tag\`"
	else
		bad "$CE has a TAG but no RELEASE for \`$tag\`"
	fi

done

note ""
if [ "$problems" -gt 0 ]; then
	note "**$problems problem(s).** Each names its resolution above."
	exit 1
fi
note "**All audited releases produced what they promised.**"
exit 0
