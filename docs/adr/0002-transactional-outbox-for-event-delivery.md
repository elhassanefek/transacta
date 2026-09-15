# ADR-0002: Transactional outbox for event delivery

## Status

Accepted.

## Context

The Cahier des Charges requires "reliable webhook delivery" via a
"transactional outbox pattern — event rows written in the same DB
transaction as the state change, eliminating dual-write inconsistency."
The dual-write problem it's naming: if a transfer commits to Postgres
and *then* a separate call fires an HTTP webhook (or writes to a
separate queue system), there is no way to make those two actions atomic
across two different systems. A crash between them means either the
transfer happened with no event ever recorded, or — if the event write
happened first — an event exists for a transfer that never actually
committed.

## Decision

`ledger.Service`'s event emission (`emitEvent` in `service.go`) writes
the outbox row *inside the same `*sql.Tx`* as the transaction/entry
writes it's reporting, via the `EventEnqueuer` interface
(`internal/ledger/events.go`) that `webhook.Repository.EnqueueEvent`
satisfies structurally. If `EnqueueEvent`'s `INSERT` fails, the whole
transaction — the transfer itself, not just the event — rolls back; see
`emitEvent`'s doc comment in `service.go` for why this is the correct
tradeoff rather than "log and continue": letting the transfer succeed
while silently dropping its event would reintroduce exactly the
dual-write problem this pattern exists to eliminate. `ledger.AuditRecorder`
(`internal/ledger/audit.go`) and `auth.AuditRecorder`
(`internal/auth/audit.go`) follow the identical shape for the audit
trail, for the same reason.

Delivery itself is decoupled from the write path entirely: a background
worker (`internal/webhook/worker.go`) polls for due rows on a fixed
interval (`DefaultPollInterval = 5s`), claiming a batch with `FOR UPDATE
SKIP LOCKED` so multiple workers never double-claim the same row, then
releases that lock (commits the claim transaction) *before* attempting
any outbound HTTP call — holding a Postgres row lock open for the
duration of a network call to an arbitrary third party would turn a slow
or hanging webhook endpoint into a database contention problem, which
this design deliberately avoids (see `processBatch`'s doc comment).

The claim/retry/dead-letter storage mechanics are generic — see
`internal/queue` and `docs/architecture.md` — with `internal/webhook`
layering HTTP delivery, HMAC-SHA256 signing (`internal/webhook/signer.go`),
and exponential-backoff-with-jitter retry policy (base 30s, cap 1h, 5
attempts by default) on top.

## Consequences

- Webhook delivery latency is bounded below by the poll interval (up to
  5s after commit before the first attempt), never zero. This is an
  accepted tradeoff for the durability guarantee — an at-least-once,
  crash-safe delivery system needs a durable queue to poll, not a
  fire-and-forget in-memory dispatch that a crash between commit and
  send would lose entirely.
- Every event delivery outcome is at-least-once, not exactly-once — a
  receiver's webhook handler must be idempotent on its own end (e.g. by
  deduplicating on the event's `transaction_id` + `status`), since a
  worker crash after a successful HTTP call but before `MarkDelivered`
  commits would cause that event to be retried.
- An event that never obtains a working endpoint (tenant never
  configures `webhook_url`) or that exhausts `maxAttempts` does not
  block or slow down any other event — see ADR content in
  `internal/webhook/service.go`'s `ProcessEvent` for the
  `ErrNoEndpointConfigured` and dead-letter branches.
