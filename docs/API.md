# API Reference

Base path for every endpoint below except `/healthz` and `/metrics` is
`/v1`. All request and response bodies are JSON
(`Content-Type: application/json`).

## Authentication

Two distinct credentials are in play, deliberately never interchangeable:

- **Tenant API key** (`X-API-Key` header) proves "I'm allowed to
  provision users for this tenant." Used only by `POST
  /auth/register`. Generated once per tenant by the operator-run
  `scripts/create_tenant` CLI (shown once, stored only as a SHA-256
  hash — see `internal/tenants/repository.go`).
- **JWT access token** (`Authorization: Bearer <token>`) proves "I am
  this specific user." Obtained from `/auth/login` or `/auth/refresh`
  and required by every ledger endpoint. Short-lived (15 minutes by
  default — `auth.DefaultAccessTokenTTL`); refreshed via the paired
  refresh token (7 days by default, rotated on every use — see
  ADR-0004 is not about this, rotation logic lives in
  `internal/auth/service.go`'s `Refresh`).

A request with neither header on a protected route gets `401
Unauthorized`; a validly-authenticated request missing the required RBAC
permission gets `403 Forbidden` (see `internal/middleware/auth/permission.go`).

## Idempotency

Every mutating endpoint (`POST /accounts`, `POST /transfers`, `POST
/transactions/pending`, `POST /transactions/{id}/post`, `POST
/transactions/{id}/fail`) requires an `Idempotency-Key` header.

- First request with a given key: executes normally, then caches the
  response (status code + body) keyed by `(tenant_id, key)`.
- Retried request with the **same** key and an identical
  `(method, path, body)` hash: returns the cached response verbatim,
  with an added `Idempotency-Replayed: true` header. The handler does
  not run again.
- Retried request with the **same** key but a **different** body:
  `422 Unprocessable Entity` (`ErrKeyReused`) — reusing a key for a
  different request is a client bug, not something to silently allow.
- Concurrent request with the same key while the first is still in
  flight: `409 Conflict` (`ErrInFlight`).
- Missing header on a route that requires one: `400 Bad Request`.

Cached responses expire after 24h by default (`idempotency.DefaultTTL`).
See ADR-0003 for how concurrent claim/retry/completion races are
resolved without a stale write ever clobbering a newer one.

## RBAC permissions

Seeded roles (`migrations/000003_users_auth.up.sql`,
`migrations/000006_audit_read_permission.up.sql`):

| Permission | admin | service | viewer |
|---|---|---|---|
| `accounts:read` | ✓ | ✓ | ✓ |
| `accounts:write` | ✓ | ✓ | |
| `transactions:read` | ✓ | ✓ | ✓ |
| `transactions:write` | ✓ | ✓ | |
| `audit:read` | ✓ | | |
| `users:manage` | ✓ | | |

`users:manage` is seeded but not yet enforced by any endpoint — see the
README's Known Limitations.

---

## `POST /v1/auth/register`

**Auth:** `X-API-Key: <tenant API key>`

Request:
```json
{ "email": "alice@acme.com", "password": "hunter2", "role_name": "admin" }
```
`role_name` defaults to `"viewer"` if omitted.

Response `201 Created`:
```json
{ "user_id": "…", "email": "alice@acme.com" }
```

Errors: `400` invalid body / unknown role, `409` email already exists
within this tenant (case-insensitive), `401` missing/invalid API key.

## `POST /v1/auth/login`

**Auth:** none — the body itself is the credential.

Request:
```json
{ "tenant_id": "…", "email": "alice@acme.com", "password": "hunter2" }
```

Response `200 OK`:
```json
{
  "access_token": "…", "refresh_token": "…",
  "access_expires_at": "2026-01-01T00:15:00Z",
  "refresh_expires_at": "2026-01-08T00:00:00Z"
}
```

Errors: `401` wrong password or unknown email (both return the same
error and take the same amount of time — a fixed-cost bcrypt comparison
runs either way — to resist user enumeration), `403` user disabled.

## `POST /v1/auth/refresh`

**Auth:** none — the refresh token itself is the credential.

Request: `{ "refresh_token": "…" }`. Response: same shape as `/login`.

Rotation: every refresh consumes the old token and issues a new one;
the old token's status flips to `rotated`. Presenting an
already-rotated or revoked token is treated as **compromise** — it
revokes every active refresh token for that user, not just the one
presented, then returns `401`.

## `POST /v1/auth/logout`

**Auth:** none — the refresh token itself is the credential.

Request: `{ "refresh_token": "…" }`. Response: `204 No Content`. Revokes
that one refresh token.

## `POST /v1/accounts`

**Auth:** JWT, `accounts:write`. **Idempotent.**

Request: `{ "name": "Alice's wallet" }`

Response `201 Created`:
```json
{ "id": "…", "name": "Alice's wallet", "balance_minor": 0 }
```

## `GET /v1/accounts/{id}`

**Auth:** JWT, `accounts:read`.

Response `200 OK`: same shape as create, with `balance_minor` computed
as the sum of every `posted` entry against this account (see ADR-0001).

Errors: `404` if the account doesn't exist in the caller's tenant
(accounts belonging to other tenants are indistinguishable from
nonexistent ones — see ADR-0004).

## `POST /v1/transfers`

**Auth:** JWT, `transactions:write`. **Idempotent.**

Executes an atomic, immediately-`posted` double-entry transfer. Entries
must sum to zero and every account must end with a non-negative
balance, or the whole transaction is rejected — nothing partially
applies.

Request:
```json
{
  "entries": [
    { "account_id": "…", "amount_minor": -500 },
    { "account_id": "…", "amount_minor": 500 }
  ]
}
```

Response `201 Created`: `{ "transaction_id": "…", "status": "posted" }`

Errors: `400` empty/unbalanced/zero-amount entries, `404` unknown
account, `409` insufficient funds on any leg.

## `POST /v1/transactions/pending`

**Auth:** JWT, `transactions:write`. **Idempotent.**

Same request shape as `/transfers`, but creates the transaction in
`pending` status without checking balances or applying it yet — the
first phase of a two-phase (hold-then-capture) transaction.

Response `201 Created`: `{ "transaction_id": "…", "status": "pending" }`

## `POST /v1/transactions/{id}/post`

**Auth:** JWT, `transactions:write`. **Idempotent.**

Re-validates balances (accounts may have moved since the transaction
was created pending) and transitions `pending` → `posted` under a
guarded compare-and-swap (see ADR-0001). Response/errors: same shape as
`/transfers`, plus `409` if the transaction isn't currently `pending`
(already posted/failed, or two concurrent posts racing).

## `POST /v1/transactions/{id}/fail`

**Auth:** JWT, `transactions:write`. **Idempotent.**

Transitions `pending` → `failed`. No balance effect (a failed
transaction was never applied). Same guarded compare-and-swap and `409`
semantics as `/post`.

## `GET /v1/transactions/{id}`

**Auth:** JWT, `transactions:read`.

Response `200 OK`:
```json
{
  "id": "…", "status": "posted",
  "entries": [
    { "account_id": "…", "amount_minor": -500 },
    { "account_id": "…", "amount_minor": 500 }
  ]
}
```

## `GET /v1/audit-log`

**Auth:** JWT, `audit:read` (admin only by default).

Query params: `limit` (default 50, capped at 200).

Response `200 OK`:
```json
{
  "entries": [
    {
      "id": "…", "action": "transaction.transferred", "actor_id": "…",
      "payload": { "transaction_id": "…", "status": "posted", "entries": [ /* … */ ] },
      "created_at": "2026-01-01T00:00:00Z"
    }
  ]
}
```

`action` is one of: `account.created`, `transaction.transferred`,
`transaction.pending_created`, `transaction.posted`,
`transaction.failed` (from `internal/ledger`), or `user.registered`,
`user.login`, `user.token_refreshed`, `user.logout` (from
`internal/auth`). `actor_id` is the authenticated user's ID, or
`"api-key"` for `user.registered` (there's no user session yet at the
moment one is being created — see `internal/auth/audit.go`).

## `GET /healthz`

**Auth:** none. `200 {"status":"ok"}` if the database is reachable,
`503 {"status":"unhealthy"}` otherwise.

## `GET /metrics`

**Auth:** none (see the README's note on restricting this at the
network layer in production). Standard Prometheus text exposition
format. Key series: `transacta_http_requests_total`,
`transacta_http_request_duration_seconds`,
`transacta_webhook_delivered_total`, `transacta_webhook_retried_total`,
`transacta_webhook_dead_lettered_total`,
`transacta_webhook_skipped_no_endpoint_total`.

## Outbound webhooks

Not an inbound endpoint, but part of the API surface: if a tenant's
`webhook_url` is configured (currently a manual/operational DB update —
see `migrations/000004_webhook_config.up.sql` for the column), Transacta
POSTs every ledger lifecycle event there:

- `transaction.posted`, `transaction.pending`, `transaction.failed`
  (see `internal/ledger/events.go`)

Each delivery carries:
```
Content-Type: application/json
X-Transacta-Event-Type: transaction.posted
X-Transacta-Timestamp: 1735689600
X-Transacta-Signature: sha256=<hex HMAC-SHA256 of "{timestamp}.{body}", keyed by the tenant's webhook_secret>
```

Verify with `internal/webhook.VerifySignature` (Go) or, in any other
language, recompute `HMAC-SHA256(secret, "{timestamp}.{raw body}")` and
compare in constant time. Non-2xx responses (or no response) trigger
retry with exponential backoff + jitter (base 30s, cap 1h, 5 attempts by
default); events that exhaust every attempt land in
`dead_letter_events` for inspection.
