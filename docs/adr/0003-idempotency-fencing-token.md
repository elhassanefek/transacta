# ADR-0003: Idempotency fencing token

## Status

Accepted.

## Context

The idempotency middleware (`internal/middleware/idempotency`) lets a
client retry a request safely: `ClaimOrGet` inserts a `processing`
record (or, if one already exists and hasn't expired, returns it instead
of re-running the handler); once the handler finishes, `Complete` writes
the response and flips the record to `completed` so future retries can
replay it.

Between those two calls there's a real race, not a hypothetical one: a
claim has a lease (`DefaultProcessingLease = 30s`). If the server holding
a claim stalls or crashes before completing it, another server is
allowed to reclaim the same key once the lease expires and start
processing again. If the *first* server is merely slow rather than
actually dead, it can still be running when the second server reclaims,
finishes, and writes its result. When the first server finally finishes
its own (now-stale) attempt, a naive `UPDATE … SET response_body = ?
WHERE tenant_id = ? AND key = ?` would silently overwrite the second
server's already-cached, already-returned-to-a-client result with the
first server's stale one — a lost update on the idempotency cache
itself, defeating the entire guarantee the middleware exists to provide.

## Decision

`ClaimOrGet` returns each claim's `created_at` as a fencing token.
`Complete` requires that token as its final argument and its `UPDATE`
carries `WHERE tenant_id = ? AND key = ? AND created_at = ?` — not just
key equality. Reclaiming a key after lease expiry always assigns it a
*new* `created_at` (the reclaim query's `ON CONFLICT … DO UPDATE SET
created_at = EXCLUDED.created_at`), so a stale server's `Complete` call,
carrying the old `created_at`, matches zero rows. `RowsAffected() == 0`
is translated to `ErrClaimSuperseded`
(`internal/middleware/idempotency/repository.go`), which the middleware
treats as "discard this result, someone else already won" rather than an
error to retry or surface — see `completeWithRetry` in `middleware.go`.

`TestComplete_FencingTokenPreventsStaleWriteFromClobberingNewerClaim`
(unit, fake store) and its `_Integration` counterpart (real Postgres)
both simulate exactly this sequence — claim, expire, reclaim, stale
server completes second — and assert the stale write is refused and the
newer result survives untouched.

## Consequences

- `Complete` must always be called with the `created_at` from the
  `ClaimOrGet` (or reclaim) that authorized the work being completed,
  never a value looked up fresh — that's the whole mechanism.
- A response cached this way is stored as `TEXT`, not `JSONB` (see
  `migrations/000007_idempotency_response_body_text.up.sql` and
  `docs/architecture.md`) — `JSONB`'s write-time canonicalization would
  return a byte-for-byte different body on replay than what the
  original handler actually produced, undermining "identical repeated
  requests return the original cached response" even in the
  non-race-condition case.
