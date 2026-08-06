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


const (
	completeRetryAttempts = 3
	completeRetryBackoff  = 100 * time.Millisecond
)


type Store interface {
	ClaimOrGet(ctx context.Context, tenantID uuid.UUID, key, requestHash string, ttl time.Duration) (*Record, bool, error)
	Complete(ctx context.Context, tenantID uuid.UUID, key string, responseCode int, responseBody []byte) error
}

// Option configures Middleware's optional behavior.
type Option func(*options)

type options struct {
	logger *slog.Logger
}


func WithLogger(l *slog.Logger) Option {
	return func(o *options) { o.logger = l }
}


func Middleware(store Store, ttl time.Duration, opts ...Option) func(http.Handler) http.Handler {
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	o := options{logger: slog.Default()}
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

			rec, claimed, err := store.ClaimOrGet(r.Context(), tenantID, key, hash, ttl)
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

			// The response has already been sent to the client at this
			// point. If the handler mutated real state (e.g. moved
			// money), failing to persist "completed" here is not
			// cosmetic: this record's status stays 'processing', which
			// blocks retries with a 409 in the short term 
			completeWithRetry(context.Background(), store, o.logger, tenantID, key, rw.statusCode, rw.body.Bytes())
		})
	}
}


func completeWithRetry(ctx context.Context, store Store, logger *slog.Logger, tenantID uuid.UUID, key string, responseCode int, responseBody []byte) {
	var lastErr error
	for attempt := 1; attempt <= completeRetryAttempts; attempt++ {
		if err := store.Complete(ctx, tenantID, key, responseCode, responseBody); err != nil {
			lastErr = err
			logger.Warn("idempotency: failed to persist completed record, retrying",
				"tenant_id", tenantID, "key", key, "attempt", attempt, "error", err)
			time.Sleep(time.Duration(attempt) * completeRetryBackoff)
			continue
		}
		return
	}
	logger.Error("idempotency: exhausted retries persisting completed record -- "+
		"key remains stuck at 'processing' and will become silently reclaimable once its TTL expires",
		"tenant_id", tenantID, "key", key, "error", lastErr)
}

// handleExisting decides what to do when ClaimOrGet finds a live record
// instead of granting a fresh claim: reject a payload mismatch, reject an
// in-flight duplicate, or replay a completed response verbatim.
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

// responseRecorder captures the downstream handler's status code and body
// so it can be cached for future replays, while still writing through to
// the real client in real time 
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