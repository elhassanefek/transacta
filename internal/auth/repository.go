package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)


type DBTX interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}


type Repository struct {
	db *sql.DB
}

func NewRepository(db *sql.DB) *Repository {
	return &Repository{db: db}
}

func (r *Repository) BeginTx(ctx context.Context, opts *sql.TxOptions) (*sql.Tx, error) {
	return r.db.BeginTx(ctx, opts)
}

// --- Roles ---


func (r *Repository) GetRoleByName(ctx context.Context, q DBTX, name string) (*Role, error) {
	const query = `SELECT id, name, description, created_at FROM roles WHERE name = $1`
	var role Role
	err := q.QueryRowContext(ctx, query, name).Scan(&role.ID, &role.Name, &role.Description, &role.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrRoleNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("auth: get role: %w", err)
	}
	return &role, nil
}


func (r *Repository) GetUserPermissions(ctx context.Context, q DBTX, tenantID, userID uuid.UUID) ([]string, error) {
	const query = `
		SELECT p.name
		FROM users u
		JOIN role_permissions rp ON rp.role_id = u.role_id
		JOIN permissions p ON p.id = rp.permission_id
		WHERE u.id = $1 AND u.tenant_id = $2`
	rows, err := q.QueryContext(ctx, query, userID, tenantID)
	if err != nil {
		return nil, fmt.Errorf("auth: get user permissions: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var perms []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("auth: scan permission: %w", err)
		}
		perms = append(perms, name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("auth: get user permissions: %w", err)
	}
	return perms, nil
}

// --- Users ---


func (r *Repository) CreateUser(ctx context.Context, q DBTX, tenantID, roleID uuid.UUID, email, passwordHash string) (*User, error) {
	u := &User{TenantID: tenantID, RoleID: roleID, Email: email, PasswordHash: passwordHash, Status: UserStatusActive}
	const query = `
		INSERT INTO users (tenant_id, role_id, email, password_hash, status)
		VALUES ($1, $2, $3, $4, 'active')
		RETURNING id, created_at`
	err := q.QueryRowContext(ctx, query, tenantID, roleID, email, passwordHash).Scan(&u.ID, &u.CreatedAt)
	if err != nil {
		if isUniqueViolation(err, "uq_users_tenant_lower_email") {
			return nil, ErrEmailAlreadyExists
		}
		return nil, fmt.Errorf("auth: create user: %w", err)
	}
	return u, nil
}


func (r *Repository) GetUserByEmail(ctx context.Context, q DBTX, tenantID uuid.UUID, email string) (*User, error) {
	const query = `
		SELECT id, tenant_id, role_id, email, password_hash, status, created_at
		FROM users WHERE tenant_id = $1 AND LOWER(email) = LOWER($2)`
	return scanUser(q.QueryRowContext(ctx, query, tenantID, email))
}


func (r *Repository) GetUserByID(ctx context.Context, q DBTX, tenantID, userID uuid.UUID) (*User, error) {
	const query = `
		SELECT id, tenant_id, role_id, email, password_hash, status, created_at
		FROM users WHERE tenant_id = $1 AND id = $2`
	return scanUser(q.QueryRowContext(ctx, query, tenantID, userID))
}

func scanUser(row *sql.Row) (*User, error) {
	var u User
	err := row.Scan(&u.ID, &u.TenantID, &u.RoleID, &u.Email, &u.PasswordHash, &u.Status, &u.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrUserNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("auth: scan user: %w", err)
	}
	return &u, nil
}

// --- Refresh tokens ---


func (r *Repository) CreateRefreshToken(ctx context.Context, q DBTX, userID, tenantID uuid.UUID, tokenHash string, ttl time.Duration) (*RefreshToken, error) {
	rt := &RefreshToken{UserID: userID, TenantID: tenantID, TokenHash: tokenHash, Status: RefreshTokenStatusActive}
	expiresAt := time.Now().UTC().Add(ttl)
	const query = `
		INSERT INTO refresh_tokens (user_id, tenant_id, token_hash, status, expires_at)
		VALUES ($1, $2, $3, 'active', $4)
		RETURNING id, created_at, expires_at`
	err := q.QueryRowContext(ctx, query, userID, tenantID, tokenHash, expiresAt).Scan(&rt.ID, &rt.CreatedAt, &rt.ExpiresAt)
	if err != nil {
		return nil, fmt.Errorf("auth: create refresh token: %w", err)
	}
	return rt, nil
}


func (r *Repository) GetRefreshTokenByHash(ctx context.Context, q DBTX, tokenHash string) (*RefreshToken, error) {
	const query = `
		SELECT id, user_id, tenant_id, token_hash, status, replaced_by, created_at, expires_at
		FROM refresh_tokens WHERE token_hash = $1`
	var rt RefreshToken
	err := q.QueryRowContext(ctx, query, tokenHash).Scan(
		&rt.ID, &rt.UserID, &rt.TenantID, &rt.TokenHash, &rt.Status, &rt.ReplacedBy, &rt.CreatedAt, &rt.ExpiresAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrRefreshTokenNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("auth: get refresh token: %w", err)
	}
	return &rt, nil
}


func (r *Repository) RotateRefreshToken(ctx context.Context, tx *sql.Tx, oldTokenID, newTokenID uuid.UUID) error {
	const query = `
		UPDATE refresh_tokens
		SET status = 'rotated', replaced_by = $2
		WHERE id = $1 AND status = 'active' AND expires_at > now()`
	res, err := tx.ExecContext(ctx, query, oldTokenID, newTokenID)
	if err != nil {
		return fmt.Errorf("auth: rotate refresh token: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("auth: rotate refresh token: %w", err)
	}
	if n == 0 {
		return ErrRefreshTokenInvalid
	}
	return nil
}


func (r *Repository) RevokeRefreshToken(ctx context.Context, q DBTX, tokenID uuid.UUID) error {
	const query = `UPDATE refresh_tokens SET status = 'revoked' WHERE id = $1 AND status = 'active'`
	if _, err := q.ExecContext(ctx, query, tokenID); err != nil {
		return fmt.Errorf("auth: revoke refresh token: %w", err)
	}
	return nil
}


func (r *Repository) RevokeAllUserRefreshTokens(ctx context.Context, q DBTX, userID uuid.UUID) error {
	const query = `UPDATE refresh_tokens SET status = 'revoked' WHERE user_id = $1 AND status = 'active'`
	if _, err := q.ExecContext(ctx, query, userID); err != nil {
		return fmt.Errorf("auth: revoke all refresh tokens: %w", err)
	}
	return nil
}


func isUniqueViolation(err error, constraintName string) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return pgErr.Code == "23505" && pgErr.ConstraintName == constraintName
}