package tenants

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/google/uuid"
)


type DBTX interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}


type Repository struct {
	db *sql.DB
}

func NewRepository(db *sql.DB) *Repository {
	return &Repository{db: db}
}


func (r *Repository) VerifyAPIKey(ctx context.Context, q DBTX, rawKey string) (uuid.UUID, error) {
	hash := HashAPIKey(rawKey)
	const query = `SELECT id FROM tenants WHERE api_key_hash = $1`
	var tenantID uuid.UUID
	err := q.QueryRowContext(ctx, query, hash).Scan(&tenantID)
	if errors.Is(err, sql.ErrNoRows) {
		return uuid.Nil, ErrInvalidAPIKey
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("tenants: verify api key: %w", err)
	}
	return tenantID, nil
}


func HashAPIKey(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

