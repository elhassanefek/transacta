# ADR-0001: Transaction isolation and row-locking strategy

## Status

Accepted.

## Context

The Cahier des Charges requires "Optimistic and pessimistic locking
strategies, applied per use case" and "Row-level locking / appropriate
isolation level, justified in an Architecture Decision Record" (Amendment
1's NFR table adds: "per operation"). A transfer or a pending-transaction
post must never let two concurrent operations both read a stale balance,
both decide funds are sufficient, and both commit — that's a classic
lost-update race, and it's exactly what `TestExecuteTransfer_ConcurrentTransfersNoLostUpdates`
and `TestPostPendingTransaction_StatusGuardConflict` in
`internal/ledger/integration_test.go` exist to catch.

## Decision

Every ledger write runs at Postgres's `READ COMMITTED` isolation level
(`sql.LevelReadCommitted`, the default anyway — set explicitly at every
`BeginTx` call site for legibility), combined with **explicit pessimistic
row locking** rather than relying on `SERIALIZABLE` isolation or an
optimistic version-column check:

- `LockAccountsForUpdate` (`internal/ledger/repository.go`) issues
  `SELECT … FOR UPDATE`, sorted by account ID before locking, against
  every account a transfer or a pending-transaction post touches. Sorting
  first is what prevents deadlock: two transfers that both touch accounts
  A and B always acquire their locks in the same order (A then B),
  regardless of which order the caller listed the entries in, so they
  can never form a lock-wait cycle.
- Once the locks are held, the balance projection (current balance +
  every entry about to be applied) is computed and checked for
  negativity *before* the new entries are inserted — a second
  transaction attempting to touch the same accounts blocks on the row
  lock until the first commits or rolls back, so it always sees the
  first transaction's effect (or its absence, on rollback), never a
  torn intermediate state.
- Transaction *status* transitions (`pending` → `posted`/`failed`) use a
  separate, lighter mechanism: `UpdateTransactionStatusGuarded` is a
  conditional `UPDATE … WHERE status = $expected`, checked via
  `RowsAffected()`. This is optimistic, not pessimistic — appropriate
  here because a transaction's own status is only ever written by the
  post/fail operation on that one transaction, so there's no analogous
  multi-row contention to serialize away, just a single racy
  read-then-write on one row that a conditional update resolves cheaply.

`SERIALIZABLE` isolation was considered and rejected for the account
locking path: it would catch the same races, but only by aborting one of
the two transactions with a serialization-failure error that the
application then has to detect and retry — an extra failure mode and
retry loop for no behavioral gain over locking the specific rows that
are actually contended.

## Consequences

- A transfer touching N accounts holds N row locks for the duration of
  its transaction. This is intentional and bounded: entries are capped
  at whatever a single transfer request contains, never grown by
  background processing, so lock hold time stays short and predictable.
- Any future write path that touches `accounts` must go through
  `LockAccountsForUpdate` (or an equivalent sorted-lock pattern) if it
  needs a consistent balance read — a plain `SELECT` without `FOR
  UPDATE` is fine for read-only endpoints (`GetAccount`) precisely
  because they don't need to prevent a concurrent writer from moving the
  balance mid-read; they just return whatever's currently committed.
