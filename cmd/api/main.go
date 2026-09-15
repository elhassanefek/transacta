package main

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/elhassanefek/transacta/internal/audit"
	"github.com/elhassanefek/transacta/internal/auth"
	"github.com/elhassanefek/transacta/internal/config"
	"github.com/elhassanefek/transacta/internal/ledger"
	appmetrics "github.com/elhassanefek/transacta/internal/metrics"
	authmw "github.com/elhassanefek/transacta/internal/middleware/auth"
	"github.com/elhassanefek/transacta/internal/middleware/idempotency"
	"github.com/elhassanefek/transacta/internal/middleware/logging"
	metricsmw "github.com/elhassanefek/transacta/internal/middleware/metrics"
	"github.com/elhassanefek/transacta/internal/middleware/recovery"
	"github.com/elhassanefek/transacta/internal/tenants"
	"github.com/elhassanefek/transacta/internal/webhook"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	cfg := config.Load()

	db, err := sql.Open("pgx", cfg.DatabaseDSN())
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
	auditRepo := audit.NewRepository(db)

	authRepo := auth.NewRepository(db)
	authSvc := auth.NewService(authRepo, []byte(cfg.JWTSecret), auth.WithAuditRecorder(auditRepo))

	tenantRepo := tenants.NewRepository(db)

	// webhookRepo is wired into ledgerSvc below via WithEventEnqueuer --
	// see internal/ledger/events.go for why ledger depends on a small
	// interface it defines itself, rather than importing this package
	// directly.
	webhookRepo := webhook.NewRepository(db)
	webhookSvc := webhook.NewService(webhookRepo, webhook.WithLogger(logger))
	webhookWorker := webhook.NewWorker(webhookRepo, webhookSvc, webhook.WithWorkerLogger(logger))

	ledgerRepo := ledger.NewRepository(db)
	ledgerSvc := ledger.NewService(ledgerRepo, ledger.WithEventEnqueuer(webhookRepo), ledger.WithAuditRecorder(auditRepo))

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
	r.Use(logging.Middleware(logger))
	r.Use(metricsmw.Middleware)
	// recovery must stay mounted here, closer to the handler than
	// logging -- see logging_test.go's
	// TestMiddleware_MountedBeforeRecoverer_LogsActualRecoveredStatus,
	// which pins this order: it's what lets logging's status recorder
	// observe the 500 that recovery writes on a panic, instead of
	// logging a zero/unwritten status because the panic skipped past
	// logging's own post-request log line entirely.
	r.Use(recovery.Middleware(logger))
	r.Use(middleware.Timeout(30 * time.Second))

	r.Get("/healthz", healthHandler(db))
	// Deliberately unauthenticated -- see appmetrics.Handler's doc
	// comment on why this needs network-level restriction in a real
	// deployment, not application-level auth.
	r.Handle("/metrics", appmetrics.Handler())

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

		r.With(
			authmw.Middleware(authSvc),
			authmw.RequirePermission(authSvc, "audit:read"),
		).Get("/audit-log", auditLogHandler(auditRepo))
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
