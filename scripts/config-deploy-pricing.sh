#!/usr/bin/env bash
# config-deploy-pricing.sh — add the shipped model rows an edited pricing.yaml
# lacks, without changing any line it has. Sourced by config-deploy.sh; tested
# by scripts/config-deploy-pricing_test.sh. Design: config-tree drift design,
# amendment 2026-11-19 (GitHub #776).
#
# WHY. Operators price their own models in pricing.yaml, and an edited file is
# never overwritten, so shipped rows cannot reach it by copy. Before this, a model added to
# the shipped file stayed unpriced on every existing install, its spend
# charged at the default rate, until someone edited the file by hand
# (reference host, 2026-11-14, #772's three Codex rows).
#
# It is a line-based merge, not a YAML rewrite: the host has no Python, and a
# re-serialised file would lose the operator's comments. So it accepts one
# narrow shape, refuses everything else with a WARN, and installs a result
# only after `vornikctl pricing check` (the daemon's own parse) accepts it.

# pricing_parse FILE — print "R<TAB>id<TAB>row" per model row (row lines joined
# by \037), "I<TAB>n" (the last content line of the models: block), or a single
# "E<TAB>reason" when the file is not a shape this merge accepts.
pricing_parse() {
	awk '
	function fail(r) { print "E\t" r; failed = 1; exit }
	function id_of(s,   q, k, i, c) {
		s = substr(s, 3)
		q = substr(s, 1, 1)
		if (q == "\"" || q == "\047") {
			k = index(substr(s, 2), q)
			if (k == 0) fail("an unterminated quoted key")
			return substr(s, 2, k - 1)
		}
		for (i = 1; i <= length(s); i++) {
			if (substr(s, i, 1) != ":") continue
			c = substr(s, i + 1, 1)
			if (c == "" || c == " " || c == "\t") return substr(s, 1, i - 1)
		}
		fail("a key line with no key")
	}
	function code(s) { sub(/[ \t]#.*$/, "", s); return s }
	{ if ($0 ~ /\r/) fail("a carriage return (CRLF line ends)"); line[NR] = $0 }
	END {
		if (failed) exit
		m = 0
		for (i = 1; i <= NR; i++) if (line[i] ~ /^models:[ \t]*(#.*)?$/) { n++; m = i }
		if (n != 1) fail("not exactly one models: block")
		cur = ""; last = m
		for (i = m + 1; i <= NR; i++) {
			l = line[i]
			if (l ~ /^[^ \t#]/) break
			if (l ~ /^[ \t]*$/) { cur = ""; continue }
			if (l ~ /^\t/) fail("tab indentation in the models: block")
			last = i
			if (l ~ /^[ \t]*#/) {
				if (cur != "" && l ~ /^   /) row[cur] = row[cur] "\037" l
				continue
			}
			c = code(l)
			if (c ~ /(^|[ :{,\[])[&*][A-Za-z0-9_]/ || c ~ /<<:/) fail("a YAML anchor, alias or merge key in the models: block")
			if (l ~ /^  [^ ]/) {
				if (index(c, "{") && !index(c, "}")) fail("a flow row ({) that does not close on its line")
				cur = id_of(l)
				if (cur in row) fail("the id " cur " twice")
				order[++k] = cur; row[cur] = l
			} else if (cur == "") {
				fail("model keys not indented by two spaces (or a blank line inside a row)")
			} else {
				row[cur] = row[cur] "\037" l
			}
		}
		for (j = 1; j <= k; j++) print "R\t" order[j] "\t" row[order[j]]
		print "I\t" last
	}' "$1"
}

# pricing_ctl — the vornikctl that checks a merged file: $VORNIK_CTL when set
# (and only it), else bin/vornikctl beside the repository, else PATH.
pricing_ctl() {
	if [ -n "${VORNIK_CTL+x}" ]; then
		[ -x "$VORNIK_CTL" ] && echo "$VORNIK_CTL"
		return 0
	fi
	if [ -x "$SCRIPT_DIR/../bin/vornikctl" ]; then echo "$SCRIPT_DIR/../bin/vornikctl"; return 0; fi
	command -v vornikctl 2>/dev/null || true
}

# list_ids — "a, b, c" for at most 10 ids on stdin, then "and K more".
list_ids() { awk 'NR <= 10 { s = s (NR > 1 ? ", " : "") $0 } END { if (NR > 10) s = s " and " NR - 10 " more"; print s }'; }

# merge_pricing_rows SRC DEST — run for a pricing.yaml that still differs from
# the template after deploy_one (the caller checks that and the slice-H skips).
merge_pricing_rows() {
	local src="$1" dest="$2" sp lp miss diff tmp ctl n_old n_new
	sp="$(pricing_parse "$src")"; lp="$(pricing_parse "$dest")"
	for p in "$sp" "$lp"; do
		if grep -q '^E	' <<<"$p"; then
			echo "WARN: config-deploy: pricing.yaml: not merged, $(sed -n 's/^E	//p' <<<"$p" | sed -n 1p); add new shipped rows by hand" >&2
			return 0
		fi
	done
	# Shipped ids the live file lacks, in shipped order; ids in both whose rows differ.
	miss="$(awk -F'\t' 'NR == FNR { if ($1 == "R") have[$2] = 1; next } $1 == "R" && !($2 in have) { print $2 }' <(printf '%s\n' "$lp") <(printf '%s\n' "$sp"))"
	diff="$(awk -F'\t' 'function n(s) { gsub(/[ \t\037]+/, " ", s); return s }
		NR == FNR { if ($1 == "R") r[$2] = n($3); next } $1 == "R" && ($2 in r) && r[$2] != n($3) { print $2 }' \
		<(printf '%s\n' "$lp") <(printf '%s\n' "$sp"))"
	[ -n "$diff" ] && echo "config-deploy: pricing.yaml: kept your prices for $(printf '%s\n' "$diff" | grep -c .) model id(s) that differ from the shipped ones: $(printf '%s\n' "$diff" | list_ids)"
	[ -n "$miss" ] || return 0

	tmp="$dest.vornik-new.$$"
	awk -F'\t' -v at="$(printf '%s\n' "$lp" | sed -n 's/^I	//p')" -v rev="${DEPLOY_REV:-unknown}" \
		'NR == FNR { if ($1 == "R") { want[$2] = 1 } ; next }
		 FILENAME == ARGV[2] { if ($1 == "R" && ($2 in want)) { gsub(/\037/, "\n", $3); add = add $3 "\n" } ; next }
		 { print } FNR == at && add != "" { printf "  # Added by config-deploy at %s: shipped model rows this file lacked.\n%s", rev, add }' \
		<(printf '%s\n' "$miss" | sed 's/^/R	/') <(printf '%s\n' "$sp") "$dest" > "$tmp" || { rm -f "$tmp"; return 0; }

	n_old="$(printf '%s\n' "$lp" | grep -c '^R	')"
	n_new="$(pricing_parse "$tmp" | grep -c '^R	')"
	if [ "$n_new" != "$((n_old + $(printf '%s\n' "$miss" | grep -c .)))" ]; then
		rm -f "$tmp"
		echo "WARN: config-deploy: pricing.yaml: not merged, the result did not have the expected rows; add by hand: $(printf '%s\n' "$miss" | list_ids)" >&2
		return 0
	fi
	ctl="$(pricing_ctl)"
	if [ -z "$ctl" ] || ! "$ctl" pricing check "$tmp" >/dev/null 2>&1; then
		rm -f "$tmp"
		echo "WARN: config-deploy: pricing.yaml: not merged, $([ -z "$ctl" ] && echo "no vornikctl to check the result" || echo "vornikctl pricing check refused the result"); add these shipped rows by hand: $(printf '%s\n' "$miss" | list_ids)" >&2
		return 0
	fi
	if ! cp -p "$dest" "${DEST:?config-deploy.sh sets DEST}/.pricing.yaml.pre-merge"; then
		rm -f "$tmp"
		echo "WARN: config-deploy: pricing.yaml: not merged, could not keep the pre-merge copy; add by hand: $(printf '%s\n' "$miss" | list_ids)" >&2
		return 0
	fi
	replace_file "$tmp" "$dest"
	echo "config-deploy: pricing.yaml: added $(printf '%s\n' "$miss" | grep -c .) shipped model row(s): $(printf '%s\n' "$miss" | list_ids)"
}
