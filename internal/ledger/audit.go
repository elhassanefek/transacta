package ledger

import (
	"context"
	"database/sql"
	"encoding/json"

	"github.com/google/uuid"
)

// AuditRecorder is the entire dependency this package has on the audit
// trail. Satisfied structurally by audit.Repository's RecordAudit method
// -- ledger never imports internal/audit, per this project's rule that
// core domain packages never import outward-facing packages; they only
// emit through an interface the consumer implements. Same shape and same
// reasoning as EventEnqueuer in events.go.
type AuditRecorder interface {
	RecordAudit(ctx context.Context, tx *sql.Tx, tenantID uuid.UUID, actorID, action string, payload json.RawMessage) error
}

// Audit action vocabulary this package emits.
const (
	AuditAccountCreated            = "account.created"
	AuditTransactionTransferred    = "transaction.transferred"
	AuditTransactionPendingCreated = "transaction.pending_created"
	AuditTransactionPosted         = "transaction.posted"
	AuditTransactionFailed         = "transaction.failed"
)

type auditEntryPayload struct {
	AccountID   string `json:"account_id"`
	AmountMinor int64  `json:"amount_minor"`
}

// transferAuditPayload captures the full set of legs, unlike the
// deliberately-minimal webhook event payload (transactionEventPayload in
// events.go) -- an outbound webhook receiver is expected to call back
// into the API for detail, but the audit trail is this system's own
// record of what happened, so it's recorded in full up front.
func transferAuditPayload(txn *Transaction, entries []EntryInput) map[string]any {
	entryPayloads := make([]auditEntryPayload, 0, len(entries))
	for _, e := range entries {
		entryPayloads = append(entryPayloads, auditEntryPayload{
			AccountID:   e.AccountID.String(),
			AmountMinor: int64(e.AmountMinor),
		})
	}
	return map[string]any{
		"transaction_id": txn.ID.String(),
		"status":         string(txn.Status),
		"entries":        entryPayloads,
	}
}

func statusChangeAuditPayload(txn *Transaction) map[string]any {
	return map[string]any{
		"transaction_id": txn.ID.String(),
		"status":         string(txn.Status),
	}
}

// recordAudit is a no-op when s.audit is nil (not configured), the same
// optionality WithEventEnqueuer gives event emission -- every caller/test
// that never configures an AuditRecorder keeps working unmodified.
func (s *Service) recordAudit(ctx context.Context, tx *sql.Tx, tenantID uuid.UUID, actorID, action string, payload any) error {
	if s.audit == nil {
		return nil
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return s.audit.RecordAudit(ctx, tx, tenantID, actorID, action, raw)
}
