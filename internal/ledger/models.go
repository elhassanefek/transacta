package ledger

import (
	"time"

	"github.com/google/uuid"
)

type Money int64

type Status string

const (
	StatusPending Status = "pending"
	StatusPosted  Status = "posted"
	StatusFailed  Status = "failed"
)

type Account struct {
	ID        uuid.UUID
	TenantID  uuid.UUID
	Name      string
	CreatedAt time.Time

	// Balance is derived (SUM of entries), populated by repository reads.

	Balance Money
}

type Transaction struct {
	ID        uuid.UUID
	TenantID  uuid.UUID
	Status    Status
	CreatedAt time.Time
}

type Entry struct {
	ID            uuid.UUID
	TenantID      uuid.UUID
	TransactionID uuid.UUID
	AccountID     uuid.UUID
	AmountMinor   Money
	CreatedAt     time.Time
}

type EntryInput struct {
	AccountID   uuid.UUID
	AmountMinor Money
}

func Sum(entries []EntryInput) Money {
	var total Money
	for _, e := range entries {
		total += e.AmountMinor
	}
	return total
}
