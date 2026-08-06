//go:build integration

// Run with: go test -tags=integration -race ./internal/middleware/idempotency/... -v
// Requires Docker running locally. See internal/ledger/integration_test.go
// for the same pattern; this mirrors it rather than introducing a new one.
package idempotency

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
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
	data, err := os.ReadFile(fmt.Sprintf("../../../migrations/%s", filename))
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

// TestClaimOrGet_ConcurrentIdenticalKeyExactlyOneClaimWins fires many
// concurrent ClaimOrGet calls with the identical tenant+key+hash and
// asserts exactly one of them reports claimed=true -- proving the
// INSERT ... ON CONFLICT ... DO UPDATE ... WHERE query is genuinely
// atomic under real concurrent load, not just correct in single-threaded
// reasoning.
func TestClaimOrGet_ConcurrentIdenticalKeyExactlyOneClaimWins(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	repo := NewRepository(db)
	tenantID := mustCreateTenant(t, db)

	const racers = 50
	var claimedCount int64
	var wg sync.WaitGroup
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, claimed, err := repo.ClaimOrGet(ctx, tenantID, "race-key", "hash-abc", time.Hour)
			if err != nil {
				t.Errorf("ClaimOrGet failed: %v", err)
				return
			}
			if claimed {
				atomic.AddInt64(&claimedCount, 1)
			}
		}()
	}
	wg.Wait()

	if claimedCount != 1 {
		t.Fatalf("claimedCount = %d, want exactly 1 (concurrent identical keys must not both win)", claimedCount)
	}
}

// TestClaimOrGet_ExpiredRecordCanBeReclaimed proves that once a record's
// expires_at has passed, ClaimOrGet treats the key as available again
// rather than permanently blocking reuse.
func TestClaimOrGet_ExpiredRecordCanBeReclaimed(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	repo := NewRepository(db)
	tenantID := mustCreateTenant(t, db)

	// Claim with a TTL in the past so it's already expired.
	_, claimed, err := repo.ClaimOrGet(ctx, tenantID, "expiring-key", "hash-1", -time.Hour)
	if err != nil {
		t.Fatalf("initial claim: %v", err)
	}
	if !claimed {
		t.Fatal("expected initial claim to succeed")
	}

	rec, claimed, err := repo.ClaimOrGet(ctx, tenantID, "expiring-key", "hash-2", time.Hour)
	if err != nil {
		t.Fatalf("reclaim after expiry: %v", err)
	}
	if !claimed {
		t.Fatal("expected expired record to be reclaimable")
	}
	if rec.RequestHash != "hash-2" {
		t.Fatalf("reclaimed record hash = %q, want %q (should reflect the new request)", rec.RequestHash, "hash-2")
	}
}

// TestRepository_CompleteThenReplay proves the full round trip: claim,
// complete with a response, then a fresh ClaimOrGet for the same key
// returns claimed=false with the cached response intact.
func TestRepository_CompleteThenReplay(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	repo := NewRepository(db)
	tenantID := mustCreateTenant(t, db)

	_, claimed, err := repo.ClaimOrGet(ctx, tenantID, "key-1", "hash-1", time.Hour)
	if err != nil || !claimed {
		t.Fatalf("initial claim: claimed=%v err=%v", claimed, err)
	}

	if err := repo.Complete(ctx, tenantID, "key-1", 201, []byte(`{"id":"txn-1"}`)); err != nil {
		t.Fatalf("complete: %v", err)
	}

	rec, claimed, err := repo.ClaimOrGet(ctx, tenantID, "key-1", "hash-1", time.Hour)
	if err != nil {
		t.Fatalf("second claim attempt: %v", err)
	}
	if claimed {
		t.Fatal("expected second attempt to NOT claim -- a completed record should block re-execution")
	}
	if rec.Status != StatusCompleted {
		t.Fatalf("status = %q, want %q", rec.Status, StatusCompleted)
	}
	if rec.ResponseCode == nil || *rec.ResponseCode != 201 {
		t.Fatalf("response code = %v, want 201", rec.ResponseCode)
	}
	if string(rec.ResponseBody) != `{"id":"txn-1"}` {
		t.Fatalf("response body = %q, want %q", rec.ResponseBody, `{"id":"txn-1"}`)
	}
}