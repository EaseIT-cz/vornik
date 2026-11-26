# The opencode bundle's skills are the Codex bundle's, rewritten by these rules
# and nothing else (LLD 29 B1). sync-skills.sh applies them, and the bundle
# test applies them again and compares. Lists that name several assistants
# ("Claude / Codex / Hermes") are left alone.
s/^\(  \)Teaches Codex /\1Teaches opencode /
s/this Codex session/this opencode session/
s/amount of Codex context/amount of opencode context/
s/cannot read Codex's local filesystem/cannot read opencode's local filesystem/
s/^Codex ships no SessionStart hook, so — unlike the Claude companion — nothing$/The Vornik plugin adds a session digest when it can (LLD 29 B3), but nothing/
s/^Codex ships no SessionStart hook, so — as with the memory tools — you MUST$/The digest carries no repo scope, so — as with the memory tools — you MUST/
s/--client=codex/--client=opencode/
s/^Unlike Claude Code, Codex has no SessionStart hook announcing the detected$/Unlike Claude Code, opencode has no session hook announcing the detected/
s/runs several Codex sessions a day/runs several opencode sessions a day/
s/^Codex, and it's available everywhere\.$/opencode, and it's available everywhere./
# opencode names an MCP tool <server>_<tool> after the entry's name, so tools
# are named bare, as the daemon's own guidance names them.
s/mcp__vornik__//g
# The skill directory carries a vornik- prefix (sync-skills.sh), and a skill's
# front-matter name must match its directory, so the name gets it too.
1,5{/^name: vornik-/!s/^name: /name: vornik-/}
# opencode has no plugin root: connect installs the deposit script under the
# opencode config directory (LLD 29, slice 3).
s|^The logic ships with this plugin:$|The logic ships with the Vornik bundle, installed by vornikctl companion connect opencode:|
s|^  \./scripts/vornik-backlog-deposit\.sh \\$|  "${XDG_CONFIG_HOME:-$HOME/.config}/opencode/vornik-companion/scripts/vornik-backlog-deposit.sh" \\|
