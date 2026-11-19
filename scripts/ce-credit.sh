#!/usr/bin/env bash
# ce-credit.sh — credit and close the community PRs a CE sync ships.
#
# Sourced by scripts/export-public-ce.sh; tested by
# scripts/export-public-ce-credit_test.sh. Design: export runbook, amendment
# 2026-11-19 (GitHub #840).
#
# WHY. The public repository takes no merges: a community PR is ported into
# EE and reaches the public repo inside one sync commit authored by the
# operator. Before this, the contributor's credit stopped at the EE commit and
# nothing closed the PR (EaseIT-cz/vornik#19 and #21, 2026-11-19).
#
# The rules that keep it safe:
#   - a CE-PR / CE-PR-Partial trailer is read with git's trailer parser, by
#     exact key, and counts only when the commit's author or one of its
#     Co-authored-by emails is on that PR's commits, so a community patch
#     cannot name another person's PR to get it closed;
#   - an identity is credited only by an email on that PR's commits, under
#     the name GitHub has for it there, so no EE-internal identity is written;
#   - a CE-PR already recorded as CE-PR-Shipped in the public history is not
#     credited or closed again.
# A gh failure is a warning: the sync goes on without that credit or close.

CE_REPO="${CE_REPO:-EaseIT-cz/vornik}"

ce_warn() { echo "WARN: ce-credit: $*" >&2; }

# ce_pr_refs SRC FROM TO — "full|partial<TAB>n<TAB>sha" for each commit in
# FROM..TO carrying a valid trailer; CE-PR wins over CE-PR-Partial.
ce_pr_refs() {
	local src="$1" from="$2" to="$3" sha full part v kind
	# Fields split on \x1f, not a tab: bash collapses runs of IFS whitespace,
	# so an empty CE-PR field between two tabs would shift CE-PR-Partial into
	# its place and close a partly ported PR.
	git -C "$src" log --reverse --format='%H%x1f%(trailers:key=CE-PR,valueonly,separator=%x2C)%x1f%(trailers:key=CE-PR-Partial,valueonly,separator=%x2C)' "$from..$to" |
		while IFS=$'\x1f' read -r sha full part; do
			for kind in full partial; do
				v="$full"; [ "$kind" = partial ] && v="$part"
				[ -n "$v" ] || continue
				[ "$kind" = partial ] && [ -n "$full" ] && continue
				IFS=',' read -ra vals <<<"$v"
				for one in "${vals[@]}"; do
					one="$(printf '%s' "$one" | tr -d '[:space:]')"
					num="${one#"$CE_REPO#"}"
					# All digits, nothing else: "#5;x" or "#5x" is not a PR number.
					case "$one:$num" in
						"$CE_REPO#"*:[0-9]*) case "$num" in *[!0-9]*) num="" ;; esac ;;
						*) num="" ;;
					esac
					if [ -n "$num" ]; then
						printf '%s\t%s\t%s\n' "$kind" "$num" "$sha"
					else
						ce_warn "ignoring trailer value '$one' on ${sha:0:12}: not $CE_REPO#<n>"
					fi
				done
			done
		done
}

# ce_pr_identities N — "email<TAB>name" for every author and committer on the
# PR's commits (cached per PR in CE_CREDIT_CACHE).
ce_pr_identities() {
	local n="$1" cache="${CE_CREDIT_CACHE:?}/pr-$1"
	if [ ! -f "$cache" ]; then
		gh api "repos/$CE_REPO/pulls/$n/commits" --jq \
			'.[] | (.commit.author.email + "\t" + .commit.author.name), (.commit.committer.email + "\t" + .commit.committer.name)' \
			>"$cache.tmp" 2>/dev/null || { rm -f "$cache.tmp"; return 1; }
		mv "$cache.tmp" "$cache"
	fi
	cat "$cache"
}

# ce_commit_emails SRC SHA — the commit's author email and its Co-authored-by emails.
ce_commit_emails() {
	git -C "$1" log -1 --format='%ae%n%(trailers:key=Co-authored-by,valueonly)' "$2" |
		sed -n 's/.*<\([^>]*\)>.*/\1/p; /^[^<]*@[^<]*$/p' | sort -u
}

