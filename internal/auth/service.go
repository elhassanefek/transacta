package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
)


const bcryptCost = 12


const DefaultAccessTokenTTL = 15 * time.Minute


const DefaultRefreshTokenTTL = 7 * 24 * time.Hour


type Claims struct {
	UserID   string `json:"user_id"`
	TenantID string `json:"tenant_id"`
	RoleID   string `json:"role_id"`
	jwt.RegisteredClaims
}


type Service struct {
	repo            *Repository
	jwtSecret       []byte
	accessTokenTTL  time.Duration
	refreshTokenTTL time.Duration
}

// Option configures optional Service behavior.
type Option func(*Service)

func WithAccessTokenTTL(d time.Duration) Option  { return func(s *Service) { s.accessTokenTTL = d } }
func WithRefreshTokenTTL(d time.Duration) Option { return func(s *Service) { s.refreshTokenTTL = d } }


func NewService(repo *Repository, jwtSecret []byte, opts ...Option) *Service {
	s := &Service{
		repo:            repo,
		jwtSecret:       jwtSecret,
		accessTokenTTL:  DefaultAccessTokenTTL,
		refreshTokenTTL: DefaultRefreshTokenTTL,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}


func (s *Service) Register(ctx context.Context, tenantID, roleID uuid.UUID, email, password string) (*User, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcryptCost)
	if err != nil {
		return nil, fmt.Errorf("auth: hash password: %w", err)
	}
	return s.repo.CreateUser(ctx, s.repo.db, tenantID, roleID, email, string(hash))
}


type AuthResult struct {
	AccessToken      string
	RefreshToken     string
	AccessExpiresAt  time.Time
	RefreshExpiresAt time.Time
}


func (s *Service) Login(ctx context.Context, tenantID uuid.UUID, email, password string) (*AuthResult, error) {
	user, err := s.repo.GetUserByEmail(ctx, s.repo.db, tenantID, email)
	if errors.Is(err, ErrUserNotFound) {
		// Run bcrypt anyway against a fixed dummy hash so a nonexistent
		// email doesn't respond measurably faster than a wrong password
		//  a basic timing-attack mitigation for user enumeration.
		_ = bcrypt.CompareHashAndPassword([]byte(dummyBcryptHash), []byte(password))
		return nil, ErrInvalidCredentials
	}
	if err != nil {
		return nil, err
	}

	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(password)); err != nil {
		return nil, ErrInvalidCredentials
	}
	if user.Status == UserStatusDisabled {
		return nil, ErrUserDisabled
	}

	return s.issueTokenPair(ctx, s.repo.db, user)
}

// dummyBcryptHash is a valid bcrypt hash of an arbitrary fixed string,
// used only to give Login's nonexistent-user path a comparably expensive
// operation to perform. It does not correspond to any real credential.
const dummyBcryptHash = "$2a$12$C6UzMDM.H6dfI/f/IKcEeO2Fv/eLwWGdWMi4X1zXK4H8xh0.0V6i2"


