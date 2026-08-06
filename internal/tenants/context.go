package tenants

import (
	"context"

	"github.com/google/uuid"
)

type contextKey struct{}

var tenantIDKey = contextKey{}

// WithTenantID returns a new context carrying the given tenant ID. Called
// by whatever authenticates the request -- M4's auth middleware, once it
// exists -- after resolving which tenant is making the call.
func WithTenantID(ctx context.Context, tenantID uuid.UUID) context.Context {
	return context.WithValue(ctx, tenantIDKey, tenantID)
}


func FromContext(ctx context.Context) (uuid.UUID, bool) {
	v, ok := ctx.Value(tenantIDKey).(uuid.UUID)
	return v, ok
}