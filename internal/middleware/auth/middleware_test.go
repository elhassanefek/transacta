package auth


import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/elhassanefek/transacta/internal/auth"
	"github.com/elhassanefek/transacta/internal/tenants"
)

type fakeValidator struct {
	claims *auth.Claims
	err    error
}

func (f *fakeValidator) ValidateAccessToken(_ string) (*auth.Claims, error) {
	return f.claims, f.err
}

func testHandler() (http.HandlerFunc, *bool) {
	called := false
	return func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}, &called
}

func TestMiddleware_MissingAuthorizationHeaderRejected(t *testing.T) {
	handler, called := testHandler()
	mw := Middleware(&fakeValidator{})(handler)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
	if *called {
		t.Fatal("expected the next handler to not be called")
	}
}

func TestMiddleware_MalformedHeaderRejected(t *testing.T) {
	handler, called := testHandler()
	mw := Middleware(&fakeValidator{})(handler)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "NotBearer sometoken")
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
	if *called {
		t.Fatal("expected the next handler to not be called")
	}
}

func TestMiddleware_EmptyBearerTokenRejected(t *testing.T) {
	handler, called := testHandler()
	mw := Middleware(&fakeValidator{})(handler)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer ")
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
	if *called {
		t.Fatal("expected the next handler to not be called")
	}
}

func TestMiddleware_InvalidTokenRejected(t *testing.T) {
	handler, called := testHandler()
	mw := Middleware(&fakeValidator{err: errors.New("signature invalid")})(handler)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer some.invalid.token")
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
	if *called {
		t.Fatal("expected the next handler to not be called")
	}
}

func TestMiddleware_MalformedTenantClaimRejected(t *testing.T) {
	handler, called := testHandler()
	claims := &auth.Claims{UserID: uuid.New().String(), TenantID: "not-a-uuid", RoleID: uuid.New().String()}
	mw := Middleware(&fakeValidator{claims: claims})(handler)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer valid-looking-token")
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
	if *called {
		t.Fatal("expected the next handler to not be called")
	}
}

func TestMiddleware_ValidTokenPopulatesContextAndCallsNext(t *testing.T) {
	userID := uuid.New()
	tenantID := uuid.New()
	roleID := uuid.New()
	claims := &auth.Claims{UserID: userID.String(), TenantID: tenantID.String(), RoleID: roleID.String()}

	var gotTenantID uuid.UUID
	var gotTenantOK bool
	var gotClaims *auth.Claims
	var gotClaimsOK bool

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTenantID, gotTenantOK = tenants.FromContext(r.Context())
		gotClaims, gotClaimsOK = auth.ClaimsFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})
	mw := Middleware(&fakeValidator{claims: claims})(handler)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer valid-looking-token")
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if !gotTenantOK || gotTenantID != tenantID {
		t.Fatalf("tenant context = %v (ok=%v), want %v", gotTenantID, gotTenantOK, tenantID)
	}
	if !gotClaimsOK || gotClaims == nil || gotClaims.UserID != userID.String() {
		t.Fatalf("claims context ok=%v, claims=%v, want UserID=%v", gotClaimsOK, gotClaims, userID)
	}
}