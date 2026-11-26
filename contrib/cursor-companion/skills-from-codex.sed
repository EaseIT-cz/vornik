# The Cursor bundle's skills are the Codex bundle's, rewritten by these rules
# and nothing else (LLD 28 B4). sync-skills.sh applies them, and
# TestCursorBundle_SkillsAreTheCodexCopiesRewritten applies them again and
# compares, so an edit to a Codex skill reaches Cursor only through here.
# Lists that name several assistants ("Claude / Codex / Hermes") are left
# alone: they name Codex as one client among others.
s/^\(  \)Teaches Codex /\1Teaches Cursor /
s/this Codex session/this Cursor session/
s/amount of Codex context/amount of Cursor context/
s/cannot read Codex's local filesystem/cannot read Cursor's local filesystem/
s/^Codex ships no SessionStart hook, so — unlike the Claude companion — nothing$/Cursor's session hook is best effort (LLD 28 B5), so — unlike the Claude companion — nothing/
s/^Codex ships no SessionStart hook, so — as with the memory tools — you MUST$/Cursor's session hook is best effort, so — as with the memory tools — you MUST/
s/--client=codex/--client=cursor/
s/^Unlike Claude Code, Codex has no SessionStart hook announcing the detected$/Unlike Claude Code, Cursor has no reliable session hook announcing the detected/
s/runs several Codex sessions a day/runs several Cursor sessions a day/
s/^Codex, and it's available everywhere\.$/Cursor, and it's available everywhere./
# Cursor's naming of MCP tools is unverified (LLD 28 A2), so tools are named
# bare, as the daemon's own guidance names them.
s/mcp__vornik__//g
