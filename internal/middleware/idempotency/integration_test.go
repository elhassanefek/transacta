//go:build integration

// Run with: go test -tags=integration -race ./internal/middleware/idempotency/... -v
// Requires Docker running locally. See internal/ledger/integration_test.go
// for the same pattern; this mirrors it rather than introducing a new one.
package idempotency

import (
	"context"
	"database/sql"
	"errors"
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

	claimedRec, claimed, err := repo.ClaimOrGet(ctx, tenantID, "key-1", "hash-1", time.Hour)
	if err != nil || !claimed {
		t.Fatalf("initial claim: claimed=%v err=%v", claimed, err)
	}

	if err := repo.Complete(ctx, tenantID, "key-1", 201, []byte(`{"id":"txn-1"}`), time.Hour, claimedRec.CreatedAt); err != nil {
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

// TestClaimOrGet_AbandonedProcessingClaimReclaimableAfterShortLease proves
// the crash-recovery scenario against real Postgres: a claim that's never
// completed (simulating a crashed worker) becomes reclaimable once its
// short processing lease elapses, without needing to wait out a long
// replay-retention window.
func TestClaimOrGet_AbandonedProcessingClaimReclaimableAfterShortLease(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	repo := NewRepository(db)
	tenantID := mustCreateTenant(t, db)

	const shortLease = 200 * time.Millisecond

	_, claimed, err := repo.ClaimOrGet(ctx, tenantID, "crash-key", "hash-1", shortLease)
	if err != nil || !claimed {
		t.Fatalf("initial claim: claimed=%v err=%v", claimed, err)
	}

	// Immediately: still within the lease, must not be reclaimable.
	_, claimed, err = repo.ClaimOrGet(ctx, tenantID, "crash-key", "hash-1", shortLease)
	if err != nil {
		t.Fatalf("immediate re-check: %v", err)
	}
	if claimed {
		t.Fatal("expected claim to still be live within the lease window")
	}

	time.Sleep(shortLease * 3)

	rec, claimed, err := repo.ClaimOrGet(ctx, tenantID, "crash-key", "hash-2", time.Hour)
	if err != nil {
		t.Fatalf("post-lease claim: %v", err)
	}
	if !claimed {
		t.Fatal("expected abandoned claim to be reclaimable after its lease expired")
	}
	if rec.RequestHash != "hash-2" {
		t.Fatalf("reclaimed record hash = %q, want %q", rec.RequestHash, "hash-2")
	}
}

// TestComplete_FencingTokenPreventsStaleWriteFromClobberingNewerClaim
// proves the same scenario as its unit-test counterpart, but against real
// Postgres: a stale writer's completion, carrying an outdated created_at
// fencing token, must be refused once another server has reclaimed the
// key -- not silently overwrite the newer claim's result.
func TestComplete_FencingTokenPreventsStaleWriteFromClobberingNewerClaim(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	repo := NewRepository(db)
	tenantID := mustCreateTenant(t, db)

	recA, claimed, err := repo.ClaimOrGet(ctx, tenantID, "key-1", "hash-1", time.Millisecond)
	if err != nil || !claimed {
		t.Fatalf("A's claim: claimed=%v err=%v", claimed, err)
	}

	time.Sleep(10 * time.Millisecond)

	recB, claimed, err := repo.ClaimOrGet(ctx, tenantID, "key-1", "hash-1", time.Hour)
	if err != nil || !claimed {
		t.Fatalf("B's reclaim: claimed=%v err=%v", claimed, err)
	}

	if err := repo.Complete(ctx, tenantID, "key-1", 200, []byte(`{"winner":"B"}`), time.Hour, recB.CreatedAt); err != nil {
		t.Fatalf("B's complete: %v", err)
	}

	err = repo.Complete(ctx, tenantID, "key-1", 500, []byte(`{"winner":"A"}`), time.Hour, recA.CreatedAt)
	if !errors.Is(err, ErrClaimSuperseded) {
		t.Fatalf("A's stale complete error = %v, want ErrClaimSuperseded", err)
	}

	final, err := repo.Get(ctx, tenantID, "key-1")
	if err != nil {
		t.Fatalf("final get: %v", err)
	}
	if final.ResponseCode == nil || *final.ResponseCode != 200 {
		t.Fatalf("final response code = %v, want 200 (B's result must survive)", final.ResponseCode)
	}
	if string(final.ResponseBody) != `{"winner":"B"}` {
		t.Fatalf("final response body = %q, want B's result", final.ResponseBody)
	}
}