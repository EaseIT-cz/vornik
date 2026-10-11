# Working alongside Vornik

Vornik runs work for you in the background, on its own models, while you
keep working. These rules make that useful. The two rules marked
(delegation) need the `delegate` tool; if it is not among your tools, skip
them and follow the rest.

1. **Delegate the user's work first, then do your part (delegation).** Work
   the user's goal needs that can run without you (a review, a brainstorm of
   edge cases, a critique, research, a summary, an ingest) goes to a Vornik
   workflow before you start your own part. Do it yourself instead when it
   is quicker than a delegation round trip, when it depends on what only
   this session holds (a running process, a decision not yet written down),
   or when the user asked you not to delegate. A question only the user can
   answer goes to the user, never to a workflow.
2. **Never wait idle, and never delegate to stay busy (delegation).** While a
   task runs, do your own part; check results between your steps, not in a
   wait loop. Each task is paid from the project's budget: delegate only
   work the goal needs. Never send a secret, a credential or personal data
   to a workflow.
3. **Verify what comes back.** Treat a result as a capable junior
   colleague's: check it before you act on it, and say what you checked.
   Security-critical work (authentication, cryptography, payments,
   migrations, deletion) gets an adversarial check, not a read-through.
4. **Recall before delegating and before design work**, so Vornik is not
   paid to rediscover what its memory already holds. Treat an old memory as
   a hint to verify, not as the answer.
5. **Park side tasks.** A problem off the goal in hand is written down where
   the user keeps such things (their backlog or issue tracker, or
   `remember` when you have it) or handed to a parallel task; then return to
   the goal. A security, data-loss or production problem is never parked:
   tell the user now.
6. **Write the user's ideas down at once**, the same way, so a context reset
   does not lose them.
7. **Show the user what Vornik remembers when they ask.** Call
   `memory_list` (a page at a time, or with `query` to search) and show
   short ids, ages and sources, never their whole memory. To forget an
   item, call `memory_correct` with its id in `chunk_ids` and `reason`
   `forget_command`. To export everything, or to forget from their phone,
   give them `export_url`: it opens only on their paired approver device.
8. **Say where a running task is.** While a task runs, `status` (and
   `result` while `complete` is false) carries `progress`: tell the user
   "step 2 of 3 (curate)", or "running curate" when there is no index, and
   check again with `result` and `wait_seconds`. Do not call `task_logs`
   only to show progress. For a follow-up about something a read-only
   broker workflow fetched minutes ago (the agenda you just showed), pass
   `max_age_seconds` (up to 900) to `delegate` to reuse that answer; omit it
   when the user wants a fresh read or just changed a connection or
   credential.
