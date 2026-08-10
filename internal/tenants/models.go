package tenants

import (
	"time"

	"github.com/google/uuid"
)


type Tenant struct {
	ID         uuid.UUID
	Name       string
	APIKeyHash string
	CreatedAt  time.Time
}