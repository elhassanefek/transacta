# ADR-0004: Multi-tenancy as the unit of data isolation

## Status

Accepted — implements Amendment 1 to the Cahier des Charges.

## Context

Amendment 1 repositions Transacta from a single-organization internal
ledger to Ledger-as-a-Service infrastructure: multiple independent
businesses integrate against the same running platform, each with fully
isolated accounts, transactions, and entries. Amendment 1 §2.2 states
the requirement directly: "No query, in any code path, may return or
aggregate data across tenants," and the NFR table (§4) requires this
enforced "at the query level in every repository method, not only at the
API/auth boundary."

## Decision

`tenant_id` is a `NOT NULL` column on every tenant-scoped table
(`users`, `accounts`, `transactions`, `entries`, `idempotency_keys`,
`webhook_events`, `dead_letter_events`, `audit_log`), and every
repository method that reads or writes one of those tables takes a
`tenantID` and includes it in the `WHERE` clause — see, for example,
`ledger.Repository.GetAccount`, `GetTransaction`, and
`LockAccountsForUpdate` all filtering on `tenant_id = $N`, or
`auth.Repository.GetUserByEmail`/`GetUserByID` doing the same. A row
belonging to another tenant is not filtered out after the fact; the
query never returns it in the first place, and a lookup for an ID that
exists but belongs to a different tenant returns the same "not found"
error as an ID that doesn't exist at all — no side channel exists for
probing whether a given ID belongs to *some* tenant.

Two additional structural choices reinforce single-tenant scope end to
end:

- `users.tenant_id` is part of a composite unique constraint
  (`uq_users_id_tenant`), which is what lets `refresh_tokens` carry a
  composite foreign key `(user_id, tenant_id) REFERENCES users(id,
  tenant_id)` — a refresh token can only ever reference a user within
  its own declared tenant at the schema level, not just by convention in
  application code.
- The JWT's own claims carry `tenant_id`
  (`auth.Claims.TenantID`), and `middleware/auth.Middleware` puts it in
  request context (`tenants.WithTenantID`) once, immediately after
  validating the token signature — every downstream handler and service
  call reads that one context value rather than trusting a
  client-supplied tenant ID anywhere in a request body or query string
  for an already-authenticated route. (The one exception,
  `POST /auth/register`, has no JWT yet by definition — its tenant
  scope instead comes from resolving the `X-API-Key` header via
  `requireTenantAPIKey`, which is exactly the credential Amendment 1
  §2.2 defines a tenant by.)

Cross-tenant value movement is explicitly out of scope for v1 (Amendment
1 §5): no code path constructs a transaction whose entries reference
accounts from more than one tenant, and `ledger.Service`'s
`LockAccountsForUpdate` scopes its own lookup to the single `tenantID`
the caller passed, so even a malformed request couldn't cause it to lock
or read a row it wasn't authorized to touch.

## Consequences

- Every new repository method added to this codebase must take a
  `tenantID` parameter and use it in the query, by convention rather
  than a framework-enforced guarantee — there's no global query
  interceptor doing this automatically. `TestRepository_*` tests across
  packages assert not-found behavior for cross-tenant lookups
  specifically to guard against a future regression here.
- Platform-operator (cross-tenant) tooling and tenant self-service
  onboarding remain unimplemented by design (Amendment 1 §5); the only
  cross-tenant-capable code path in the entire system is the operator-run
  `scripts/create_tenant` CLI, which talks to Postgres directly rather
  than through the tenant-scoped API.
