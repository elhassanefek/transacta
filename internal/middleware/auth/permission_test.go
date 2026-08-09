package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	
	"github.com/elhassanefek/transacta/internal/auth"
	"github.com/elhassanefek/transacta/internal/tenants"
)

type fakePermissionChecker struct {
	perms []string
	err   error
}

func (f *fakePermissionChecker) GetUserPermissions(_ context.Context, _, _ uuid.UUID) ([]string, error) {
	return f.perms, f.err
}

func withAuthenticatedContext(req *http.Request, userID, tenantID, roleID uuid.UUID) *http.Request {
	ctx := tenants.WithTenantID(req.Context(), tenantID)
	ctx = auth.WithClaims(ctx, &auth.Claims{
		UserID:   userID.String(),
		TenantID: tenantID.String(),
		RoleID:   roleID.String(),
	})
	return req.WithContext(ctx)
}

func TestRequirePermission_NoAuthenticatedUserRejected(t *testing.T) {
	handler, called := testHandler()
	mw := RequirePermission(&fakePermissionChecker{}, "transactions:write")(handler)

	// Deliberately not calling withAuthenticatedContext -- simulates
	// Middleware not having run first.
	req := httptest.NewRequest(http.MethodPost, "/transfers", nil)
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
	if *called {
		t.Fatal("expected the next handler to not be called")
	}
}

func TestRequirePermission_UserHasPermission_Allowed(t *testing.T) {
	handler, called := testHandler()
	checker := &fakePermissionChecker{perms: []string{"transactions:read", "transactions:write"}}
	mw := RequirePermission(checker, "transactions:write")(handler)

	req := withAuthenticatedContext(httptest.NewRequest(http.MethodPost, "/transfers", nil), uuid.New(), uuid.New(), uuid.New())
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if !*called {
		t.Fatal("expected the next handler to be called")
	}
}

func TestRequirePermission_UserLacksPermission_Forbidden(t *testing.T) {
	handler, called := testHandler()
	checker := &fakePermissionChecker{perms: []string{"transactions:read"}} // read only, no write
	mw := RequirePermission(checker, "transactions:write")(handler)

	req := withAuthenticatedContext(httptest.NewRequest(http.MethodPost, "/transfers", nil), uuid.New(), uuid.New(), uuid.New())
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
	if *called {
		t.Fatal("expected the next handler to not be called")
	}
}

func TestRequirePermission_CheckerErrorRejected(t *testing.T) {
	handler, called := testHandler()
	checker := &fakePermissionChecker{err: errors.New("db unavailable")}
	mw := RequirePermission(checker, "transactions:write")(handler)

	req := withAuthenticatedContext(httptest.NewRequest(http.MethodPost, "/transfers", nil), uuid.New(), uuid.New(), uuid.New())
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
	if *called {
		t.Fatal("expected the next handler to not be called")
	}
}

// TestRequirePermission_ChainedMiddleware_RequiresAll proves that
// chaining two RequirePermission calls gives AND semantics: a user
// missing either required permission is rejected.
func TestRequirePermission_ChainedMiddleware_RequiresAll(t *testing.T) {
	handler, called := testHandler()
	checker := &fakePermissionChecker{perms: []string{"transactions:read"}} // missing accounts:read
	mw := RequirePermission(checker, "transactions:read")(
		RequirePermission(checker, "accounts:read")(handler),
	)

	req := withAuthenticatedContext(httptest.NewRequest(http.MethodGet, "/transfers", nil), uuid.New(), uuid.New(), uuid.New())
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d (missing one of two required permissions)", rec.Code, http.StatusForbidden)
	}
	if *called {
		t.Fatal("expected the next handler to not be called")
	}
}