package auth

import (
	"context"
	"database/sql"
	"encoding/json"

	"github.com/google/uuid"
)

// AuditRecorder is the entire dependency this package has on the audit
// trail -- same shape and same reasoning as ledger.AuditRecorder
// (internal/ledger/audit.go): satisfied structurally by
// audit.Repository's RecordAudit method, never imported directly.
type AuditRecorder interface {
	RecordAudit(ctx context.Context, tx *sql.Tx, tenantID uuid.UUID, actorID, action string, payload json.RawMessage) error
}

// Audit action vocabulary this package emits.
const (
	AuditUserRegistered = "user.registered"
	AuditUserLoggedIn   = "user.login"
	AuditTokenRefreshed = "user.token_refreshed"
	AuditUserLoggedOut  = "user.logout"
)

// auditActorAPIKey is the actor_id recorded for actions authenticated by
// a tenant's API key rather than a user session -- Register is the only
// one: there's no user yet, by definition, at the moment a new one is
// being created.
const auditActorAPIKey = "api-key"

// recordAudit is a no-op when s.audit is nil (not configured), the same
// optionality ledger.Service.recordAudit gives its callers.
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
