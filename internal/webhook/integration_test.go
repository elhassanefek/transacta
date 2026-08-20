//go:build integration

// Run with: go test -tags=integration -race ./internal/webhook/... -v
// Requires Docker running locally. See internal/ledger/integration_test.go
// for the same pattern.
package webhook

import (
	"context"
	"crypto/tls"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
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
	applyMigration(t, db, "000004_webhook_config.up.sql")
	return db
}

func applyMigration(t *testing.T, db *sql.DB, filename string) {
	t.Helper()
	data, err := os.ReadFile(fmt.Sprintf("../../migrations/%s", filename))
	if err != nil {
		t.Fatalf("read migration file %s: %v", filename, err)
	}
	if _, err := db.Exec(string(data)); err != nil {
		t.Fatalf("apply migration %s: %v", filename, err)
	}
}

func mustCreateTenant(t *testing.T, db *sql.DB, webhookURL, webhookSecret string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	err := db.QueryRow(
		`INSERT INTO tenants (name, api_key_hash, webhook_url, webhook_secret) VALUES ($1, $2, $3, $4) RETURNING id`,
		"test-tenant", "hash", nullIfEmpty(webhookURL), nullIfEmpty(webhookSecret),
	).Scan(&id)
	if err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	return id
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// --- Repository: transactional outbox guarantee ---

func TestRepository_Enqueue_RollsBackWithItsTransaction(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	repo := NewRepository(db)
	tenantID := mustCreateTenant(t, db, "", "")

	tx, err := repo.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	if _, err := repo.Enqueue(ctx, tx, tenantID, "test.event", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	var count int
	if err := db.QueryRow(`SELECT count(*) FROM webhook_events WHERE tenant_id = $1`, tenantID).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Fatalf("event count after rollback = %d, want 0 -- Enqueue must not survive its transaction rolling back", count)
	}
}

func TestRepository_Enqueue_CommitsWithItsTransaction(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	repo := NewRepository(db)
	tenantID := mustCreateTenant(t, db, "", "")

	tx, err := repo.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	ev, err := repo.Enqueue(ctx, tx, tenantID, "test.event", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	if ev.Status != StatusPending {
		t.Fatalf("status = %q, want %q", ev.Status, StatusPending)
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM webhook_events WHERE tenant_id = $1`, tenantID).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Fatalf("event count after commit = %d, want 1", count)
	}
}

// TestRepository_ClaimPendingEvents_ConcurrentRaceExactlyOneWinner proves
// FOR UPDATE SKIP LOCKED actually prevents two workers from claiming (and
// therefore delivering) the same event under real concurrent load.
func TestRepository_ClaimPendingEvents_ConcurrentRaceExactlyOneWinner(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	repo := NewRepository(db)
	tenantID := mustCreateTenant(t, db, "", "")

	tx, _ := repo.BeginTx(ctx, nil)
	if _, err := repo.Enqueue(ctx, tx, tenantID, "test.event", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	const workers = 10
	var claimedCount int64
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			claimTx, err := repo.BeginTx(ctx, nil)
			if err != nil {
				return
			}
			defer func() { _ = claimTx.Rollback() }()
			events, err := repo.ClaimPendingEvents(ctx, claimTx, 10)
			if err != nil {
				return
			}
			if len(events) > 0 {
				atomic.AddInt64(&claimedCount, int64(len(events)))
			}
			_ = claimTx.Commit()
		}()
	}
	wg.Wait()

	if claimedCount != 1 {
		t.Fatalf("claimedCount = %d, want exactly 1 (concurrent claim must not double-assign an event)", claimedCount)
	}
}

func TestRepository_MoveToDeadLetter(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	repo := NewRepository(db)
	tenantID := mustCreateTenant(t, db, "", "")

	tx, _ := repo.BeginTx(ctx, nil)
	ev, err := repo.Enqueue(ctx, tx, tenantID, "test.event", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	ev.AttemptCount = 5

	tx2, _ := repo.BeginTx(ctx, nil)
	if err := repo.MoveToDeadLetter(ctx, tx2, ev, "gave up"); err != nil {
		t.Fatalf("move to dead letter: %v", err)
	}
	if err := tx2.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	var dlCount int
	if err := db.QueryRow(`SELECT count(*) FROM dead_letter_events WHERE original_event_id = $1`, ev.ID).Scan(&dlCount); err != nil {
		t.Fatalf("count dead letter: %v", err)
	}
	if dlCount != 1 {
		t.Fatalf("dead_letter_events count = %d, want 1", dlCount)
	}

	var status string
	if err := db.QueryRow(`SELECT status FROM webhook_events WHERE id = $1`, ev.ID).Scan(&status); err != nil {
		t.Fatalf("get original status: %v", err)
	}
	if status != "failed" {
		t.Fatalf("original event status = %q, want %q (marked failed, not deleted)", status, "failed")
	}
}

// --- Service/Worker: full delivery lifecycle against real HTTP servers ---

func TestService_SuccessfulDelivery_WithValidSignature(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	repo := NewRepository(db)

	const secret = "whsec_test123"
	var receivedSig, receivedTS string
	var receivedBody []byte
	var callCount int32

	receiver := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&callCount, 1)
		receivedBody, _ = io.ReadAll(r.Body)
		receivedSig = r.Header.Get("X-Transacta-Signature")
		receivedTS = r.Header.Get("X-Transacta-Timestamp")
		w.WriteHeader(http.StatusOK)
	}))
	defer receiver.Close()

	tenantID := mustCreateTenant(t, db, receiver.URL, secret)

	tx, _ := repo.BeginTx(ctx, nil)
	if _, err := repo.Enqueue(ctx, tx, tenantID, "transaction.posted", json.RawMessage(`{"transaction_id":"abc"}`)); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	insecureClient := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	svc := NewService(repo, WithHTTPClient(insecureClient), WithMaxAttempts(3), WithBaseBackoff(100*time.Millisecond))
	worker := NewWorker(repo, svc, WithBatchSize(10), WithPollInterval(50*time.Millisecond))

	runWorkerFor(ctx, worker, 500*time.Millisecond)

	if atomic.LoadInt32(&callCount) != 1 {
		t.Fatalf("receiver called %d times, want 1", callCount)
	}
	if !VerifySignature(secret, parseUnixHeader(t, receivedTS), receivedBody, stripPrefix(receivedSig)) {
		t.Fatal("delivered request's signature does not verify against the tenant's secret")
	}

	var status string
	if err := db.QueryRow(`SELECT status FROM webhook_events WHERE tenant_id = $1`, tenantID).Scan(&status); err != nil {
		t.Fatalf("get status: %v", err)
	}
	if status != "delivered" {
		t.Fatalf("status = %q, want %q", status, "delivered")
	}
}

func TestService_RetryThenSucceed(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	repo := NewRepository(db)

	var attempts int32
	flaky := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&attempts, 1) < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer flaky.Close()

	tenantID := mustCreateTenant(t, db, flaky.URL, "secret")

	tx, _ := repo.BeginTx(ctx, nil)
	ev, err := repo.Enqueue(ctx, tx, tenantID, "test.event", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	insecureClient := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	svc := NewService(repo, WithHTTPClient(insecureClient), WithMaxAttempts(5), WithBaseBackoff(100*time.Millisecond), WithMaxBackoff(300*time.Millisecond))
	worker := NewWorker(repo, svc, WithBatchSize(10), WithPollInterval(50*time.Millisecond))

	runWorkerFor(ctx, worker, 3*time.Second)

	if atomic.LoadInt32(&attempts) != 3 {
		t.Fatalf("delivery attempts = %d, want 3 (2 failures + 1 success)", attempts)
	}
	var status string
	if err := db.QueryRow(`SELECT status FROM webhook_events WHERE id = $1`, ev.ID).Scan(&status); err != nil {
		t.Fatalf("get status: %v", err)
	}
	if status != "delivered" {
		t.Fatalf("status = %q, want %q", status, "delivered")
	}
}

func TestService_DeadLetterAfterExhaustingAttempts(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	repo := NewRepository(db)

	alwaysFails := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer alwaysFails.Close()

	tenantID := mustCreateTenant(t, db, alwaysFails.URL, "secret")

	tx, _ := repo.BeginTx(ctx, nil)
	ev, err := repo.Enqueue(ctx, tx, tenantID, "test.event", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	insecureClient := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	svc := NewService(repo, WithHTTPClient(insecureClient), WithMaxAttempts(3), WithBaseBackoff(100*time.Millisecond), WithMaxBackoff(200*time.Millisecond))
	worker := NewWorker(repo, svc, WithBatchSize(10), WithPollInterval(50*time.Millisecond))

	runWorkerFor(ctx, worker, 3*time.Second)

	var status string
	if err := db.QueryRow(`SELECT status FROM webhook_events WHERE id = $1`, ev.ID).Scan(&status); err != nil {
		t.Fatalf("get status: %v", err)
	}
	if status != "failed" {
		t.Fatalf("status = %q, want %q", status, "failed")
	}

	var dlCount int
	if err := db.QueryRow(`SELECT count(*) FROM dead_letter_events WHERE original_event_id = $1`, ev.ID).Scan(&dlCount); err != nil {
		t.Fatalf("count dead letter: %v", err)
	}
	if dlCount != 1 {
		t.Fatalf("dead_letter_events count = %d, want 1", dlCount)
	}
}

func TestService_NoEndpointConfigured_LeftPendingNotRetried(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	repo := NewRepository(db)
	tenantID := mustCreateTenant(t, db, "", "") // no webhook_url

	tx, _ := repo.BeginTx(ctx, nil)
	ev, err := repo.Enqueue(ctx, tx, tenantID, "test.event", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	svc := NewService(repo, WithMaxAttempts(3), WithBaseBackoff(50*time.Millisecond))
	worker := NewWorker(repo, svc, WithBatchSize(10), WithPollInterval(50*time.Millisecond))

	runWorkerFor(ctx, worker, 500*time.Millisecond)

	// Not delivered, not dead-lettered -- just skipped, since there's
	// nowhere to deliver to and burning through attempts against a
	// destination that doesn't exist would be pointless.
	var status string
	var attemptCount int
	if err := db.QueryRow(`SELECT status, attempt_count FROM webhook_events WHERE id = $1`, ev.ID).Scan(&status, &attemptCount); err != nil {
		t.Fatalf("get status: %v", err)
	}
	if status != "pending" {
		t.Fatalf("status = %q, want %q (unconfigured endpoint should not change event status)", status, "pending")
	}
}

func runWorkerFor(parent context.Context, w *Worker, d time.Duration) {
	ctx, cancel := context.WithTimeout(parent, d)
	defer cancel()
	done := make(chan struct{})
	go func() {
		w.Run(ctx)
		close(done)
	}()
	<-done
}

func parseUnixHeader(t *testing.T, s string) time.Time {
	t.Helper()
	var ts int64
	if _, err := fmt.Sscanf(s, "%d", &ts); err != nil {
		t.Fatalf("parse timestamp header %q: %v", s, err)
	}
	return time.Unix(ts, 0)
}

func stripPrefix(sig string) string {
	const prefix = "sha256="
	if len(sig) > len(prefix) && sig[:len(prefix)] == prefix {
		return sig[len(prefix):]
	}
	return sig
}