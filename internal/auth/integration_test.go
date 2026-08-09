//go:build integration

// Run with: go test -tags=integration -race ./internal/auth/... -v
// Requires Docker running locally. See internal/ledger/integration_test.go
// for the same pattern.
package auth

import (
	"context"
	"database/sql"
	"fmt"
	"os"
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
	applyMigration(t, db, "000003_users_auth.up.sql")
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

func mustGetRoleID(t *testing.T, db *sql.DB, name string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := db.QueryRow(`SELECT id FROM roles WHERE name = $1`, name).Scan(&id); err != nil {
		t.Fatalf("get role %q: %v", name, err)
	}
	return id
}

func TestService_RegisterLoginRoundTrip(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	repo := NewRepository(db)
	svc := NewService(repo, []byte("test-secret"))
	tenantID := mustCreateTenant(t, db)
	adminRoleID := mustGetRoleID(t, db, "admin")

	user, err := svc.Register(ctx, tenantID, adminRoleID, "alice@acme.com", "correct-horse-battery-staple")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	result, err := svc.Login(ctx, tenantID, "alice@acme.com", "correct-horse-battery-staple")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if result.AccessToken == "" || result.RefreshToken == "" {
		t.Fatal("expected non-empty access and refresh tokens")
	}

	claims, err := svc.ValidateAccessToken(result.AccessToken)
	if err != nil {
		t.Fatalf("ValidateAccessToken: %v", err)
	}
	if claims.UserID != user.ID.String() {
		t.Fatalf("claims.UserID = %q, want %q", claims.UserID, user.ID.String())
	}
	if claims.TenantID != tenantID.String() {
		t.Fatalf("claims.TenantID = %q, want %q", claims.TenantID, tenantID.String())
	}
}

func TestService_Login_WrongPasswordRejected(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	repo := NewRepository(db)
	svc := NewService(repo, []byte("test-secret"))
	tenantID := mustCreateTenant(t, db)
	roleID := mustGetRoleID(t, db, "viewer")

	if _, err := svc.Register(ctx, tenantID, roleID, "bob@acme.com", "correct-password"); err != nil {
		t.Fatalf("Register: %v", err)
	}

	if _, err := svc.Login(ctx, tenantID, "bob@acme.com", "wrong-password"); err == nil {
		t.Fatal("expected login with wrong password to fail")
	}
}

func TestService_Login_NonexistentEmailRejectedSameAsWrongPassword(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	repo := NewRepository(db)
	svc := NewService(repo, []byte("test-secret"))
	tenantID := mustCreateTenant(t, db)

	_, err := svc.Login(ctx, tenantID, "nobody@acme.com", "whatever")
	if err == nil {
		t.Fatal("expected login with nonexistent email to fail")
	}
	if err != ErrInvalidCredentials {
		t.Fatalf("error = %v, want ErrInvalidCredentials specifically (not a distinguishable 'not found')", err)
	}
}

func TestService_Register_CaseInsensitiveDuplicateEmailRejected(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	repo := NewRepository(db)
	svc := NewService(repo, []byte("test-secret"))
	tenantID := mustCreateTenant(t, db)
	roleID := mustGetRoleID(t, db, "viewer")

	if _, err := svc.Register(ctx, tenantID, roleID, "carol@acme.com", "password"); err != nil {
		t.Fatalf("Register: %v", err)
	}
	_, err := svc.Register(ctx, tenantID, roleID, "Carol@Acme.com", "different-password")
	if err != ErrEmailAlreadyExists {
		t.Fatalf("error = %v, want ErrEmailAlreadyExists", err)
	}
}

func TestService_Refresh_RotatesToken(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	repo := NewRepository(db)
	svc := NewService(repo, []byte("test-secret"))
	tenantID := mustCreateTenant(t, db)
	roleID := mustGetRoleID(t, db, "viewer")

	if _, err := svc.Register(ctx, tenantID, roleID, "dave@acme.com", "password"); err != nil {
		t.Fatalf("Register: %v", err)
	}
	original, err := svc.Login(ctx, tenantID, "dave@acme.com", "password")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	refreshed, err := svc.Refresh(ctx, original.RefreshToken)
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if refreshed.RefreshToken == original.RefreshToken {
		t.Fatal("expected Refresh to issue a new, different refresh token")
	}
}

// TestService_Refresh_ReuseOfRotatedTokenRevokesEntireSessionFamily is the
// core security property this package exists to provide: presenting an
// already-rotated (superseded) refresh token is treated as evidence of
// theft, and the fix isn't just rejecting that one token -- it's killing
// every active session for the user, including the legitimately-current
// one, since we can no longer trust which client is the real owner.
func TestService_Refresh_ReuseOfRotatedTokenRevokesEntireSessionFamily(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	repo := NewRepository(db)
	svc := NewService(repo, []byte("test-secret"))
	tenantID := mustCreateTenant(t, db)
	roleID := mustGetRoleID(t, db, "viewer")

	if _, err := svc.Register(ctx, tenantID, roleID, "eve@acme.com", "password"); err != nil {
		t.Fatalf("Register: %v", err)
	}
	original, err := svc.Login(ctx, tenantID, "eve@acme.com", "password")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	refreshed, err := svc.Refresh(ctx, original.RefreshToken)
	if err != nil {
		t.Fatalf("first Refresh: %v", err)
	}

	// Replay the OLD, now-superseded token -- this is the theft signal.
	if _, err := svc.Refresh(ctx, original.RefreshToken); err == nil {
		t.Fatal("expected reuse of an already-rotated token to fail")
	}

	// The newer, legitimately-current token must ALSO be dead now --
	// proving the escalation was family-wide, not just rejecting the
	// replayed token itself.
	if _, err := svc.Refresh(ctx, refreshed.RefreshToken); err == nil {
		t.Fatal("expected the entire session family to be revoked, but the newer token still worked")
	}
}

func TestService_Refresh_UnknownTokenRejected(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	repo := NewRepository(db)
	svc := NewService(repo, []byte("test-secret"))

	if _, err := svc.Refresh(ctx, "this-token-was-never-issued"); err != ErrRefreshTokenNotFound {
		t.Fatalf("error = %v, want ErrRefreshTokenNotFound", err)
	}
}

func TestRepository_GetUserPermissions_MatchesSeededRoleGrants(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	repo := NewRepository(db)
	tenantID := mustCreateTenant(t, db)
	viewerRoleID := mustGetRoleID(t, db, "viewer")

	user, err := repo.CreateUser(ctx, db, tenantID, viewerRoleID, "frank@acme.com", "hashed")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	perms, err := repo.GetUserPermissions(ctx, db, tenantID, user.ID)
	if err != nil {
		t.Fatalf("GetUserPermissions: %v", err)
	}
	want := map[string]bool{"transactions:read": true, "accounts:read": true}
	if len(perms) != len(want) {
		t.Fatalf("got %d permissions, want %d: %v", len(perms), len(want), perms)
	}
	for _, p := range perms {
		if !want[p] {
			t.Fatalf("unexpected permission %q for viewer role", p)
		}
	}
}

// TestRepository_RotateRefreshToken_ConcurrentRaceExactlyOneWinner proves
// the atomic WHERE-guarded UPDATE actually serializes concurrent rotation
// attempts on the same token under real concurrent load, not just in
// single-threaded reasoning.
func TestRepository_RotateRefreshToken_ConcurrentRaceExactlyOneWinner(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	repo := NewRepository(db)
	tenantID := mustCreateTenant(t, db)
	roleID := mustGetRoleID(t, db, "viewer")

	user, err := repo.CreateUser(ctx, db, tenantID, roleID, "grace@acme.com", "hashed")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	oldToken, err := repo.CreateRefreshToken(ctx, db, user.ID, tenantID, "shared-hash", time.Hour)
	if err != nil {
		t.Fatalf("CreateRefreshToken: %v", err)
	}

	const racers = 20
	results := make(chan error, racers)
	for i := 0; i < racers; i++ {
		go func() {
			tx, err := repo.BeginTx(ctx, nil)
			if err != nil {
				results <- err
				return
			}
			defer func() { _ = tx.Rollback() }()

			newTok, err := repo.CreateRefreshToken(ctx, tx, user.ID, tenantID, fmt.Sprintf("new-hash-%d", i), time.Hour)
			if err != nil {
				results <- err
				return
			}
			err = repo.RotateRefreshToken(ctx, tx, oldToken.ID, newTok.ID)
			if err == nil {
				err = tx.Commit()
			}
			results <- err
		}()
	}

	successes := 0
	for i := 0; i < racers; i++ {
		if err := <-results; err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("successes = %d, want exactly 1 (concurrent rotation of the same token must not both win)", successes)
	}
}