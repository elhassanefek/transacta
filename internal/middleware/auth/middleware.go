package auth

import (
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/elhassanefek/transacta/internal/auth"
	"github.com/elhassanefek/transacta/internal/tenants"
)


type TokenValidator interface {
	ValidateAccessToken(tokenString string) (*auth.Claims, error)
}


func Middleware(validator TokenValidator) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			header := r.Header.Get("Authorization")
			const prefix = "Bearer "
			if !strings.HasPrefix(header, prefix) || len(header) <= len(prefix) {
				http.Error(w, "missing or malformed Authorization header", http.StatusUnauthorized)
				return
			}
			tokenString := strings.TrimPrefix(header, prefix)

			claims, err := validator.ValidateAccessToken(tokenString)
			if err != nil {
				http.Error(w, "invalid or expired access token", http.StatusUnauthorized)
				return
			}

			tenantID, err := uuid.Parse(claims.TenantID)
			if err != nil {
				// A malformed tenant_id claim on an otherwise
				// signature-valid token means something is wrong with
				// token issuance itself, not with this request -- 401
				// still, but distinct from an ordinary bad/expired token
				// if this ever needs its own alerting.
				http.Error(w, "token contains an invalid tenant claim", http.StatusUnauthorized)
				return
			}

			ctx := tenants.WithTenantID(r.Context(), tenantID)
			ctx = auth.WithClaims(ctx, claims)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}