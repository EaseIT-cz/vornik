---
description: Show what Vornik's memory holds for this project, a page at a time, and how to forget or export it
allowed-tools: mcp__vornik__memory_list, mcp__vornik__memory_correct, mcp__vornik__recent_memory
argument-hint: "[words to search] | forget <id> | export"
---

# What Vornik remembers

The user asked to see what Vornik's memory holds: **$ARGUMENTS**

- **No arguments, or words:** call `mcp__vornik__memory_list` (with
  `query=$ARGUMENTS` when words were given, otherwise no query, `limit` 20).
  Show one line per item: `short_id`, `age`, `source`, `origin`, then the
  `snippet`. Say how many were returned; when `next_cursor` is set, offer the
  next page (call again with `cursor` set to it, unchanged). Never paste the
  whole memory, and never fetch every page unasked.
- **`forget <id>`:** call `mcp__vornik__memory_correct` with
  `chunk_ids=[<id>]` and `reason="forget_command"`. Use the full `id` from
  the listing; when the user gives a `short_id`, find the item with that
  `short_id` in the listing you showed and send its `id` (a short id is not
  always a prefix of the id). Report
  `refuted_count`: 1 means forgotten (every assistant stops recalling it; the
  record stays until retention removes it), 0 means nothing was found.
- **`export`:** do not export through the conversation. Call
  `mcp__vornik__memory_list` once and give the user its `export_url`: it
  opens only on their paired approver device, where they can download
  everything as JSON or Markdown and forget items themselves.

If the tool is missing or answers that memory listing is not available, the
daemon is older than this command: fall back to `mcp__vornik__recent_memory`
(the 20 newest) and say export is not available yet.
