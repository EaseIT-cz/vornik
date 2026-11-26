#!/usr/bin/env bash
# Regenerate the opencode bundle's skills from the Codex bundle's (LLD 29 B1),
# under vornik- names so they never collide with the user's own skills.
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
src="$here/../codex-companion/skills"
rm -rf "$here/skills"
for d in "$src"/*/; do
	name=$(basename "$d")
	case "$name" in vornik-*) dst="$name" ;; *) dst="vornik-$name" ;; esac
	mkdir -p "$here/skills/$dst"
	sed -f "$here/skills-from-codex.sed" "$d/SKILL.md" > "$here/skills/$dst/SKILL.md"
done
