# Design: retry budget for the export worker

Status: DRAFT

## Problem

The export worker retries a failed upload forever. A permanently rejected
payload (bad credentials, malformed row) therefore occupies a worker slot
indefinitely, and three such payloads have starved the pool in production.

## Proposal

Give each upload a budget of 3 attempts with exponential backoff (1s, 4s, 16s).
On exhaustion the payload moves to a dead-letter table with the last error, and
the worker slot is released.

## Open question

Whether the dead-letter table should be retried automatically on the next
deploy, or only by an operator. Automatic retry risks replaying a payload the
downstream system has since rejected for a different reason.
