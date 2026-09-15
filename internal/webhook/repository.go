package webhook

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/elhassanefek/transacta/internal/queue"
)

type DBTX interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Repository is webhook delivery's persistence layer. The outbox/retry/
// dead-letter mechanics it exposes (Enqueue, ClaimPendingEvents,
// MarkDelivered, ScheduleRetry, MoveToDeadLetter) are thin adapters over
// internal/queue's generic Postgres-backed job queue, configured here
// against the webhook_events/dead_letter_events tables -- webhook.Event
// is queue.Job under a name this package's callers already expect.
// GetTenantWebhookConfig, by contrast, is genuinely webhook-specific (no
// generic queue concept of "delivery endpoint") and talks to the
// tenants table directly.
type Repository struct {
	db    *sql.DB
	queue *queue.Repository
}

func NewRepository(db *sql.DB) *Repository {
	return &Repository{
		db:    db,
		queue: queue.NewRepository(db, "webhook_events", "dead_letter_events"),
	}
}

func (r *Repository) BeginTx(ctx context.Context, opts *sql.TxOptions) (*sql.Tx, error) {
	return r.db.BeginTx(ctx, opts)
}

func (r *Repository) Enqueue(ctx context.Context, q DBTX, tenantID uuid.UUID, eventType string, payload json.RawMessage) (*Event, error) {
	job, err := r.queue.Enqueue(ctx, q, tenantID, eventType, payload)
	if err != nil {
		return nil, fmt.Errorf("webhook: enqueue: %w", err)
	}
	return eventFromJob(job), nil
}

func (r *Repository) ClaimPendingEvents(ctx context.Context, tx *sql.Tx, limit int) ([]*Event, error) {
	jobs, err := r.queue.ClaimPending(ctx, tx, limit)
	if err != nil {
		return nil, fmt.Errorf("webhook: claim pending events: %w", err)
	}
	events := make([]*Event, 0, len(jobs))
	for _, job := range jobs {
		events = append(events, eventFromJob(job))
	}
	return events, nil
}

// GetEvent reads a single event, tenant-scoped.
func (r *Repository) GetEvent(ctx context.Context, q DBTX, tenantID, eventID uuid.UUID) (*Event, error) {
	job, err := r.queue.Get(ctx, q, tenantID, eventID)
	if errors.Is(err, queue.ErrJobNotFound) {
		return nil, ErrEventNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("webhook: get event: %w", err)
	}
	return eventFromJob(job), nil
}

// MarkDelivered marks a successfully-delivered event terminal.
func (r *Repository) MarkDelivered(ctx context.Context, q DBTX, eventID uuid.UUID) error {
	if err := r.queue.MarkDelivered(ctx, q, eventID); err != nil {
		return fmt.Errorf("webhook: mark delivered: %w", err)
	}
	return nil
}

func (r *Repository) ScheduleRetry(ctx context.Context, q DBTX, eventID uuid.UUID, attemptCount int, nextRetryAt time.Time, lastError string) error {
	if err := r.queue.ScheduleRetry(ctx, q, eventID, attemptCount, nextRetryAt, lastError); err != nil {
		return fmt.Errorf("webhook: schedule retry: %w", err)
	}
	return nil
}

func (r *Repository) MoveToDeadLetter(ctx context.Context, tx *sql.Tx, ev *Event, lastError string) error {
	if err := r.queue.MoveToDeadLetter(ctx, tx, jobFromEvent(ev), lastError); err != nil {
		return fmt.Errorf("webhook: move to dead letter: %w", err)
	}
	return nil
}

// EnqueueEvent satisfies ledger.EventEnqueuer -- see
// internal/ledger/events.go for why ledger depends on a structurally-
// matched interface it defines itself rather than importing this
// package.
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

func eventFromJob(job *queue.Job) *Event {
	return &Event{
		ID:           job.ID,
		TenantID:     job.TenantID,
		EventType:    job.JobType,
		Payload:      job.Payload,
		Status:       Status(job.Status),
		AttemptCount: job.AttemptCount,
		NextRetryAt:  job.NextRetryAt,
		CreatedAt:    job.CreatedAt,
		LastError:    job.LastError,
	}
}

func jobFromEvent(ev *Event) *queue.Job {
	return &queue.Job{
		ID:           ev.ID,
		TenantID:     ev.TenantID,
		JobType:      ev.EventType,
		Payload:      ev.Payload,
		Status:       queue.Status(ev.Status),
		AttemptCount: ev.AttemptCount,
		NextRetryAt:  ev.NextRetryAt,
		CreatedAt:    ev.CreatedAt,
		LastError:    ev.LastError,
	}
}
