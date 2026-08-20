package webhook

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)


type Status string

const (
	StatusPending    Status = "pending"
	StatusProcessing Status = "processing"
	StatusDelivered  Status = "delivered"
	StatusFailed     Status = "failed"
)


type Event struct {
	ID           uuid.UUID
	TenantID     uuid.UUID
	EventType    string
	Payload      json.RawMessage
	Status       Status
	AttemptCount int
	NextRetryAt  time.Time
	CreatedAt    time.Time
	LastError *string
}

type DeadLetterEvent struct {
	ID              uuid.UUID
	TenantID        uuid.UUID
	OriginalEventID uuid.UUID
	EventType       string
	Payload         json.RawMessage
	AttemptCount    int
	LastError       *string
	FailedAt        time.Time
}