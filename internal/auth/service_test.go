package auth

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func testService(t *testing.T, secret string, opts ...Option) *Service {
	t.Helper()
	return NewService(&Repository{}, []byte(secret), opts...)
}

func testUser() *User {
	return &User{
		ID:       uuid.New(),
		TenantID: uuid.New(),
		RoleID:   uuid.New(),
		Email:    "alice@acme.com",
		Status:   UserStatusActive,
	}
}

func TestGenerateAndValidateAccessToken_RoundTrip(t *testing.T) {
	svc := testService(t, "test-secret")
	user := testUser()

	token, expiresAt, err := svc.generateAccessToken(user)
	if err != nil {
		t.Fatalf("generateAccessToken: %v", err)
	}
	if token == "" {
		t.Fatal("expected non-empty token")
	}
	if expiresAt.Before(time.Now()) {
		t.Fatal("expected expiresAt in the future")
	}

	claims, err := svc.ValidateAccessToken(token)
	if err != nil {
		t.Fatalf("ValidateAccessToken: %v", err)
	}
	if claims.UserID != user.ID.String() {
		t.Errorf("claims.UserID = %q, want %q", claims.UserID, user.ID.String())
	}
	if claims.TenantID != user.TenantID.String() {
		t.Errorf("claims.TenantID = %q, want %q", claims.TenantID, user.TenantID.String())
	}
	if claims.RoleID != user.RoleID.String() {
		t.Errorf("claims.RoleID = %q, want %q", claims.RoleID, user.RoleID.String())
	}
}

func TestValidateAccessToken_RejectsExpired(t *testing.T) {
	svc := testService(t, "test-secret", WithAccessTokenTTL(-time.Minute))
	user := testUser()

	token, _, err := svc.generateAccessToken(user)
	if err != nil {
		t.Fatalf("generateAccessToken: %v", err)
	}

	if _, err := svc.ValidateAccessToken(token); err == nil {
		t.Fatal("expected an error validating an already-expired token")
	}
}

func TestValidateAccessToken_RejectsWrongSecret(t *testing.T) {
	issuer := testService(t, "secret-a")
	verifier := testService(t, "secret-b")
	user := testUser()

	token, _, err := issuer.generateAccessToken(user)
	if err != nil {
		t.Fatalf("generateAccessToken: %v", err)
	}

	if _, err := verifier.ValidateAccessToken(token); err == nil {
		t.Fatal("expected an error validating a token signed with a different secret")
	}
}

func TestValidateAccessToken_RejectsTamperedToken(t *testing.T) {
	svc := testService(t, "test-secret")
	user := testUser()

	token, _, err := svc.generateAccessToken(user)
	if err != nil {
		t.Fatalf("generateAccessToken: %v", err)
	}

	tampered := token[:len(token)-4] + "abcd"
	if _, err := svc.ValidateAccessToken(tampered); err == nil {
		t.Fatal("expected an error validating a tampered token")
	}
}

func TestGenerateRawToken_ProducesDistinctValues(t *testing.T) {
	raw1, hash1, err := generateRawToken()
	if err != nil {
		t.Fatalf("generateRawToken: %v", err)
	}
	raw2, hash2, err := generateRawToken()
	if err != nil {
		t.Fatalf("generateRawToken: %v", err)
	}
	if raw1 == raw2 {
		t.Fatal("expected two calls to generateRawToken to produce distinct values")
	}
	if hash1 == hash2 {
		t.Fatal("expected distinct raw tokens to hash to distinct values")
	}
	if hashToken(raw1) != hash1 {
		t.Fatal("expected generateRawToken's returned hash to match hashToken(raw)")
	}
}

func TestHashToken_Deterministic(t *testing.T) {
	h1 := hashToken("some-raw-token")
	h2 := hashToken("some-raw-token")
	if h1 != h2 {
		t.Fatal("expected identical inputs to hash identically")
	}
	if h1 == hashToken("a-different-token") {
		t.Fatal("expected different inputs to hash differently")
	}
}

func TestBcryptCost_Is12(t *testing.T) {
	// Pinning this in a test, not just a comment, so a future accidental
	// edit to the constant gets caught rather than silently shipped.
	if bcryptCost != 12 {
		t.Fatalf("bcryptCost = %d, want 12", bcryptCost)
	}
}

func TestDummyBcryptHash_IsWellFormed(t *testing.T) {
	// Login's timing-attack mitigation depends on this being a real,
	// valid bcrypt hash -- if it's malformed, bcrypt.CompareHashAndPassword
	// would fail fast instead of doing the comparably expensive work it's
	// there to simulate.
	if !strings.HasPrefix(dummyBcryptHash, "$2a$12$") {
		t.Fatalf("dummyBcryptHash = %q, want a cost-12 bcrypt hash", dummyBcryptHash)
	}
}
