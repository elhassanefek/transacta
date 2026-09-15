// Package queue is a Postgres-backed job queue: the storage mechanics
// behind the transactional outbox pattern (claim-with-FOR-UPDATE-SKIP-
// LOCKED, retry scheduling, dead-lettering), independent of what a job
// actually does once claimed. internal/webhook is its first and
// currently only caller -- it configures a Repository over the
// webhook_events/dead_letter_events tables and layers HTTP delivery,
// HMAC signing, and backoff policy on top -- but nothing here knows
// about webhooks specifically, so a second outbox-shaped queue (e.g. for
// a future notification channel) can reuse this package against its own
// pair of tables rather than re-implementing claim/retry/dead-letter
// logic from scratch.
package queue

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// Status is a job's position in the queue lifecycle. The value set is
// fixed by the queue and dead-letter tables' own CHECK constraints
// (originally defined for webhook delivery outcomes), which is why
// "delivered" -- not a more generic "completed" -- is the terminal
// success state.
type Status string

const (
	StatusPending    Status = "pending"
	StatusProcessing Status = "processing"
	StatusDelivered  Status = "delivered"
	StatusFailed     Status = "failed"
)

// Job is one row of the queue table.
type Job struct {
	ID           uuid.UUID
	TenantID     uuid.UUID
	JobType      string
	Payload      json.RawMessage
	Status       Status
	AttemptCount int
	NextRetryAt  time.Time
	CreatedAt    time.Time
	LastError    *string
}

// DeadLetterJob is one row of the dead-letter table: a job that
// exhausted every retry attempt, kept for inspection/replay tooling
// rather than deleted.
type DeadLetterJob struct {
	ID            uuid.UUID
	TenantID      uuid.UUID
	OriginalJobID uuid.UUID
	JobType       string
	Payload       json.RawMessage
	AttemptCount  int
	LastError     *string
	FailedAt      time.Time
}
