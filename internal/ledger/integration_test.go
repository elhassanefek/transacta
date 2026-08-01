// Run with: go test -tags=integration -race ./internal/ledger/... -v
// Requires Docker running locally; testcontainers-go pulls a real
// postgres:16 image and applies migrations/000001_init.up.sql +
// 000002_constraints.up.sql against it. See service_test.go for the fast,
// no-Docker unit tests.
package ledger

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

func setupTestDB(t *testing.T) *sql.DB {
	t.Helper()
	ctx := context.Background()

	container, err := postgres.Run(ctx,
		"postgres:16-alpine",
		postgres.WithDatabase("transacta_test"),
		postgres.WithUsername("test"),
		postgres.WithPassword("test"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).WithStartupTimeout(30*time.Second)),
	)
	if err != nil {
		t.Fatalf("start postgres container: %v", err)
	}
	t.Cleanup(func() {
		if err := container.Terminate(ctx); err != nil {
			t.Logf("terminate container: %v", err)
		}
	})

	connStr, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}

	db, err := sql.Open("pgx", connStr)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if err := db.PingContext(ctx); err != nil {
		t.Fatalf("ping db: %v", err)
	}

	applyMigration(t, db, "000001_init.up.sql")
	applyMigration(t, db, "000002_constraints.up.sql")
	return db
}

func applyMigration(t *testing.T, db *sql.DB, filename string) {
	t.Helper()
	// Inlined file read rather than depending on the migrate CLI/library
	// version, to keep this test self-contained. The real deployment path
	// uses `make migrate-up` against these same files.
	data, err := os.ReadFile(fmt.Sprintf("../../migrations/%s", filename))
	if err != nil {
		t.Fatalf("read migration file %s: %v", filename, err)
	}
	if _, err := db.Exec(string(data)); err != nil {
		t.Fatalf("apply migration %s: %v", filename, err)
	}
}

func mustCreateTenant(t *testing.T, db *sql.DB) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	err := db.QueryRow(
		`INSERT INTO tenants (name, api_key_hash) VALUES ($1, $2) RETURNING id`,
		"test-tenant", "hash",
	).Scan(&id)
	if err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	return id
}

// seedBalance gives an account a starting balance for test setup, via a
// direct "genesis" transaction+entry inserted with raw SQL -- deliberately
// bypassing Service.ExecuteTransfer's insufficient-funds check. There is
// no account type in this schema that's allowed to go negative (no
// external/unlimited funding source), so there's no legitimate way to
// originate a starting balance through the normal transfer path. Real
// double-entry ledgers solve this with a designated equity/external
// account; this schema doesn't have one yet, so tests seed directly.
func seedBalance(t *testing.T, db *sql.DB, tenantID, accountID uuid.UUID, amount Money) {
	t.Helper()
	var txnID uuid.UUID
	err := db.QueryRow(
		`INSERT INTO transactions (tenant_id, status) VALUES ($1, 'posted') RETURNING id`,
		tenantID,
	).Scan(&txnID)
	if err != nil {
		t.Fatalf("seed: create genesis transaction: %v", err)
	}
	_, err = db.Exec(
		`INSERT INTO entries (tenant_id, transaction_id, account_id, amount_minor) VALUES ($1, $2, $3, $4)`,
		tenantID, txnID, accountID, int64(amount),
	)
	if err != nil {
		t.Fatalf("seed: insert genesis entry: %v", err)
	}
}

