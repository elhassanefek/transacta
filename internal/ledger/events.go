package ledger

import (
	"context"
	"database/sql"
	"encoding/json"

	"github.com/google/uuid"
)

// EventEnqueuer is the entire dependency this package has on event
// delivery. It's satisfied structurally by webhook.Repository's
// EnqueueEvent adapter method -- ledger never imports internal/webhook,
// per this project's architecture rule that core domain packages
// (transaction engine, idempotency) never import outward-facing
// packages (webhook, auth); they only emit events that those layers
// consume. The dependency arrow points from webhook toward ledger, not
// the reverse.
type EventEnqueuer interface {
	EnqueueEvent(ctx context.Context, tx *sql.Tx, tenantID uuid.UUID, eventType string, payload json.RawMessage) error
}

// Event type constants -- the vocabulary ledger emits. A receiver's
// integration is written against these strings, so changing one is a
// breaking change for every configured webhook consumer, same as
// renaming a public API field.
const (
	EventTransactionPosted  = "transaction.posted"
	EventTransactionPending = "transaction.pending"
	EventTransactionFailed  = "transaction.failed"
)

// transactionEventPayload is the JSON shape sent for every transaction
// lifecycle event. Deliberately minimal (id + status), matching the
// same "don't embed data that can go stale" reasoning as auth.Claims --
// a receiver that needs more detail calls back into the API for the
// full transaction (GET /transactions/{id}) rather than trusting
// whatever was true at the moment the event fired.
type transactionEventPayload struct {
	TransactionID string `json:"transaction_id"`
	Status        string `json:"status"`
}

func transactionEventJSON(txn *Transaction) (json.RawMessage, error) {
	return json.Marshal(transactionEventPayload{
		TransactionID: txn.ID.String(),
		Status:        string(txn.Status),
	})
}