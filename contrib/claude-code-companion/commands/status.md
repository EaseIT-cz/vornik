---
description: Check status of a specific vornik task
allowed-tools: mcp__plugin_vornik-companion_vornik__status
argument-hint: <task_id>
---

# Status of a vornik task

The user wants the current state of task `$ARGUMENTS`.

Call `mcp__plugin_vornik-companion_vornik__status` with `task_id=$ARGUMENTS`. Surface the result
in plain English: which workflow it's running, what state it's in, when
it was created, and any `last_error`.

While the task runs, the answer may carry `progress`: say where it is
("step 2 of 3, curate, started 40 s ago"; "running curate" when there is
no `step_index`). There is no time estimate; do not invent one.

If the status is COMPLETED / FAILED / CANCELLED, remind the user they
can pull the final artifacts with `/result $ARGUMENTS`.
