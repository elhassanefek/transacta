package auth

import (
	"time"

	"github.com/google/uuid"
)

type Role struct {
	ID          uuid.UUID
	Name        string
	Description *string
	CreatedAt   time.Time
}

type Permission struct {
	ID          uuid.UUID
	Name        string
	Description *string
	CreatedAt   time.Time
}
type UserStatus string
 
const (
	UserStatusActive   UserStatus = "active"
	UserStatusDisabled UserStatus = "disabled"
)

type User struct {
	ID           uuid.UUID
	TenantID     uuid.UUID
	RoleID       uuid.UUID
	Email        string
	PasswordHash string
	Status       UserStatus
	CreatedAt    time.Time
}
 
type RefreshTokenStatus string
 
const (
	RefreshTokenStatusActive  RefreshTokenStatus = "active"
	RefreshTokenStatusRotated RefreshTokenStatus = "rotated"
	RefreshTokenStatusRevoked RefreshTokenStatus = "revoked"
)

type RefreshToken struct {
	ID         uuid.UUID
	UserID     uuid.UUID
	TenantID   uuid.UUID
	TokenHash  string
	Status     RefreshTokenStatus
	ReplacedBy *uuid.UUID
	CreatedAt  time.Time
	ExpiresAt  time.Time
}