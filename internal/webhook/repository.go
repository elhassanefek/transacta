package webhook

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)


type DBTX interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

type Repository struct {
	db *sql.DB
}

func NewRepository(db *sql.DB) *Repository {
	return &Repository{db: db}
}

func (r *Repository) BeginTx(ctx context.Context, opts *sql.TxOptions) (*sql.Tx, error) {
	return r.db.BeginTx(ctx, opts)
}


func (r *Repository) Enqueue(ctx context.Context, q DBTX, tenantID uuid.UUID, eventType string, payload json.RawMessage) (*Event, error) {
	ev := &Event{TenantID: tenantID, EventType: eventType, Payload: payload, Status: StatusPending}
	const query = `
		INSERT INTO webhook_events (tenant_id, event_type, payload, status, attempt_count, next_retry_at)
		VALUES ($1, $2, $3, 'pending', 0, now())
		RETURNING id, attempt_count, next_retry_at, created_at`
	err := q.QueryRowContext(ctx, query, tenantID, eventType, []byte(payload)).
		Scan(&ev.ID, &ev.AttemptCount, &ev.NextRetryAt, &ev.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("webhook: enqueue: %w", err)
	}
	return ev, nil
}


func (r *Repository) ClaimPendingEvents(ctx context.Context, tx *sql.Tx, limit int) ([]*Event, error) {
	const query = `
		UPDATE webhook_events
		SET status = 'processing'
		WHERE id IN (
			SELECT id FROM webhook_events
			WHERE status = 'pending' AND next_retry_at <= now()
			ORDER BY next_retry_at
			LIMIT $1
			FOR UPDATE SKIP LOCKED
		)
		RETURNING id, tenant_id, event_type, payload, status, attempt_count, next_retry_at, created_at, last_error`
	rows, err := tx.QueryContext(ctx, query, limit)
	if err != nil {
		return nil, fmt.Errorf("webhook: claim pending events: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var events []*Event
	for rows.Next() {
		ev, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		events = append(events, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("webhook: claim pending events: %w", err)
	}
	return events, nil
}

// GetEvent reads a single event, tenant-scoped.
func (r *Repository) GetEvent(ctx context.Context, q DBTX, tenantID, eventID uuid.UUID) (*Event, error) {
	const query = `
		SELECT id, tenant_id, event_type, payload, status, attempt_count, next_retry_at, created_at, last_error
		FROM webhook_events WHERE id = $1 AND tenant_id = $2`
	ev, err := scanEventRow(q.QueryRowContext(ctx, query, eventID, tenantID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrEventNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("webhook: get event: %w", err)
	}
	return ev, nil
}

// MarkDelivered marks a successfully-delivered event terminal.
func (r *Repository) MarkDelivered(ctx context.Context, q DBTX, eventID uuid.UUID) error {
	const query = `UPDATE webhook_events SET status = 'delivered' WHERE id = $1`
	if _, err := q.ExecContext(ctx, query, eventID); err != nil {
		return fmt.Errorf("webhook: mark delivered: %w", err)
	}
	return nil
}

func (r *Repository) ScheduleRetry(ctx context.Context, q DBTX, eventID uuid.UUID, attemptCount int, nextRetryAt time.Time, lastError string) error {
	const query = `
		UPDATE webhook_events
		SET status = 'pending', attempt_count = $2, next_retry_at = $3, last_error = $4
		WHERE id = $1`
	if _, err := q.ExecContext(ctx, query, eventID, attemptCount, nextRetryAt, lastError); err != nil {
		return fmt.Errorf("webhook: schedule retry: %w", err)
	}
	return nil
}


func (r *Repository) MoveToDeadLetter(ctx context.Context, tx *sql.Tx, ev *Event, lastError string) error {
	const insertDL = `
		INSERT INTO dead_letter_events (tenant_id, original_event_id, event_type, payload, attempt_count, last_error)
		VALUES ($1, $2, $3, $4, $5, $6)`
	if _, err := tx.ExecContext(ctx, insertDL,
		ev.TenantID, ev.ID, ev.EventType, []byte(ev.Payload), ev.AttemptCount, lastError,
	); err != nil {
		return fmt.Errorf("webhook: insert dead letter event: %w", err)
	}

	const updateOriginal = `UPDATE webhook_events SET status = 'failed', last_error = $2 WHERE id = $1`
	if _, err := tx.ExecContext(ctx, updateOriginal, ev.ID, lastError); err != nil {
		return fmt.Errorf("webhook: mark original event failed: %w", err)
	}
	return nil
}


func (r *Repository) EnqueueEvent(ctx context.Context, tx *sql.Tx, tenantID uuid.UUID, eventType string, payload json.RawMessage) error {
	_, err := r.Enqueue(ctx, tx, tenantID, eventType, payload)
	return err
}


func (r *Repository) GetTenantWebhookConfig(ctx context.Context, q DBTX, tenantID uuid.UUID) (url, secret string, err error) {
	const query = `SELECT webhook_url, webhook_secret FROM tenants WHERE id = $1`
	var nullURL, nullSecret sql.NullString
	if scanErr := q.QueryRowContext(ctx, query, tenantID).Scan(&nullURL, &nullSecret); scanErr != nil {
		return "", "", fmt.Errorf("webhook: get tenant webhook config: %w", scanErr)
	}
	if !nullURL.Valid || nullURL.String == "" {
		return "", "", ErrNoEndpointConfigured
	}
	return nullURL.String, nullSecret.String, nil
}

func scanEvent(rows *sql.Rows) (*Event, error) {
	var ev Event
	var payload []byte
	var lastError sql.NullString
	if err := rows.Scan(
		&ev.ID, &ev.TenantID, &ev.EventType, &payload, &ev.Status,
		&ev.AttemptCount, &ev.NextRetryAt, &ev.CreatedAt, &lastError,
	); err != nil {
		return nil, fmt.Errorf("webhook: scan event: %w", err)
	}
	ev.Payload = payload
	if lastError.Valid {
		ev.LastError = &lastError.String
	}
	return &ev, nil
}

func scanEventRow(row *sql.Row) (*Event, error) {
	var ev Event
	var payload []byte
	var lastError sql.NullString
	err := row.Scan(
		&ev.ID, &ev.TenantID, &ev.EventType, &payload, &ev.Status,
		&ev.AttemptCount, &ev.NextRetryAt, &ev.CreatedAt, &lastError,
	)
	if err != nil {
		return nil, err // sql.ErrNoRows bubbles up unwrapped for GetEvent's caller to translate
	}
	ev.Payload = payload
	if lastError.Valid {
		ev.LastError = &lastError.String
	}
	return &ev, nil
}