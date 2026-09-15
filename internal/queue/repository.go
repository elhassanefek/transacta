package queue

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/google/uuid"
)

type DBTX interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

var ErrJobNotFound = errors.New("queue: job not found")

// validIdentifier matches a plain lowercase SQL identifier -- the only
// shape queueTable/deadLetterTable are ever allowed to take.
var validIdentifier = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

// Repository is a Postgres-backed job queue over two caller-specified
// tables: a queue table (pending/processing/delivered/failed rows,
// shaped like webhook_events) and a dead-letter table (shaped like
// dead_letter_events). Table names are validated against
// validIdentifier and are only ever supplied by Go code at construction
// time -- e.g. webhook.NewRepository's call to queue.NewRepository --
// never derived from request input, so building queries with fmt.Sprintf
// around them does not introduce a SQL-injection path.
type Repository struct {
	db              *sql.DB
	queueTable      string
	deadLetterTable string
}

// NewRepository builds a queue Repository over the given table pair. It
// panics if either name isn't a plain SQL identifier -- a programming
// error caught at startup, not something that can vary per request.
func NewRepository(db *sql.DB, queueTable, deadLetterTable string) *Repository {
	if !validIdentifier.MatchString(queueTable) {
		panic(fmt.Sprintf("queue: invalid queue table name %q", queueTable))
	}
	if !validIdentifier.MatchString(deadLetterTable) {
		panic(fmt.Sprintf("queue: invalid dead-letter table name %q", deadLetterTable))
	}
	return &Repository{db: db, queueTable: queueTable, deadLetterTable: deadLetterTable}
}

func (r *Repository) DB() *sql.DB { return r.db }

func (r *Repository) BeginTx(ctx context.Context, opts *sql.TxOptions) (*sql.Tx, error) {
	return r.db.BeginTx(ctx, opts)
}

// Enqueue inserts a new pending job, due immediately. Called with q as
// the caller's own in-flight *sql.Tx, this is what makes the
// transactional outbox pattern work: the job row commits atomically with
// whatever state change it reports.
func (r *Repository) Enqueue(ctx context.Context, q DBTX, tenantID uuid.UUID, jobType string, payload json.RawMessage) (*Job, error) {
	job := &Job{TenantID: tenantID, JobType: jobType, Payload: payload, Status: StatusPending}
	query := fmt.Sprintf(`
		INSERT INTO %s (tenant_id, event_type, payload, status, attempt_count, next_retry_at)
		VALUES ($1, $2, $3, 'pending', 0, now())
		RETURNING id, attempt_count, next_retry_at, created_at`, r.queueTable)
	err := q.QueryRowContext(ctx, query, tenantID, jobType, []byte(payload)).
		Scan(&job.ID, &job.AttemptCount, &job.NextRetryAt, &job.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("queue: enqueue: %w", err)
	}
	return job, nil
}