func (s *Service) Refresh(ctx context.Context, rawRefreshToken string) (*AuthResult, error) {
	tokenHash := hashToken(rawRefreshToken)

	existing, err := s.repo.GetRefreshTokenByHash(ctx, s.repo.db, tokenHash)
	if err != nil {
		return nil, err // ErrRefreshTokenNotFound as-is
	}

	if existing.Status == RefreshTokenStatusRotated || existing.Status == RefreshTokenStatusRevoked {
		// Reuse of an already-superseded token: treat as compromise,
		// kill every active session for this user.
		if revokeErr := s.repo.RevokeAllUserRefreshTokens(ctx, s.repo.db, existing.UserID); revokeErr != nil {
			return nil, fmt.Errorf("auth: revoke sessions after detected reuse: %w", revokeErr)
		}
		return nil, ErrRefreshTokenInvalid
	}
	if existing.ExpiresAt.Before(time.Now()) {
		return nil, ErrRefreshTokenInvalid
	}

	user, err := s.repo.GetUserByID(ctx, s.repo.db, existing.TenantID, existing.UserID)
	if err != nil {
		return nil, err
	}
	if user.Status == UserStatusDisabled {
		return nil, ErrUserDisabled
	}

	tx, err := s.repo.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return nil, fmt.Errorf("auth: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	newRawToken, newTokenHash, err := generateRawToken()
	if err != nil {
		return nil, err
	}
	newRecord, err := s.repo.CreateRefreshToken(ctx, tx, user.ID, user.TenantID, newTokenHash, s.refreshTokenTTL)
	if err != nil {
		return nil, err
	}
	if err := s.repo.RotateRefreshToken(ctx, tx, existing.ID, newRecord.ID); err != nil {
		// Someone else won a race rotating this exact token between our
		// the new token we just created
		// gets rolled back with the transaction, nothing persists.
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("auth: commit refresh: %w", err)
	}

	accessToken, accessExpiresAt, err := s.generateAccessToken(user)
	if err != nil {
		return nil, err
	}
	return &AuthResult{
		AccessToken:      accessToken,
		RefreshToken:     newRawToken,
		AccessExpiresAt:  accessExpiresAt,
		RefreshExpiresAt: newRecord.ExpiresAt,
	}, nil
}


func (s *Service) Logout(ctx context.Context, rawRefreshToken string) error {
	tokenHash := hashToken(rawRefreshToken)
	existing, err := s.repo.GetRefreshTokenByHash(ctx, s.repo.db, tokenHash)
	if err != nil {
		return err
	}
	return s.repo.RevokeRefreshToken(ctx, s.repo.db, existing.ID)
}

func (s *Service) GetUserPermissions(ctx context.Context, tenantID, userID uuid.UUID) ([]string, error) {
	return s.repo.GetUserPermissions(ctx, s.repo.db, tenantID, userID)
}

func (s *Service) ValidateAccessToken(tokenString string) (*Claims, error) {
	claims := &Claims{}
	token, err := jwt.ParseWithClaims(tokenString, claims, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("auth: unexpected signing method %v", t.Header["alg"])
		}
		return s.jwtSecret, nil
	})
	if err != nil {
		return nil, fmt.Errorf("auth: invalid access token: %w", err)
	}
	if !token.Valid {
		return nil, errors.New("auth: invalid access token")
	}
	return claims, nil
}

func (s *Service) issueTokenPair(ctx context.Context, db DBTX, user *User) (*AuthResult, error) {
	accessToken, accessExpiresAt, err := s.generateAccessToken(user)
	if err != nil {
		return nil, err
	}
	rawRefreshToken, tokenHash, err := generateRawToken()
	if err != nil {
		return nil, err
	}
	rt, err := s.repo.CreateRefreshToken(ctx, db, user.ID, user.TenantID, tokenHash, s.refreshTokenTTL)
	if err != nil {
		return nil, err
	}
	return &AuthResult{
		AccessToken:      accessToken,
		RefreshToken:     rawRefreshToken,
		AccessExpiresAt:  accessExpiresAt,
		RefreshExpiresAt: rt.ExpiresAt,
	}, nil
}

func (s *Service) generateAccessToken(user *User) (string, time.Time, error) {
	expiresAt := time.Now().UTC().Add(s.accessTokenTTL)
	claims := Claims{
		UserID:   user.ID.String(),
		TenantID: user.TenantID.String(),
		RoleID:   user.RoleID.String(),
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   user.ID.String(),
			ExpiresAt: jwt.NewNumericDate(expiresAt),
			IssuedAt:  jwt.NewNumericDate(time.Now().UTC()),
		},
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := token.SignedString(s.jwtSecret)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("auth: sign access token: %w", err)
	}
	return signed, expiresAt, nil
}


func generateRawToken() (raw string, hash string, err error) {
	buf := make([]byte, 32) // 256 bits
	if _, err := rand.Read(buf); err != nil {
		return "", "", fmt.Errorf("auth: generate refresh token: %w", err)
	}
	raw = hex.EncodeToString(buf)
	return raw, hashToken(raw), nil
}


func hashToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}
