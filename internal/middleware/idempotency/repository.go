package idempotency

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)


type Repository struct {
	db *sql.DB
}

func NewRepository(db *sql.DB) *Repository {
	return &Repository{db: db}
}


func (r *Repository) ClaimOrGet(ctx context.Context, tenantID uuid.UUID, key, requestHash string, leaseTTL time.Duration) (*Record, bool, error) {
	now := time.Now().UTC()
	expiresAt := now.Add(leaseTTL)

	const claimQuery = `
		INSERT INTO idempotency_keys (tenant_id, key, request_hash, status, created_at, expires_at)
		VALUES ($1, $2, $3, 'processing', $4, $5)
		ON CONFLICT (tenant_id, key) DO UPDATE
		SET request_hash   = EXCLUDED.request_hash,
		    status         = 'processing',
		    response_code  = NULL,
		    response_body  = NULL,
		    created_at     = EXCLUDED.created_at,
		    expires_at     = EXCLUDED.expires_at
		WHERE idempotency_keys.expires_at < $4
		RETURNING id, tenant_id, key, request_hash, status, response_code, response_body, created_at, expires_at`

	rec, err := scanRecord(r.db.QueryRowContext(ctx, claimQuery, tenantID, key, requestHash, now, expiresAt))
	if err == nil {
		return rec, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, false, fmt.Errorf("idempotency: claim: %w", err)
	}

	// Zero rows back means a live (non-expired) record already exists and
	// the WHERE guard correctly refused to touch it. Fetch it so the
	// caller can decide what to do with it.
	existing, err := r.Get(ctx, tenantID, key)
	if err != nil {
		return nil, false, err
	}
	return existing, false, nil
}


func (r *Repository) Get(ctx context.Context, tenantID uuid.UUID, key string) (*Record, error) {
	const query = `
		SELECT id, tenant_id, key, request_hash, status, response_code, response_body, created_at, expires_at
		FROM idempotency_keys WHERE tenant_id = $1 AND key = $2`
	rec, err := scanRecord(r.db.QueryRowContext(ctx, query, tenantID, key))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("idempotency: no record for key %q", key)
	}
	if err != nil {
		return nil, fmt.Errorf("idempotency: get: %w", err)
	}
	return rec, nil
}


func (r *Repository) Complete(ctx context.Context, tenantID uuid.UUID, key string, responseCode int, responseBody []byte, retentionTTL time.Duration) error {
	expiresAt := time.Now().UTC().Add(retentionTTL)
	const query = `
		UPDATE idempotency_keys
		SET status = 'completed', response_code = $3, response_body = $4, expires_at = $5
		WHERE tenant_id = $1 AND key = $2`
	if _, err := r.db.ExecContext(ctx, query, tenantID, key, responseCode, responseBody, expiresAt); err != nil {
		return fmt.Errorf("idempotency: complete: %w", err)
	}
	return nil
}

func scanRecord(row *sql.Row) (*Record, error) {
	var rec Record
	var responseCode sql.NullInt32
	var responseBody []byte
	err := row.Scan(
		&rec.ID, &rec.TenantID, &rec.Key, &rec.RequestHash, &rec.Status,
		&responseCode, &responseBody, &rec.CreatedAt, &rec.ExpiresAt,
	)
	if err != nil {
		return nil, err // sql.ErrNoRows deliberately bubbles up unwrapped for ClaimOrGet's conflict detection
	}
	if responseCode.Valid {
		v := int(responseCode.Int32)
		rec.ResponseCode = &v
	}
	rec.ResponseBody = responseBody
	return &rec, nil
}