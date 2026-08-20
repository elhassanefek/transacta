package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/elhassanefek/transacta/internal/auth"
	"github.com/elhassanefek/transacta/internal/ledger"
	authmw "github.com/elhassanefek/transacta/internal/middleware/auth"
	"github.com/elhassanefek/transacta/internal/middleware/idempotency"
	"github.com/elhassanefek/transacta/internal/tenants"
	"github.com/elhassanefek/transacta/internal/webhook"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	cfg := loadConfig()

	db, err := sql.Open("pgx", cfg.databaseDSN())
	if err != nil {
		logger.Error("open database", "error", err)
		os.Exit(1)
	}
	defer func() { _ = db.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		logger.Error("ping database", "error", err)
		os.Exit(1)
	}
	logger.Info("connected to database", "host", cfg.DBHost, "name", cfg.DBName)

	if cfg.JWTSecret == "" {
		logger.Error("JWT_SECRET is not set -- refusing to start with an unsigned/insecure JWT signing key")
		os.Exit(1)
	}

	idemRepo := idempotency.NewRepository(db)

	authRepo := auth.NewRepository(db)
	authSvc := auth.NewService(authRepo, []byte(cfg.JWTSecret))

	tenantRepo := tenants.NewRepository(db)

	// webhookRepo is wired into ledgerSvc below via WithEventEnqueuer --
	// see internal/ledger/events.go for why ledger depends on a small
	// interface it defines itself, rather than importing this package
	// directly.
	webhookRepo := webhook.NewRepository(db)
	webhookSvc := webhook.NewService(webhookRepo, webhook.WithLogger(logger))
	webhookWorker := webhook.NewWorker(webhookRepo, webhookSvc, webhook.WithWorkerLogger(logger))

	ledgerRepo := ledger.NewRepository(db)
	ledgerSvc := ledger.NewService(ledgerRepo, ledger.WithEventEnqueuer(webhookRepo))

	// The webhook worker runs independently of the HTTP server's request
	// lifecycle -- its own cancellable context, started before the
	// server begins accepting requests, stopped as part of the same
	// graceful-shutdown sequence below.
	workerCtx, workerCancel := context.WithCancel(context.Background())
	defer workerCancel()
	go webhookWorker.Run(workerCtx)
	logger.Info("webhook worker started")

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(30 * time.Second))

	r.Get("/healthz", healthHandler(db))

	r.Route("/v1", func(r chi.Router) {
		// Registration is gated by the tenant's own API key -- proving
		// "I'm allowed to provision users for this tenant," not "I'm
		// already a user of it" (there's no user yet).
		r.With(requireTenantAPIKey(tenantRepo, db)).Post("/auth/register", registerHandler(authSvc, authRepo, db))

		// Login/refresh/logout are public: the credential being checked
		// IS the request body (password, or the refresh token itself).
		r.Post("/auth/login", loginHandler(authSvc))
		r.Post("/auth/refresh", refreshHandler(authSvc))
		r.Post("/auth/logout", logoutHandler(authSvc))

		// Mutating ledger operations: authenticate (JWT) -> authorize
		// (RBAC) -> guard against duplicate execution (idempotency) ->
		// only then does the handler run.
		r.With(
			authmw.Middleware(authSvc),
			authmw.RequirePermission(authSvc, "accounts:write"),
			idempotency.Middleware(idemRepo, idempotency.DefaultTTL),
		).Post("/accounts", createAccountHandler(ledgerSvc))

		r.With(
			authmw.Middleware(authSvc),
			authmw.RequirePermission(authSvc, "accounts:read"),
		).Get("/accounts/{id}", getAccountHandler(ledgerSvc))

		r.With(
			authmw.Middleware(authSvc),
			authmw.RequirePermission(authSvc, "transactions:read"),
		).Get("/transactions/{id}", getTransactionHandler(ledgerSvc))

		r.With(
			authmw.Middleware(authSvc),
			authmw.RequirePermission(authSvc, "transactions:write"),
			idempotency.Middleware(idemRepo, idempotency.DefaultTTL),
		).Post("/transfers", transferHandler(ledgerSvc))

		r.With(
			authmw.Middleware(authSvc),
			authmw.RequirePermission(authSvc, "transactions:write"),
			idempotency.Middleware(idemRepo, idempotency.DefaultTTL),
		).Post("/transactions/pending", createPendingTransactionHandler(ledgerSvc))

		r.With(
			authmw.Middleware(authSvc),
			authmw.RequirePermission(authSvc, "transactions:write"),
			idempotency.Middleware(idemRepo, idempotency.DefaultTTL),
		).Post("/transactions/{id}/post", postPendingTransactionHandler(ledgerSvc))

		r.With(
			authmw.Middleware(authSvc),
			authmw.RequirePermission(authSvc, "transactions:write"),
			idempotency.Middleware(idemRepo, idempotency.DefaultTTL),
		).Post("/transactions/{id}/fail", failPendingTransactionHandler(ledgerSvc))
	})

	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           r,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		logger.Info("listening", "port", cfg.Port)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("server error", "error", err)
			os.Exit(1)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop

	logger.Info("shutting down")

	// Stop accepting new webhook delivery work before tearing down the
	// HTTP server -- order doesn't strictly matter here (they're
	// independent), but stopping the worker first means no new delivery
	// attempts start during the shutdown window.
	workerCancel()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("graceful shutdown failed", "error", err)
		os.Exit(1)
	}
}

// healthHandler reports 200 only if the database is actually reachable --
// a health check that doesn't check the DB isn't a health check.
func healthHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		ctx, cancel := context.WithTimeout(req.Context(), 2*time.Second)
		defer cancel()
		if err := db.PingContext(ctx); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"status":"unhealthy"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}
}

// config holds the same DB_* env vars the CI workflow's test job already
// sets, plus PORT and JWT_SECRET. No config library pulled in for six
// env vars.
type config struct {
	DBHost     string
	DBPort     string
	DBUser     string
	DBPassword string
	DBName     string
	DBSSLMode  string
	Port       string
	JWTSecret  string
}

func loadConfig() config {
	return config{
		DBHost:     getenv("DB_HOST", "localhost"),
		DBPort:     getenv("DB_PORT", "5432"),
		DBUser:     getenv("DB_USER", "transacta"),
		DBPassword: getenv("DB_PASSWORD", "transacta_dev"),
		DBName:     getenv("DB_NAME", "transacta"),
		DBSSLMode:  getenv("DB_SSLMODE", "disable"),
		Port:       getenv("PORT", "8080"),
		JWTSecret:  getenv("JWT_SECRET", ""),
	}
}

func (c config) databaseDSN() string {
	return fmt.Sprintf(
		"host=%s port=%s user=%s password=%s dbname=%s sslmode=%s",
		c.DBHost, c.DBPort, c.DBUser, c.DBPassword, c.DBName, c.DBSSLMode,
	)
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}