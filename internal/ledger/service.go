package ledger

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/google/uuid"
)


type Service struct {
	repo *Repository
}

func NewService(repo *Repository) *Service {
	return &Service{repo: repo}
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

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("ledger: commit post: %w", err)
	}
	txn.Status = StatusPosted
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
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("ledger: commit fail: %w", err)
	}
	txn.Status = StatusFailed
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