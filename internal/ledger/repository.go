package ledger

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"

	"github.com/google/uuid"
)

type DBTX interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

type Repository struct {
	db *sql.DB
}

func NewRepository(db *sql.DB) *Repository {
	return &Repository{db: db}
}

func (r *Repository) BeginTx(ctx context.Context, opts *sql.TxOptions) (*sql.Tx, error) {
	return r.db.BeginTx(ctx, opts)
}

func (r *Repository) CreateAccount(ctx context.Context, q DBTX, tenantID uuid.UUID, name string) (*Account, error) {
	a := &Account{TenantID: tenantID, Name: name}
	const query = `
		INSERT INTO accounts (tenant_id, name)
		VALUES ($1, $2)
		RETURNING id, created_at`
	if err := q.QueryRowContext(ctx, query, tenantID, name).Scan(&a.ID, &a.CreatedAt); err != nil {
		return nil, fmt.Errorf("ledger: create account: %w", err)
	}
	return a, nil
}

func (r *Repository) GetAccount(ctx context.Context, q DBTX, tenantID, id uuid.UUID) (*Account, error) {
	const query = `
		SELECT a.id, a.tenant_id, a.name, a.created_at,
		       COALESCE((
		           SELECT SUM(e.amount_minor)
		           FROM entries e
		           JOIN transactions t ON t.id = e.transaction_id
		           WHERE e.account_id = a.id AND t.status = 'posted'
		       ), 0)
		FROM accounts a
		WHERE a.id = $1 AND a.tenant_id = $2`
	var a Account
	var balance int64
	err := q.QueryRowContext(ctx, query, id, tenantID).Scan(&a.ID, &a.TenantID, &a.Name, &a.CreatedAt, &balance)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrAccountNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("ledger: get account: %w", err)
	}
	a.Balance = Money(balance)
	return &a, nil
}

func (r *Repository) LockAccountsForUpdate(ctx context.Context, tx *sql.Tx, tenantID uuid.UUID, ids []uuid.UUID) (map[uuid.UUID]*Account, error) {
	if len(ids) == 0 {
		return map[uuid.UUID]*Account{}, nil
	}

	sorted := append([]uuid.UUID(nil), ids...)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].String() < sorted[j].String()
	})
	idStrings := uuidsToStrings(sorted)

	const lockQuery = `
		SELECT id, tenant_id, name, created_at
		FROM accounts
		WHERE id = ANY($1) AND tenant_id = $2
		ORDER BY id
		FOR UPDATE`

	rows, err := tx.QueryContext(ctx, lockQuery, idStrings, tenantID)
	if err != nil {
		return nil, fmt.Errorf("ledger: lock accounts: %w", err)
	}
	out := make(map[uuid.UUID]*Account, len(sorted))
	for rows.Next() {
		var a Account
		if err := rows.Scan(&a.ID, &a.TenantID, &a.Name, &a.CreatedAt); err != nil {
			rows.Close()
			return nil, fmt.Errorf("ledger: scan locked account: %w", err)
		}
		out[a.ID] = &a
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("ledger: lock accounts: %w", err)
	}
	rows.Close()

	for _, id := range sorted {
		if _, ok := out[id]; !ok {
			return nil, fmt.Errorf("%w: %s", ErrAccountNotFound, id)
		}
	}

	// Now that the rows are locked, compute each account's current
	// derived balance within the same transaction.
	const balanceQuery = `
		SELECT e.account_id, COALESCE(SUM(e.amount_minor), 0)
		FROM entries e
		JOIN transactions t ON t.id = e.transaction_id
		WHERE e.account_id = ANY($1) AND t.status = 'posted'
		GROUP BY e.account_id`
	brows, err := tx.QueryContext(ctx, balanceQuery, idStrings)
	if err != nil {
		return nil, fmt.Errorf("ledger: sum balances: %w", err)
	}
	defer brows.Close()
	for brows.Next() {
		var accID uuid.UUID
		var sum int64
		if err := brows.Scan(&accID, &sum); err != nil {
			return nil, fmt.Errorf("ledger: scan balance sum: %w", err)
		}
		if a, ok := out[accID]; ok {
			a.Balance = Money(sum)
		}
	}
	if err := brows.Err(); err != nil {
		return nil, fmt.Errorf("ledger: sum balances: %w", err)
	}

	return out, nil
}

