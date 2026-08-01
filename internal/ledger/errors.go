package ledger

import "errors"

var (
	// ErrUnbalancedEntries is returned when a caller supplies a set of
	// entries whose amounts do not sum to zero.

	ErrUnbalancedEntries = errors.New("ledger: entries must sum to zero")

	// ErrEmptyEntries is returned when a transaction is submitted with
	// fewer than two entries.

	ErrEmptyEntries = errors.New("ledger: transaction requires at least two entries")

	// ErrZeroAmountEntry is returned when any single entry has an amount
	// of zero — mirrors the chk_entry_amount_nonzero DB constraint, but
	// checked in the service layer first so callers get a clean domain
	// error instead of a raw constraint-violation error from Postgres.

	ErrZeroAmountEntry = errors.New("ledger: entry amount must be nonzero")

	// ErrAccountNotFound is returned when a referenced account does not
	// exist for the given tenant.

	ErrAccountNotFound = errors.New("ledger: account not found")

	// ErrInsufficientFunds is returned when a debit would drive an
	// account's derived balance negative.

	ErrInsufficientFunds = errors.New("ledger: insufficient funds")

	// ErrTransactionNotFound is returned when a referenced transaction
	// does not exist for the given tenant.

	ErrTransactionNotFound = errors.New("ledger: transaction not found")

	// ErrTransactionNotPending is returned when an operation that only
	// makes sense against a pending transaction (post, fail) is attempted
	// against one that has already reached a terminal state.

	ErrTransactionNotPending = errors.New("ledger: transaction is not pending")

	// ErrStatusConflict is returned when a status-guarded UPDATE (e.g.
	// "WHERE id = $1 AND status = 'pending'") matches zero rows because
	// another writer already transitioned the row first. This schema has
	// no version column, so the transaction's own status is the
	// compare-and-swap guard. Callers should treat this as "reload and
	// find out what happened," not as a hard failure.
	ErrStatusConflict = errors.New("ledger: status changed concurrently, reload and retry")
)
