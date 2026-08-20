package ledger

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/google/uuid"
)

type Service struct {
	repo   *Repository
	events EventEnqueuer // nil is valid: event emission is optional
}

// Option configures optional Service behavior.
type Option func(*Service)

// WithEventEnqueuer wires event emission into the service. Without this,
// ExecuteTransfer/CreatePendingTransaction/PostPendingTransaction/
// FailPendingTransaction all behave exactly as before -- event emission
// is additive, never required, so every existing caller/test that never
// configures one keeps working unmodified.
func WithEventEnqueuer(e EventEnqueuer) Option {
	return func(s *Service) { s.events = e }
}

func NewService(repo *Repository, opts ...Option) *Service {
	s := &Service{repo: repo}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

type TransferRequest struct {
	TenantID uuid.UUID
	Entries  []EntryInput
}

func validateEntries(entries []EntryInput) error {
	if len(entries) < 2 {
		return ErrEmptyEntries
	}
	for _, e := range entries {
		if e.AmountMinor == 0 {
			return ErrZeroAmountEntry
		}
	}
	if Sum(entries) != 0 {
		return ErrUnbalancedEntries
	}
	return nil
}

func uniqueAccountIDs(entries []EntryInput) []uuid.UUID {
	seen := make(map[uuid.UUID]bool, len(entries))
	ids := make([]uuid.UUID, 0, len(entries))
	for _, e := range entries {
		if !seen[e.AccountID] {
			seen[e.AccountID] = true
			ids = append(ids, e.AccountID)
		}
	}
	return ids
}

// emitEvent enqueues a lifecycle event inside tx, the same transaction
// that's about to commit the state change it reports. If events is nil
// (not configured), this is a no-op.
//
// If Enqueue itself fails, this returns that error, and every call site
// below returns early on it -- which means the whole transaction rolls
// back, the transfer/status-change never happens. That's deliberate,
// not overly strict: the entire point of the transactional outbox
// pattern is that "the state change happened" and "an event recording
// it exists" are atomic, never two independent writes that could
// disagree. Letting the transfer succeed while silently dropping its
// event would reintroduce exactly the dual-write problem this pattern
// exists to eliminate.
func (s *Service) emitEvent(ctx context.Context, tx *sql.Tx, tenantID uuid.UUID, eventType string, txn *Transaction) error {
	if s.events == nil {
		return nil
	}
	payload, err := transactionEventJSON(txn)
	if err != nil {
		return fmt.Errorf("ledger: marshal event payload: %w", err)
	}
	if err := s.events.EnqueueEvent(ctx, tx, tenantID, eventType, payload); err != nil {
		return fmt.Errorf("ledger: enqueue event: %w", err)
	}
	return nil
}

func (s *Service) ExecuteTransfer(ctx context.Context, req TransferRequest) (*Transaction, []*Entry, error) {
	if err := validateEntries(req.Entries); err != nil {
		return nil, nil, err
	}

	tx, err := s.repo.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return nil, nil, fmt.Errorf("ledger: begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op if already committed

	accountIDs := uniqueAccountIDs(req.Entries)
	accounts, err := s.repo.LockAccountsForUpdate(ctx, tx, req.TenantID, accountIDs)
	if err != nil {
		return nil, nil, err
	}

	projected := make(map[uuid.UUID]Money, len(accounts))
	for id, a := range accounts {
		projected[id] = a.Balance
	}
	for _, e := range req.Entries {
		projected[e.AccountID] += e.AmountMinor
	}
	for id, bal := range projected {
		if bal < 0 {
			return nil, nil, fmt.Errorf("%w: account %s", ErrInsufficientFunds, id)
		}
	}

	txn, entries, err := s.repo.InsertTransactionWithEntries(ctx, tx, req.TenantID, StatusPosted, req.Entries)
	if err != nil {
		return nil, nil, err
	}

	if err := s.emitEvent(ctx, tx, req.TenantID, EventTransactionPosted, txn); err != nil {
		return nil, nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, nil, fmt.Errorf("ledger: commit transfer: %w", err)
	}
	return txn, entries, nil
}

func (s *Service) CreatePendingTransaction(ctx context.Context, req TransferRequest) (*Transaction, []*Entry, error) {
	if err := validateEntries(req.Entries); err != nil {
		return nil, nil, err
	}

	tx, err := s.repo.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return nil, nil, fmt.Errorf("ledger: begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	txn, entries, err := s.repo.InsertTransactionWithEntries(ctx, tx, req.TenantID, StatusPending, req.Entries)
	if err != nil {
		return nil, nil, err
	}
	if err := s.emitEvent(ctx, tx, req.TenantID, EventTransactionPending, txn); err != nil {
		return nil, nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, fmt.Errorf("ledger: commit pending transaction: %w", err)
	}
	return txn, entries, nil
}

func (s *Service) PostPendingTransaction(ctx context.Context, tenantID, id uuid.UUID) (*Transaction, error) {
	tx, err := s.repo.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return nil, fmt.Errorf("ledger: begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	txn, entries, err := s.repo.GetTransaction(ctx, tx, tenantID, id)
	if err != nil {
		return nil, err
	}
	if txn.Status != StatusPending {
		return nil, ErrTransactionNotPending
	}

	accountIDs := make([]uuid.UUID, 0, len(entries))
	for _, e := range entries {
		accountIDs = append(accountIDs, e.AccountID)
	}
	accounts, err := s.repo.LockAccountsForUpdate(ctx, tx, tenantID, accountIDs)
	if err != nil {
		return nil, err
	}
	projected := make(map[uuid.UUID]Money, len(accounts))
	for aid, a := range accounts {
		projected[aid] = a.Balance
	}
	for _, e := range entries {
		projected[e.AccountID] += e.AmountMinor
	}
	for aid, bal := range projected {
		if bal < 0 {
			return nil, fmt.Errorf("%w: account %s", ErrInsufficientFunds, aid)
		}
	}

	if err := s.repo.UpdateTransactionStatusGuarded(ctx, tx, tenantID, txn.ID, StatusPending, StatusPosted); err != nil {
		return nil, err
	}
	txn.Status = StatusPosted

	if err := s.emitEvent(ctx, tx, tenantID, EventTransactionPosted, txn); err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("ledger: commit post: %w", err)
	}
	return txn, nil
}

func (s *Service) FailPendingTransaction(ctx context.Context, tenantID, id uuid.UUID) (*Transaction, error) {
	tx, err := s.repo.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return nil, fmt.Errorf("ledger: begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	txn, _, err := s.repo.GetTransaction(ctx, tx, tenantID, id)
	if err != nil {
		return nil, err
	}
	if txn.Status != StatusPending {
		return nil, ErrTransactionNotPending
	}

	if err := s.repo.UpdateTransactionStatusGuarded(ctx, tx, tenantID, txn.ID, StatusPending, StatusFailed); err != nil {
		return nil, err
	}
	txn.Status = StatusFailed

	if err := s.emitEvent(ctx, tx, tenantID, EventTransactionFailed, txn); err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("ledger: commit fail: %w", err)
	}
	return txn, nil
}

func (s *Service) CreateAccount(ctx context.Context, tenantID uuid.UUID, name string) (*Account, error) {
	return s.repo.CreateAccount(ctx, s.repo.db, tenantID, name)
}

// GetTransaction is a read-only passthrough for API/handlers.
func (s *Service) GetTransaction(ctx context.Context, tenantID, id uuid.UUID) (*Transaction, []*Entry, error) {
	return s.repo.GetTransaction(ctx, s.repo.db, tenantID, id)
}

// GetAccount is a read-only passthrough for API/handlers.
func (s *Service) GetAccount(ctx context.Context, tenantID, id uuid.UUID) (*Account, error) {
	return s.repo.GetAccount(ctx, s.repo.db, tenantID, id)
}