func (r *Repository) InsertTransactionWithEntries(ctx context.Context, tx *sql.Tx, tenantID uuid.UUID, status Status, entries []EntryInput) (*Transaction, []*Entry, error) {
	txn := &Transaction{TenantID: tenantID, Status: status}
	const insertTxn = `
		INSERT INTO transactions (tenant_id, status)
		VALUES ($1, $2)
		RETURNING id, created_at`
	if err := tx.QueryRowContext(ctx, insertTxn, tenantID, status).Scan(&txn.ID, &txn.CreatedAt); err != nil {
		return nil, nil, fmt.Errorf("ledger: insert transaction: %w", err)
	}

	const insertEntry = `
		INSERT INTO entries (tenant_id, transaction_id, account_id, amount_minor)
		VALUES ($1, $2, $3, $4)
		RETURNING id, created_at`

	out := make([]*Entry, 0, len(entries))
	for _, e := range entries {
		row := &Entry{TenantID: tenantID, TransactionID: txn.ID, AccountID: e.AccountID, AmountMinor: e.AmountMinor}
		if err := tx.QueryRowContext(ctx, insertEntry,
			tenantID, txn.ID, row.AccountID, int64(row.AmountMinor),
		).Scan(&row.ID, &row.CreatedAt); err != nil {
			return nil, nil, fmt.Errorf("ledger: insert entry: %w", err)
		}
		out = append(out, row)
	}
	return txn, out, nil
}

func (r *Repository) GetTransaction(ctx context.Context, q DBTX, tenantID, id uuid.UUID) (*Transaction, []*Entry, error) {
	const txnQuery = `
		SELECT id, tenant_id, status, created_at
		FROM transactions WHERE id = $1 AND tenant_id = $2`
	var txn Transaction
	err := q.QueryRowContext(ctx, txnQuery, id, tenantID).Scan(&txn.ID, &txn.TenantID, &txn.Status, &txn.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, ErrTransactionNotFound
	}
	if err != nil {
		return nil, nil, fmt.Errorf("ledger: get transaction: %w", err)
	}

	const entriesQuery = `
		SELECT id, tenant_id, transaction_id, account_id, amount_minor, created_at
		FROM entries WHERE transaction_id = $1 AND tenant_id = $2 ORDER BY created_at`
	rows, err := q.QueryContext(ctx, entriesQuery, id, tenantID)
	if err != nil {
		return nil, nil, fmt.Errorf("ledger: get entries: %w", err)
	}
	defer rows.Close()

	var entries []*Entry
	for rows.Next() {
		var e Entry
		var amount int64
		if err := rows.Scan(&e.ID, &e.TenantID, &e.TransactionID, &e.AccountID, &amount, &e.CreatedAt); err != nil {
			return nil, nil, fmt.Errorf("ledger: scan entry: %w", err)
		}
		e.AmountMinor = Money(amount)
		entries = append(entries, &e)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("ledger: get entries: %w", err)
	}
	return &txn, entries, nil
}

func (r *Repository) UpdateTransactionStatusGuarded(ctx context.Context, tx *sql.Tx, tenantID, id uuid.UUID, expectedStatus, newStatus Status) error {
	const query = `
		UPDATE transactions
		SET status = $1
		WHERE id = $2 AND tenant_id = $3 AND status = $4`
	res, err := tx.ExecContext(ctx, query, newStatus, id, tenantID, expectedStatus)
	if err != nil {
		return fmt.Errorf("ledger: update transaction status: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("ledger: update transaction status: %w", err)
	}
	if n == 0 {
		var status string
		checkErr := tx.QueryRowContext(ctx,
			`SELECT status FROM transactions WHERE id = $1 AND tenant_id = $2`, id, tenantID,
		).Scan(&status)
		if errors.Is(checkErr, sql.ErrNoRows) {
			return ErrTransactionNotFound
		}
		if status != string(expectedStatus) {
			if expectedStatus == StatusPending {
				return ErrTransactionNotPending
			}
			return ErrStatusConflict
		}
		return ErrStatusConflict
	}
	return nil
}

func uuidsToStrings(ids []uuid.UUID) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = id.String()
	}
	return out
}
