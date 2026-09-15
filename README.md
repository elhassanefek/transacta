# Transacta

A transactional processing platform with idempotency, atomic state
management, and reliable event delivery. Transacta is deliberately
domain-agnostic: it's built as a general-purpose **Transaction Log**
engine (double-entry accounts, transactions, and entries) rather than a
financial ledger specifically, so the same primitives apply to financial
transfers, order processing, inventory movement, credits/points systems,
or job/task state machines.

Transacta is multi-tenant, API-key-provisioned infrastructure (embedded
finance / banking-as-a-service style, the same architectural category as
Modern Treasury or Unit): every tenant's accounts, transactions, and
entries are fully isolated from every other tenant's. See
[`docs/adr/0004-multi-tenancy-boundary.md`](docs/adr/0004-multi-tenancy-boundary.md).

## Quickstart

```sh
cp docker/.env.example docker/.env
# edit docker/.env and set JWT_SECRET (e.g. `openssl rand -hex 32`)

make up          # postgres -> automatic migrations -> api, all via docker compose
make migrate-up  # re-run manually any time you need to (idempotent)

# create your first tenant (writes directly to the running Postgres container)
DB_HOST=localhost go run ./scripts/create_tenant -name "Acme Corp"
```

`make up` runs `docker compose up`: Postgres starts, a one-shot
`migrate` container applies every pending migration and exits, and only
once that completes successfully does the `api` container start (see
`depends_on: migrate: condition: service_completed_successfully` in
`docker/docker-compose.yml`). The API listens on `:8080`.

For local Go development against just the database (e.g. to run the
integration test suites below), use `make up-db` instead, which starts
Postgres alone.

## Running the tests

```sh
go test ./...                                    # fast unit tests, no Docker
go test -tags=integration -race ./...            # full suite against real Postgres (testcontainers-go; needs Docker running)
```

The integration suites spin up their own ephemeral Postgres containers
via testcontainers-go and apply the relevant migrations directly — they
don't depend on `make up-db` being running. This is also exactly what
CI's `test` job runs (`.github/workflows/ci.yml`).

## Architecture

See [`docs/architecture.md`](docs/architecture.md) for the folder
layout, dependency-direction rules, and an entity-relationship diagram
of the schema.

Design decisions with real tradeoffs are recorded as ADRs in
[`docs/adr/`](docs/adr/):

- [0001 — Transaction isolation and row-locking strategy](docs/adr/0001-transaction-isolation-and-locking.md)
- [0002 — Transactional outbox for event delivery](docs/adr/0002-transactional-outbox-for-event-delivery.md)
- [0003 — Idempotency fencing token](docs/adr/0003-idempotency-fencing-token.md)
- [0004 — Multi-tenancy as the unit of data isolation](docs/adr/0004-multi-tenancy-boundary.md)

## API

See [`docs/API.md`](docs/API.md) for the full endpoint reference
(request/response shapes, required permissions, error codes). Briefly:

| Endpoint | Auth | Purpose |
|---|---|---|
| `POST /v1/auth/register` | tenant API key (`X-API-Key`) | create a user within the calling tenant |
| `POST /v1/auth/login` | none (credential is the body) | issue an access + refresh token pair |
| `POST /v1/auth/refresh` | none (credential is the body) | rotate a refresh token |
| `POST /v1/auth/logout` | none (credential is the body) | revoke a refresh token |
| `POST /v1/accounts` | JWT, `accounts:write` | create an account |
| `GET /v1/accounts/{id}` | JWT, `accounts:read` | read an account and its derived balance |
| `POST /v1/transfers` | JWT, `transactions:write`, idempotency key | execute an atomic, immediately-posted transfer |
| `POST /v1/transactions/pending` | JWT, `transactions:write`, idempotency key | create a pending (two-phase) transaction |
| `POST /v1/transactions/{id}/post` | JWT, `transactions:write`, idempotency key | post a pending transaction |
| `POST /v1/transactions/{id}/fail` | JWT, `transactions:write`, idempotency key | fail a pending transaction |
| `GET /v1/transactions/{id}` | JWT, `transactions:read` | read a transaction and its entries |
| `GET /v1/audit-log` | JWT, `audit:read` (admin by default) | list the tenant's audit trail, newest first |
| `GET /healthz` | none | liveness/readiness (pings the database) |
| `GET /metrics` | none (see note below) | Prometheus metrics |

Every mutating endpoint requires an `Idempotency-Key` header; retried
requests with the same key and the same request body/method/path replay
the original cached response instead of re-executing (see ADR-0003).

`/metrics` is deliberately unauthenticated (Prometheus scrapers don't
carry a bearer token by default) — restrict it at the network layer
(internal-only ingress, firewall rule) in any real deployment.

## Feature coverage against the Cahier des Charges

| Milestone | Status |
|---|---|
| M1 — Schema & migrations | Done — `migrations/` |
| M2 — Transaction engine | Done — `internal/ledger`, concurrency-tested |
| M3 — Idempotency engine | Done — `internal/middleware/idempotency` |
| M4 — Auth & RBAC | Done — `internal/auth`, `internal/middleware/auth` |
| M5 — Event delivery | Done — `internal/webhook` (outbox mechanics factored out into `internal/queue`) |
| M6 — Security & observability | Done — HMAC signing, structured logging, Prometheus metrics, `/healthz`, panic recovery, audit trail |
| M7 — Docker Compose & automatic migrations | Done |
| M8 — Documentation | Done — this file, `docs/API.md`, `docs/architecture.md`, `docs/adr/` |

### Known limitations (v1)

- **User management beyond registration is not implemented.** The
  `users:manage` permission is seeded (see
  `migrations/000003_users_auth.up.sql`) but nothing currently checks
  it — there's no list/disable-user endpoint yet. Users are created via
  `POST /v1/auth/register` and otherwise unmanageable through the API.
- **No per-tenant rate limiting.** Amendment 1 to the Cahier des
  Charges names this as valuable future work, not a v1 requirement.
- **Cross-tenant transactions, platform-operator (cross-tenant) admin
  tooling, and tenant self-service onboarding** are explicitly out of
  scope for v1 per Amendment 1 §5. Tenant creation is the manual/
  operational `scripts/create_tenant` CLI.
