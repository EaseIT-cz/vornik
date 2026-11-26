#!/usr/bin/env bash
# Regenerate the Cursor bundle's skills from the Codex bundle's (LLD 28 B4).
# Run after editing a Codex skill; the bundle test fails until you do.
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
src="$here/../codex-companion/skills"
rm -rf "$here/skills"
for d in "$src"/*/; do
	name=$(basename "$d")
	mkdir -p "$here/skills/$name"
	sed -f "$here/skills-from-codex.sed" "$d/SKILL.md" > "$here/skills/$name/SKILL.md"
done
