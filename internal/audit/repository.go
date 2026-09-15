// Package audit persists the platform's immutable audit trail: one row
// per mutating action, written inside the same database transaction as
// the state change it reports. Every other domain package (ledger,
// auth) depends on this only through a small interface it defines for
// itself -- see internal/ledger/audit.go and internal/auth/audit.go --
// the same dependency-direction rule already used for webhook event
// emission (internal/ledger/events.go): core/outward-facing packages
// never import each other directly, they only emit through interfaces
// their consumers implement structurally.
package audit

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
)

type DBTX interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

var ErrEntryNotFound = errors.New("audit: entry not found")

type Repository struct {
	db *sql.DB
}

func NewRepository(db *sql.DB) *Repository {
	return &Repository{db: db}
}

// RecordAudit appends one audit_log row inside tx, the caller's own
// in-flight transaction -- so "the state change happened" and "an audit
// record of it exists" commit or roll back together, never as two
// independent writes that could disagree.
func (r *Repository) RecordAudit(ctx context.Context, tx *sql.Tx, tenantID uuid.UUID, actorID, action string, payload json.RawMessage) error {
	const query = `
		INSERT INTO audit_log (tenant_id, actor_id, action, payload)
		VALUES ($1, $2, $3, $4)`
	if _, err := tx.ExecContext(ctx, query, tenantID, actorID, action, []byte(payload)); err != nil {
		return fmt.Errorf("audit: record: %w", err)
	}
	return nil
}

// List returns a tenant's audit entries newest-first, for an eventual
// GET /audit-log endpoint or operator tooling. limit is capped at 200 to
// keep a single request bounded.
func (r *Repository) List(ctx context.Context, tenantID uuid.UUID, limit int) ([]*Entry, error) {
	if limit <= 0 || limit > 200 {
		limit = 200
	}
	const query = `
		SELECT id, tenant_id, actor_id, action, payload, created_at
		FROM audit_log
		WHERE tenant_id = $1
		ORDER BY created_at DESC
		LIMIT $2`
	rows, err := r.db.QueryContext(ctx, query, tenantID, limit)
	if err != nil {
		return nil, fmt.Errorf("audit: list: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var entries []*Entry
	for rows.Next() {
		var e Entry
		var payload []byte
		if err := rows.Scan(&e.ID, &e.TenantID, &e.ActorID, &e.Action, &payload, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("audit: scan entry: %w", err)
		}
		e.Payload = payload
		entries = append(entries, &e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("audit: list: %w", err)
	}
	return entries, nil
}