# ce_verified_refs SRC FROM TO SHIPPED — the refs whose commit is a port of
# that PR (an author or co-author is on its commits), skipping a full ref
# already in SHIPPED (a file of PR numbers). Echoes "kind<TAB>n<TAB>sha".
ce_verified_refs() {
	local src="$1" from="$2" to="$3" shipped="${4:-/dev/null}" kind n sha ids
	ce_pr_refs "$src" "$from" "$to" | while IFS=$'\t' read -r kind n sha; do
		if [ "$kind" = full ] && grep -qx "$n" "$shipped" 2>/dev/null; then
			continue
		fi
		if ! ids="$(ce_pr_identities "$n")"; then
			ce_warn "could not read the commits of $CE_REPO#$n; no credit or close for it"
			continue
		fi
		local emails; emails="$(ce_commit_emails "$src" "$sha")"
		if grep -qxFf <(cut -f1 <<<"$ids") <<<"$emails"; then
			printf '%s\t%s\t%s\n' "$kind" "$n" "$sha"
		else
			ce_warn "ignoring the trailer naming $CE_REPO#$n on ${sha:0:12}: none of its authors is on that PR"
		fi
	done
}

# ce_credit_trailers SRC FROM TO [SHIPPED] — the Co-authored-by trailers for
# the sync commit, each once, then a CE-PR-Shipped trailer per full ref.
ce_credit_trailers() {
	local src="$1" from="$2" to="$3" shipped="${4:-/dev/null}"
	if [ -z "$from" ]; then
		ce_warn "no previous sync recorded; no contributor credit on this sync"
		return 0
	fi
	local tmp; tmp="$(mktemp -d)"; local CE_CREDIT_CACHE="$tmp"
	local refs; refs="$(ce_verified_refs "$src" "$from" "$to" "$shipped")"
	local kind n sha email name
	while IFS=$'\t' read -r kind n sha; do
		[ -n "$n" ] || continue
		while read -r email; do
			# The name GitHub has for that email on the PR, with any control
			# character removed before it becomes a trailer (review 2aa6 L5).
			name="$(ce_pr_identities "$n" | awk -F'\t' -v e="$email" '$1 == e { print $2; exit }' | tr -d '[:cntrl:]')"
			if [ -n "$name" ]; then
				printf 'Co-authored-by: %s <%s>\n' "$name" "$email"
			else
				ce_warn "not crediting <$email> for $CE_REPO#$n: that address is not on the PR's commits"
			fi
		done < <(ce_commit_emails "$src" "$sha")
	done <<<"$refs" | awk '!seen[$0]++'
	awk -F'\t' -v r="$CE_REPO" '$1 == "full" && !seen[$2]++ { print "CE-PR-Shipped: " r "#" $2 }' <<<"$refs"
	rm -rf "$tmp"
}

# ce_close_prs SRC FROM TO URL [SHIPPED] — after a successful publish: comment
# on each shipped PR with URL; close it for CE-PR, leave it open for
# CE-PR-Partial; leave an already-closed PR alone. Never fails.
ce_close_prs() {
	local src="$1" from="$2" to="$3" url="$4" shipped="${5:-/dev/null}"
	[ -n "$from" ] || return 0
	local tmp; tmp="$(mktemp -d)"; local CE_CREDIT_CACHE="$tmp"
	local kind n sha state seen=" "
	while IFS=$'\t' read -r kind n sha; do
		[ -n "$n" ] || continue
		case "$seen" in *" $kind$n "*) continue ;; esac
		seen="$seen$kind$n "
		state="$(gh pr view "$n" -R "$CE_REPO" --json state -q .state 2>/dev/null)" || {
			ce_warn "could not read $CE_REPO#$n; run by hand: gh pr comment $n -R $CE_REPO -b 'Shipped in $url'"
			continue
		}
		[ "$state" = OPEN ] || continue
		if [ "$kind" = full ]; then
			if ! { gh pr comment "$n" -R "$CE_REPO" -b "Thank you. This change was ported into Vornik and is now public in $url. Closing, as the public repository takes changes through the export, not by merge." >/dev/null 2>&1 &&
				gh pr close "$n" -R "$CE_REPO" >/dev/null 2>&1; }; then
				ce_warn "could not comment on or close $CE_REPO#$n; run by hand: gh pr close $n -R $CE_REPO -c 'Shipped in $url'"
			fi
		else
			gh pr comment "$n" -R "$CE_REPO" -b "Part of this change was ported into Vornik and is now public in $url. The PR stays open for the rest." >/dev/null 2>&1 ||
				ce_warn "could not comment on $CE_REPO#$n; run by hand: gh pr comment $n -R $CE_REPO -b 'Partly shipped in $url'"
		fi
	done < <(ce_verified_refs "$src" "$from" "$to" "$shipped")
	rm -rf "$tmp"
	return 0
}