// TestExecuteTransfer_ConcurrentTransfersNoLostUpdates fires many
// concurrent transfers between the same two accounts from many goroutines
// and asserts the final balances are exactly what sequential execution
// would have produced — i.e. pessimistic row locking actually serializes
// the conflicting writers instead of letting any lost update through.
func TestExecuteTransfer_ConcurrentTransfersNoLostUpdates(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	repo := NewRepository(db)
	svc := NewService(repo)

	tenantID := mustCreateTenant(t, db)

	a, err := repo.CreateAccount(ctx, db, tenantID, "alice")
	if err != nil {
		t.Fatalf("create account a: %v", err)
	}
	b, err := repo.CreateAccount(ctx, db, tenantID, "bob")
	if err != nil {
		t.Fatalf("create account b: %v", err)
	}

	// Seed both accounts to a starting balance directly -- there's no
	// account type in this schema allowed to go negative, so a normal
	// ExecuteTransfer can't legitimately originate a starting balance.
	// See seedBalance's doc comment.
	const startingBalance = Money(1_000_000) // 10,000.00 minor units
	seedBalance(t, db, tenantID, a.ID, startingBalance)
	seedBalance(t, db, tenantID, b.ID, startingBalance)

	const (
		numGoroutines  = 50
		transfersEach  = 20
		transferAmount = Money(100)
	)

	var wg sync.WaitGroup
	var successCount int64
	var mu sync.Mutex

	for g := 0; g < numGoroutines; g++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			for i := 0; i < transfersEach; i++ {
				from, to := a.ID, b.ID
				if (idx+i)%2 == 0 {
					from, to = b.ID, a.ID
				}
				_, _, err := svc.ExecuteTransfer(ctx, TransferRequest{
					TenantID: tenantID,
					Entries: []EntryInput{
						{AccountID: from, AmountMinor: -transferAmount},
						{AccountID: to, AmountMinor: transferAmount},
					},
				})
				if err != nil {
					t.Errorf("transfer failed: %v", err)
					continue
				}
				mu.Lock()
				successCount++
				mu.Unlock()
			}
		}(g)
	}
	wg.Wait()

	expectedTransfers := int64(numGoroutines * transfersEach)
	if successCount != expectedTransfers {
		t.Fatalf("successCount = %d, want %d", successCount, expectedTransfers)
	}

	finalA, err := repo.GetAccount(ctx, db, tenantID, a.ID)
	if err != nil {
		t.Fatalf("get account a: %v", err)
	}
	finalB, err := repo.GetAccount(ctx, db, tenantID, b.ID)
	if err != nil {
		t.Fatalf("get account b: %v", err)
	}

	totalBefore := 2 * startingBalance
	totalAfter := finalA.Balance + finalB.Balance
	if totalAfter != totalBefore {
		t.Fatalf("money was created/destroyed: total before=%d after=%d", totalBefore, totalAfter)
	}
	if finalA.Balance != startingBalance || finalB.Balance != startingBalance {
		t.Fatalf("balances drifted: a=%d b=%d, want %d each (even split of directions)",
			finalA.Balance, finalB.Balance, startingBalance)
	}
}

// TestPostPendingTransaction_StatusGuardConflict proves that when two
// goroutines race to post the same pending transaction, exactly one wins
// and the rest observe ErrStatusConflict rather than double-applying the
// balance effect.
func TestPostPendingTransaction_StatusGuardConflict(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	repo := NewRepository(db)
	svc := NewService(repo)

	tenantID := mustCreateTenant(t, db)

	a, err := repo.CreateAccount(ctx, db, tenantID, "alice")
	if err != nil {
		t.Fatalf("create account a: %v", err)
	}
	b, err := repo.CreateAccount(ctx, db, tenantID, "bob")
	if err != nil {
		t.Fatalf("create account b: %v", err)
	}
	seedBalance(t, db, tenantID, a.ID, 1000)

	txn, _, err := svc.CreatePendingTransaction(ctx, TransferRequest{
		TenantID: tenantID,
		Entries: []EntryInput{
			{AccountID: a.ID, AmountMinor: -500},
			{AccountID: b.ID, AmountMinor: 500},
		},
	})
	if err != nil {
		t.Fatalf("create pending transaction: %v", err)
	}

	const racers = 10
	var wg sync.WaitGroup
	results := make([]error, racers)
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_, err := svc.PostPendingTransaction(ctx, tenantID, txn.ID)
			results[idx] = err
		}(i)
	}
	wg.Wait()

	var successes, conflicts, other int
	for _, err := range results {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, ErrStatusConflict), errors.Is(err, ErrTransactionNotPending):
			conflicts++
		default:
			other++
			t.Logf("unexpected error: %v", err)
		}
	}

	if successes != 1 {
		t.Fatalf("successes = %d, want exactly 1 (double-post must not both succeed)", successes)
	}
	if other != 0 {
		t.Fatalf("got %d unexpected errors, want 0", other)
	}

	finalA, _ := repo.GetAccount(ctx, db, tenantID, a.ID)
	finalB, _ := repo.GetAccount(ctx, db, tenantID, b.ID)
	if finalA.Balance != 500 || finalB.Balance != 500 {
		t.Fatalf("balance applied more than once: a=%d b=%d, want 500 each", finalA.Balance, finalB.Balance)
	}
}
