package idempotency

import (
	"time"

	"github.com/google/uuid"
)


type Status string

const (
	StatusProcessing Status = "processing"
	StatusCompleted  Status = "completed"
)


type Record struct {
	ID           uuid.UUID
	TenantID     uuid.UUID
	Key          string
	RequestHash  string
	Status       Status
	ResponseCode *int   // nil until Status == StatusCompleted
	ResponseBody []byte // nil until Status == StatusCompleted; raw JSON bytes
	CreatedAt    time.Time
	ExpiresAt    time.Time
}
