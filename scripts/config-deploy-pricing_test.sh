#!/usr/bin/env bash
# Tests for config-deploy.sh's pricing.yaml handling (config-tree drift design,
# amendment 2026-11-19, GitHub #776).
#
# #776: an install's pricing.yaml was seeded once and never took a shipped row
# on upgrade. Observed on the reference host 2026-11-14 (2026.11.5-3): #772's
# three Codex rows reached the template baseline but not the live file, so
# `vornikctl models list` showed them unpriced and their spend fell to the
# default rate. Two causes: top-level canonical files never followed the
# template (slice H covered directories only), and an edited file had no way
# to gain a row.
#
# Found at: production (reference host). Regression test at: e2e-lane, the
# real config-deploy.sh on temporary trees, with a stub vornikctl as the
# parse checker (the verb itself is tested in Go).
#
# Run: bash scripts/config-deploy-pricing_test.sh
set -u

HERE="$(cd "$(dirname "$0")" && pwd)"
DEPLOY="$HERE/config-deploy.sh"
TMP="$(mktemp -d "${TMPDIR:-/tmp}/cfgdeploy-pricing.XXXXXX")"
trap '[ -n "${KEEP:-}" ] || rm -rf "$TMP"' EXIT

fails=0
pass() { echo "ok   - $1"; }
fail() { echo "FAIL - $1"; fails=$((fails+1)); }

# A stub vornikctl: `pricing check <file>` exits $STUB_CHECK_RC (default 0)
# and records the file it was given.
mkdir -p "$TMP/bin"
cat > "$TMP/bin/vornikctl" <<'STUB'
#!/usr/bin/env bash
[ "$1 $2" = "pricing check" ] || exit 2
echo "$3" >> "${STUB_LOG:-/dev/null}"
exit "${STUB_CHECK_RC:-0}"
STUB
chmod +x "$TMP/bin/vornikctl"

shipped() { # the template's pricing.yaml
	cat > "$REPO/pricing.yaml" <<'EOF'
# Model prices, USD per 1M tokens.
models:
  gpt-6-luna:      { input: 0.10,  output: 0.50 }
  "glm-5.3:cloud": { input: 1.40, output: 4.40 } # :cloud suffix form
  qwen.qwen3-32b-v1:0: { input: 0.15, output: 0.62 }
  moonshot.kimi-k2-thinking:
    input: 0.60
    # a comment inside the row
    output: 2.50
  claude-x:        { input: 3.00,  output: 15.00 }

default: { input: 1.00, output: 3.00 }
EOF
}

build() { # build <name>: a fresh repo and target, deployed once at rev-1
	REPO="$TMP/$1/repo/configs"; TGT="$TMP/$1/t"; rm -rf "${TMP:?}/$1"; mkdir -p "$REPO/swarms"
	echo "dev" > "$REPO/swarms/dev-swarm.md"
	cat > "$REPO/pricing.yaml" <<'EOF'
models:
  claude-x:        { input: 3.00,  output: 15.00 }

default: { input: 1.00, output: 3.00 }
EOF
	deploy rev-1 >/dev/null
	LIVE="$TGT/configs/pricing.yaml"
}
deploy() {
	VORNIK_REPO_CONFIGS_DIR="$REPO" VORNIK_DEPLOY_REVISION="$1" VORNIK_CTL="${CTL-$TMP/bin/vornikctl}" \
		STUB_LOG="$TMP/stub.log" "$DEPLOY" "$TGT" 2>&1
}
operator_edits() { # the live file gets its own row and a changed shipped price
	cat > "$LIVE" <<'EOF'
models:
  claude-x:        { input: 2.50,  output: 12.00 }   # our negotiated rate
  gemma4-26b:      { input: 0.00,  output: 0.00 }    # local

default: { input: 1.00, output: 3.00 }
EOF
}
ids() { awk '/^models:/{m=1;next} m && /^[^ #]/{m=0} m && /^  [^ #]/{print}' "$1" | grep -c .; }

# P1: an unedited pricing.yaml follows a changed template (slice H, now for files).
build p1
shipped
out="$(deploy rev-2)"
if cmp -s "$LIVE" "$REPO/pricing.yaml"; then pass "an unedited pricing.yaml follows the template"
else fail "unedited pricing.yaml did not follow: $out"; fi

# P2: an edited one gains every missing row, keeps its own lines byte for byte,
# and lists the differing price.
build p2
operator_edits; before="$(cat "$LIVE")"
shipped
out="$(deploy rev-2)"
added_ok=1
for want in '  gpt-6-luna:      { input: 0.10,  output: 0.50 }' '  "glm-5.3:cloud": { input: 1.40, output: 4.40 } # :cloud suffix form' \
	'  qwen.qwen3-32b-v1:0: { input: 0.15, output: 0.62 }' '  moonshot.kimi-k2-thinking:' '    # a comment inside the row' '    output: 2.50'; do
	grep -qxF -- "$want" "$LIVE" || { added_ok=0; echo "  missing line: $want"; }
