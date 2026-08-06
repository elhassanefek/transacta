package idempotency

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/elhassanefek/transacta/internal/tenants"
)


const HeaderKey = "Idempotency-Key"


const DefaultTTL = 24 * time.Hour

// DefaultProcessingLease is how long a claimed-but-not-yet-completed

const DefaultProcessingLease = 30 * time.Second


const (
	completeRetryAttempts = 3
	completeRetryBackoff  = 100 * time.Millisecond
)


type Store interface {
	ClaimOrGet(ctx context.Context, tenantID uuid.UUID, key, requestHash string, leaseTTL time.Duration) (*Record, bool, error)
	Complete(ctx context.Context, tenantID uuid.UUID, key string, responseCode int, responseBody []byte, retentionTTL time.Duration) error
}

// Option configures Middleware's optional behavior.
type Option func(*options)

type options struct {
	logger          *slog.Logger
	processingLease time.Duration
}


func WithLogger(l *slog.Logger) Option {
	return func(o *options) { o.logger = l }
}


func WithProcessingLease(d time.Duration) Option {
	return func(o *options) { o.processingLease = d }
}


func Middleware(store Store, retentionTTL time.Duration, opts ...Option) func(http.Handler) http.Handler {
	if retentionTTL <= 0 {
		retentionTTL = DefaultTTL
	}
	o := options{logger: slog.Default(), processingLease: DefaultProcessingLease}
	for _, opt := range opts {
		opt(&o)
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := r.Header.Get(HeaderKey)
			if key == "" {
				http.Error(w, ErrMissingKey.Error(), http.StatusBadRequest)
				return
			}

			tenantID, ok := tenants.FromContext(r.Context())
			if !ok {
				http.Error(w, "idempotency: no tenant in request context", http.StatusInternalServerError)
				return
			}

			bodyBytes, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, "failed to read request body", http.StatusBadRequest)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(bodyBytes))

			hash := requestHash(r.Method, r.URL.Path, bodyBytes)

			rec, claimed, err := store.ClaimOrGet(r.Context(), tenantID, key, hash, o.processingLease)
			if err != nil {
				http.Error(w, "idempotency check failed", http.StatusInternalServerError)
				return
			}

			if !claimed {
				handleExisting(w, rec, hash)
				return
			}

			rw := &responseRecorder{ResponseWriter: w, statusCode: http.StatusOK}
			next.ServeHTTP(rw, r)

			
			completeWithRetry(context.Background(), store, o.logger, tenantID, key, rw.statusCode, rw.body.Bytes(), retentionTTL)
		})
	}
}


func completeWithRetry(ctx context.Context, store Store, logger *slog.Logger, tenantID uuid.UUID, key string, responseCode int, responseBody []byte, retentionTTL time.Duration) {
	var lastErr error
	for attempt := 1; attempt <= completeRetryAttempts; attempt++ {
		if err := store.Complete(ctx, tenantID, key, responseCode, responseBody, retentionTTL); err != nil {
			lastErr = err
			logger.Warn("idempotency: failed to persist completed record, retrying",
				"tenant_id", tenantID, "key", key, "attempt", attempt, "error", err)
			time.Sleep(time.Duration(attempt) * completeRetryBackoff)
			continue
		}
		return
	}
	logger.Error("idempotency: exhausted retries persisting completed record -- "+
		"key remains stuck at 'processing' until its processing lease expires",
		"tenant_id", tenantID, "key", key, "error", lastErr)
}


func handleExisting(w http.ResponseWriter, rec *Record, hash string) {
	if rec.RequestHash != hash {
		http.Error(w, ErrKeyReused.Error(), http.StatusUnprocessableEntity)
		return
	}
	if rec.Status == StatusProcessing {
		http.Error(w, ErrInFlight.Error(), http.StatusConflict)
		return
	}
	w.Header().Set("Idempotency-Replayed", "true")
	code := http.StatusOK
	if rec.ResponseCode != nil {
		code = *rec.ResponseCode
	}
	w.WriteHeader(code)
	_, _ = w.Write(rec.ResponseBody)
}

func requestHash(method, path string, body []byte) string {
	h := sha256.New()
	_, _ = h.Write([]byte(method))
	_, _ = h.Write([]byte(path))
	_, _ = h.Write(body)
	return hex.EncodeToString(h.Sum(nil))
}


type responseRecorder struct {
	http.ResponseWriter
	statusCode int
	body       bytes.Buffer
	wroteHead  bool
}

func (r *responseRecorder) WriteHeader(code int) {
	r.statusCode = code
	r.wroteHead = true
	r.ResponseWriter.WriteHeader(code)
}

func (r *responseRecorder) Write(b []byte) (int, error) {
	if !r.wroteHead {
		r.statusCode = http.StatusOK
	}
	_, _ = r.body.Write(b)
	return r.ResponseWriter.Write(b)
}