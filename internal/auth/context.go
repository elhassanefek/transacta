package auth

import "context"

type claimsContextKey struct{}

var claimsKey = claimsContextKey{}

// WithClaims returns a new context carrying the given validated Claims.
// Called by middleware after ValidateAccessToken succeeds.
func WithClaims(ctx context.Context, claims *Claims) context.Context {
	return context.WithValue(ctx, claimsKey, claims)
}

// ClaimsFromContext reads the Claims set by WithClaims. ok is false if no

func ClaimsFromContext(ctx context.Context) (*Claims, bool) {
	c, ok := ctx.Value(claimsKey).(*Claims)
	return c, ok
}