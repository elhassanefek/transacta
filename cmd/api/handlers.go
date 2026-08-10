package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/google/uuid"

	"github.com/elhassanefek/transacta/internal/auth"
	"github.com/elhassanefek/transacta/internal/ledger"
	"github.com/elhassanefek/transacta/internal/tenants"
)

// --- Tenant API-key gate (Register only) ---


func requireTenantAPIKey(tenantRepo *tenants.Repository, db *sql.DB) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rawKey := r.Header.Get("X-API-Key")
			if rawKey == "" {
				http.Error(w, "X-API-Key header required", http.StatusUnauthorized)
				return
			}
			tenantID, err := tenantRepo.VerifyAPIKey(r.Context(), db, rawKey)
			if err != nil {
				http.Error(w, "invalid API key", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r.WithContext(tenants.WithTenantID(r.Context(), tenantID)))
		})
	}
}

// --- Register ---

type registerRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
	RoleName string `json:"role_name"`
}


func registerHandler(authSvc *auth.Service, authRepo *auth.Repository, db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tenantID, ok := tenants.FromContext(r.Context())
		if !ok {
			http.Error(w, "no tenant in request context", http.StatusInternalServerError)
			return
		}

		var req registerRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid JSON body", http.StatusBadRequest)
			return
		}
		if req.RoleName == "" {
			req.RoleName = "viewer"
		}

		role, err := authRepo.GetRoleByName(r.Context(), db, req.RoleName)
		if err != nil {
			http.Error(w, fmt.Sprintf("unknown role: %q", req.RoleName), http.StatusBadRequest)
			return
		}

		user, err := authSvc.Register(r.Context(), tenantID, role.ID, req.Email, req.Password)
		if err != nil {
			writeAuthError(w, err)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"user_id": user.ID.String(),
			"email":   user.Email,
		})
	}
}

// --- Login / Refresh / Logout ---

type loginRequest struct {
	TenantID string `json:"tenant_id"`
	Email    string `json:"email"`
	Password string `json:"password"`
}


func loginHandler(authSvc *auth.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req loginRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid JSON body", http.StatusBadRequest)
			return
		}
		tenantID, err := uuid.Parse(req.TenantID)
		if err != nil {
			http.Error(w, "invalid tenant_id", http.StatusBadRequest)
			return
		}

		result, err := authSvc.Login(r.Context(), tenantID, req.Email, req.Password)
		if err != nil {
			writeAuthError(w, err)
			return
		}
		writeAuthResult(w, http.StatusOK, result)
	}
}

type refreshRequest struct {
	RefreshToken string `json:"refresh_token"`
}

func refreshHandler(authSvc *auth.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req refreshRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid JSON body", http.StatusBadRequest)
			return
		}
		result, err := authSvc.Refresh(r.Context(), req.RefreshToken)
		if err != nil {
			writeAuthError(w, err)
			return
		}
		writeAuthResult(w, http.StatusOK, result)
	}
}

func logoutHandler(authSvc *auth.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req refreshRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid JSON body", http.StatusBadRequest)
			return
		}
		if err := authSvc.Logout(r.Context(), req.RefreshToken); err != nil {
			writeAuthError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func writeAuthResult(w http.ResponseWriter, status int, result *auth.AuthResult) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"access_token":       result.AccessToken,
		"refresh_token":      result.RefreshToken,
		"access_expires_at":  result.AccessExpiresAt,
		"refresh_expires_at": result.RefreshExpiresAt,
	})
}

func writeAuthError(w http.ResponseWriter, err error) {
	status := http.StatusUnprocessableEntity
	switch {
	case errors.Is(err, auth.ErrInvalidCredentials), errors.Is(err, auth.ErrRefreshTokenInvalid), errors.Is(err, auth.ErrRefreshTokenNotFound):
		status = http.StatusUnauthorized
	case errors.Is(err, auth.ErrUserDisabled):
		status = http.StatusForbidden
	case errors.Is(err, auth.ErrEmailAlreadyExists):
		status = http.StatusConflict
	case errors.Is(err, auth.ErrRoleNotFound), errors.Is(err, auth.ErrUserNotFound):
		status = http.StatusNotFound
	}
	http.Error(w, err.Error(), status)
}

// --- Transfers ---

type transferEntryRequest struct {
	AccountID   string `json:"account_id"`
	AmountMinor int64  `json:"amount_minor"`
}

type transferRequest struct {
	Entries []transferEntryRequest `json:"entries"`
}

type transferResponse struct {
	TransactionID string `json:"transaction_id"`
	Status        string `json:"status"`
}


func transferHandler(svc *ledger.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tenantID, ok := tenants.FromContext(r.Context())
		if !ok {
			http.Error(w, "no tenant in request context", http.StatusInternalServerError)
			return
		}

		var req transferRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid JSON body", http.StatusBadRequest)
			return
		}

		entries := make([]ledger.EntryInput, 0, len(req.Entries))
		for _, e := range req.Entries {
			accountID, err := uuid.Parse(e.AccountID)
			if err != nil {
				http.Error(w, fmt.Sprintf("invalid account_id: %q", e.AccountID), http.StatusBadRequest)
				return
			}
			entries = append(entries, ledger.EntryInput{
				AccountID:   accountID,
				AmountMinor: ledger.Money(e.AmountMinor),
			})
		}

		txn, _, err := svc.ExecuteTransfer(r.Context(), ledger.TransferRequest{
			TenantID: tenantID,
			Entries:  entries,
		})
		if err != nil {
			writeTransferError(w, err)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(transferResponse{
			TransactionID: txn.ID.String(),
			Status:        string(txn.Status),
		})
	}
}

func writeTransferError(w http.ResponseWriter, err error) {
	status := http.StatusUnprocessableEntity
	switch {
	case errors.Is(err, ledger.ErrInsufficientFunds):
		status = http.StatusConflict
	case errors.Is(err, ledger.ErrAccountNotFound), errors.Is(err, ledger.ErrTransactionNotFound):
		status = http.StatusNotFound
	case errors.Is(err, ledger.ErrUnbalancedEntries),
		errors.Is(err, ledger.ErrEmptyEntries),
		errors.Is(err, ledger.ErrZeroAmountEntry):
		status = http.StatusBadRequest
	}
	http.Error(w, err.Error(), status)
}