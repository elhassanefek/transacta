package auth

import (
	"context"
	"fmt"
	"net/http"

	"github.com/google/uuid"

	"github.com/elhassanefek/transacta/internal/auth"
	"github.com/elhassanefek/transacta/internal/tenants"
)


type PermissionChecker interface {
	GetUserPermissions(ctx context.Context, tenantID, userID uuid.UUID) ([]string, error)
}




func RequirePermission(checker PermissionChecker, permission string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			claims, ok := auth.ClaimsFromContext(r.Context())
			if !ok {
				// Middleware wasn't run first, or ran and failed to
				// populate context -- either way this is a wiring bug,
				// not a client error, hence 500 not 401.
				http.Error(w, "auth: no authenticated user in request context", http.StatusInternalServerError)
				return
			}
			userID, err := uuid.Parse(claims.UserID)
			if err != nil {
				http.Error(w, "token contains an invalid user claim", http.StatusUnauthorized)
				return
			}
			tenantID, ok := tenants.FromContext(r.Context())
			if !ok {
				http.Error(w, "auth: no tenant in request context", http.StatusInternalServerError)
				return
			}

			perms, err := checker.GetUserPermissions(r.Context(), tenantID, userID)
			if err != nil {
				http.Error(w, "permission check failed", http.StatusInternalServerError)
				return
			}

			if !hasPermission(perms, permission) {
				http.Error(w, fmt.Sprintf("missing required permission: %s", permission), http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func hasPermission(perms []string, want string) bool {
	for _, p := range perms {
		if p == want {
			return true
		}
	}
	return false
}