// ClaimPending atomically claims up to limit due jobs by flipping them to
// 'processing' and returning the claimed rows. FOR UPDATE SKIP LOCKED
// means concurrent workers/pollers never block on or double-claim the
// same row. Callers are expected to commit tx immediately after this
// call -- see internal/webhook/worker.go's processBatch for why the
// claim transaction must not stay open for the duration of any slow work
// done with the claimed jobs.
func (r *Repository) ClaimPending(ctx context.Context, tx *sql.Tx, limit int) ([]*Job, error) {
	query := fmt.Sprintf(`
		UPDATE %[1]s
		SET status = 'processing'
		WHERE id IN (
			SELECT id FROM %[1]s
			WHERE status = 'pending' AND next_retry_at <= now()
			ORDER BY next_retry_at
			LIMIT $1
			FOR UPDATE SKIP LOCKED
		)
		RETURNING id, tenant_id, event_type, payload, status, attempt_count, next_retry_at, created_at, last_error`, r.queueTable)
	rows, err := tx.QueryContext(ctx, query, limit)
	if err != nil {
		return nil, fmt.Errorf("queue: claim pending: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var jobs []*Job
	for rows.Next() {
		job, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("queue: claim pending: %w", err)
	}
	return jobs, nil
}

// Get reads a single job, tenant-scoped.
func (r *Repository) Get(ctx context.Context, q DBTX, tenantID, jobID uuid.UUID) (*Job, error) {
	query := fmt.Sprintf(`
		SELECT id, tenant_id, event_type, payload, status, attempt_count, next_retry_at, created_at, last_error
		FROM %s WHERE id = $1 AND tenant_id = $2`, r.queueTable)
	job, err := scanJobRow(q.QueryRowContext(ctx, query, jobID, tenantID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrJobNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("queue: get: %w", err)
	}
	return job, nil
}

// MarkDelivered marks a successfully-processed job terminal.
func (r *Repository) MarkDelivered(ctx context.Context, q DBTX, jobID uuid.UUID) error {
	query := fmt.Sprintf(`UPDATE %s SET status = 'delivered' WHERE id = $1`, r.queueTable)
	if _, err := q.ExecContext(ctx, query, jobID); err != nil {
		return fmt.Errorf("queue: mark delivered: %w", err)
	}
	return nil
}

// ScheduleRetry puts a job back in 'pending' with an advanced
// next_retry_at, recording why the previous attempt failed.
func (r *Repository) ScheduleRetry(ctx context.Context, q DBTX, jobID uuid.UUID, attemptCount int, nextRetryAt time.Time, lastError string) error {
	query := fmt.Sprintf(`
		UPDATE %s
		SET status = 'pending', attempt_count = $2, next_retry_at = $3, last_error = $4
		WHERE id = $1`, r.queueTable)
	if _, err := q.ExecContext(ctx, query, jobID, attemptCount, nextRetryAt, lastError); err != nil {
		return fmt.Errorf("queue: schedule retry: %w", err)
	}
	return nil
}

// MoveToDeadLetter records job in the dead-letter table and marks the
// original row 'failed', within the caller's transaction so both writes
// commit or roll back together.
func (r *Repository) MoveToDeadLetter(ctx context.Context, tx *sql.Tx, job *Job, lastError string) error {
	insertDL := fmt.Sprintf(`
		INSERT INTO %s (tenant_id, original_event_id, event_type, payload, attempt_count, last_error)
		VALUES ($1, $2, $3, $4, $5, $6)`, r.deadLetterTable)
	if _, err := tx.ExecContext(ctx, insertDL,
		job.TenantID, job.ID, job.JobType, []byte(job.Payload), job.AttemptCount, lastError,
	); err != nil {
		return fmt.Errorf("queue: insert dead letter job: %w", err)
	}

	updateOriginal := fmt.Sprintf(`UPDATE %s SET status = 'failed', last_error = $2 WHERE id = $1`, r.queueTable)
	if _, err := tx.ExecContext(ctx, updateOriginal, job.ID, lastError); err != nil {
		return fmt.Errorf("queue: mark original job failed: %w", err)
	}
	return nil
}

func scanJob(rows *sql.Rows) (*Job, error) {
	var job Job
	var payload []byte
	var lastError sql.NullString
	if err := rows.Scan(
		&job.ID, &job.TenantID, &job.JobType, &payload, &job.Status,
		&job.AttemptCount, &job.NextRetryAt, &job.CreatedAt, &lastError,
	); err != nil {
		return nil, fmt.Errorf("queue: scan job: %w", err)
	}
	job.Payload = payload
	if lastError.Valid {
		job.LastError = &lastError.String
	}
	return &job, nil
}

func scanJobRow(row *sql.Row) (*Job, error) {
	var job Job
	var payload []byte
	var lastError sql.NullString
	err := row.Scan(
		&job.ID, &job.TenantID, &job.JobType, &payload, &job.Status,
		&job.AttemptCount, &job.NextRetryAt, &job.CreatedAt, &lastError,
	)
	if err != nil {
		return nil, err // sql.ErrNoRows bubbles up unwrapped for the caller to translate
	}
	job.Payload = payload
	if lastError.Valid {
		job.LastError = &lastError.String
	}
	return &job, nil
}
