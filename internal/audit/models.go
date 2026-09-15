package audit

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// Entry is one immutable row of the audit_log table: a record that some
// actor performed some mutating action, scoped to a tenant. Rows are
// never updated or deleted by application code -- audit_log is
// append-only by design, the same guarantee the transaction log itself
// makes for financial state.
type Entry struct {
	ID        uuid.UUID
	TenantID  uuid.UUID
	Action    string
	ActorID   string
	Payload   json.RawMessage
	CreatedAt time.Time
}