done
kept_ok=1
while IFS= read -r line; do grep -qxF -- "$line" "$LIVE" || { kept_ok=0; echo "  lost line: $line"; }; done <<< "$before"
if [ "$added_ok" = 1 ] && [ "$kept_ok" = 1 ] && [ "$(grep -c 'claude-x' "$LIVE")" = 1 ] \
	&& grep -q '# Added by config-deploy at rev-2' "$LIVE" \
	&& printf '%s' "$out" | grep -q 'pricing.yaml: added 4 shipped model row(s)' \
	&& printf '%s' "$out" | grep -q 'kept your prices for 1 model id(s) that differ from the shipped ones: claude-x' \
	&& [ "$(ids "$LIVE")" = 6 ] && [ -f "$TGT/configs/.pricing.yaml.pre-merge" ] \
	&& cmp -s <(printf '%s\n' "$before") "$TGT/configs/.pricing.yaml.pre-merge"; then
	pass "an edited pricing.yaml gains the 4 missing rows, keeps its own, lists claude-x, keeps a pre-merge copy"
else fail "edited merge: out=$out"$'\n'"$(cat "$LIVE")"; fi

# P3: the merged file went through the checker before it was installed.
if grep -q 'pricing.yaml' "$TMP/stub.log" 2>/dev/null; then pass "the result was checked with vornikctl pricing check"
else fail "the checker was not run"; fi

# P4: a second upgrade with nothing new changes nothing and adds no marker.
snap="$(cat "$LIVE")"; out="$(deploy rev-3)"
if [ "$(cat "$LIVE")" = "$snap" ] && ! grep -q 'rev-3' "$LIVE"; then pass "an upgrade with no new row leaves the file as it is"
else fail "a no-op upgrade changed the file: $out"; fi

# P5: a quoted shipped key matches an unquoted live copy of the same id.
build p5
operator_edits; printf '  glm-5.3:cloud: { input: 9.99, output: 9.99 }\n' > "$TMP/row"
sed -i "/^  gemma4-26b/r $TMP/row" "$LIVE"
shipped; deploy rev-2 >/dev/null
if [ "$(grep -c 'glm-5.3:cloud' "$LIVE")" = 1 ]; then pass "a quoted shipped key is the same id as an unquoted live one"
else fail "quoted/unquoted duplicate: $(grep 'glm-5.3' "$LIVE")"; fi

# P6: acknowledged, symlinked and two-hard-link files are left alone.
for kind in ack symlink hardlink; do
	build "p6$kind"; operator_edits
	case $kind in
		ack) printf 'pricing.yaml\trev-1\tok\n' > "$TGT/configs/.template-acks" ;;
		symlink) mv "$LIVE" "$TMP/p6$kind/real.yaml"; ln -s "$TMP/p6$kind/real.yaml" "$LIVE" ;;
		hardlink) ln "$LIVE" "$TMP/p6$kind/second-link" ;;
	esac
	snap="$(cat "$LIVE")"; shipped; deploy rev-2 >/dev/null
	if [ "$(cat "$LIVE")" = "$snap" ]; then pass "a $kind pricing.yaml is left alone"
	else fail "$kind file was changed"; fi
done

# P7: refused shapes keep the file and say why.
refuse_case() { # refuse_case <name> <live-file-content> <expected WARN fragment>
	build "$1"; printf '%s' "$2" > "$LIVE"; snap="$(cat "$LIVE")"; shipped
	out="$(deploy rev-2)"
	if [ "$(cat "$LIVE")" = "$snap" ] && printf '%s' "$out" | grep -q "WARN: config-deploy: pricing.yaml" \
		&& printf '%s' "$out" | grep -qi -- "$3"; then pass "refused shape: $1"
	else fail "shape $1 not refused: $out"; fi
}
refuse_case crlf $'models:\r\n  own: { input: 1, output: 1 }\r\n\r\ndefault: { input: 1, output: 3 }\r\n' 'carriage return'
refuse_case anchor $'models:\n  base: &b { input: 1, output: 1 }\n  own: *b\n\ndefault: { input: 1, output: 3 }\n' 'anchor'
refuse_case flow $'models:\n  own: { input: 1,\n      output: 1 }\n\ndefault: { input: 1, output: 3 }\n' 'flow'
refuse_case nomodels $'prices:\n  own: { input: 1, output: 1 }\n' 'models:'
refuse_case indent4 $'models:\n    own: { input: 1, output: 1 }\n\ndefault: { input: 1, output: 3 }\n' 'indent'

# P8: a checker that refuses the result, and no checker at all, keep the file
# and print the ids to add by hand.
for how in refuses absent; do
	build "p8$how"; operator_edits; snap="$(cat "$LIVE")"; shipped
	if [ "$how" = refuses ]; then out="$(STUB_CHECK_RC=1 deploy rev-2)"; else out="$(CTL="$TMP/none" PATH=/usr/bin:/bin deploy rev-2)"; fi
	if [ "$(cat "$LIVE")" = "$snap" ] && printf '%s' "$out" | grep -q 'gpt-6-luna'; then pass "a checker that $how keeps the file and names the rows to add"
	else fail "checker $how: file changed or no ids named: $out"; fi
done

# P9 (review 00d5 L1): an edited pricing.yaml with no origin record (an
# install from before origin tracking) still gains the missing rows.
build p9; operator_edits
awk -F'\t' '$1 != "pricing.yaml"' "$TGT/configs/.origin/.index" > "$TMP/idx" && cat "$TMP/idx" > "$TGT/configs/.origin/.index"
shipped; out="$(deploy rev-2)"
if grep -q '^  gpt-6-luna:' "$LIVE" && grep -q 'gemma4-26b' "$LIVE"; then pass "a pricing.yaml with no origin record still gains the missing rows"
else fail "no-origin file not merged: $out"; fi

echo ""
if [ "$fails" -eq 0 ]; then echo "config-deploy-pricing: ALL PASS"; exit 0; fi
echo "config-deploy-pricing: $fails case(s) failed"; exit 1
