package webhook

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"math/big"
	"net/http"
	"time"
)


const DefaultMaxAttempts = 5


const (
	DefaultBaseBackoff = 30 * time.Second
	DefaultMaxBackoff  = 1 * time.Hour
)

// httpDoer is the subset of *http.Client this package needs -- an
// interface, not the concrete type, so delivery can be tested with a
// fake instead of a real HTTP server, the same pattern used for
// TokenValidator/Store/PermissionChecker elsewhere in this codebase.
type httpDoer interface {
	Do(req *http.Request) (*http.Response, error)
}


type Service struct {
	repo        *Repository
	httpClient  httpDoer
	maxAttempts int
	baseBackoff time.Duration
	maxBackoff  time.Duration
	logger      *slog.Logger
}

// Option configures optional Service behavior.
type Option func(*Service)

func WithMaxAttempts(n int) Option           { return func(s *Service) { s.maxAttempts = n } }
func WithBaseBackoff(d time.Duration) Option { return func(s *Service) { s.baseBackoff = d } }
func WithMaxBackoff(d time.Duration) Option  { return func(s *Service) { s.maxBackoff = d } }
func WithLogger(l *slog.Logger) Option       { return func(s *Service) { s.logger = l } }
func WithHTTPClient(c httpDoer) Option       { return func(s *Service) { s.httpClient = c } }

func NewService(repo *Repository, opts ...Option) *Service {
	s := &Service{
		repo:        repo,
		httpClient:  &http.Client{Timeout: 10 * time.Second},
		maxAttempts: DefaultMaxAttempts,
		baseBackoff: DefaultBaseBackoff,
		maxBackoff:  DefaultMaxBackoff,
		logger:      slog.Default(),
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// ProcessEvent delivers one already-claimed (status='processing') event
// and persists the outcome: MarkDelivered on success, ScheduleRetry with
// computed backoff on a retryable failure, or MoveToDeadLetter once
// maxAttempts is reached. Every outcome is written, so an event is never
// left stuck at 'processing' after this returns -- that status only ever
// exists for the brief window between ClaimPendingEvents and this
// function's completion.
func (s *Service) ProcessEvent(ctx context.Context, ev *Event) {
	deliverErr := s.deliver(ctx, ev)
	attempt := ev.AttemptCount + 1

	if deliverErr == nil {
		if err := s.repo.MarkDelivered(ctx, s.repo.db, ev.ID); err != nil {
			s.logger.Error("webhook: failed to mark event delivered", "event_id", ev.ID, "error", err)
		}
		return
	}

	if errors.Is(deliverErr, ErrNoEndpointConfigured) {
		// Nothing to retry toward -- leave it pending indefinitely
		// rather than burning through attempts against a destination
		// that doesn't exist. A future ScheduleRetry-based approach once
		// the tenant configures a URL would need its own trigger; for
		// now this just logs and moves on, matching the "not a delivery
		// failure" framing in ErrNoEndpointConfigured's doc comment.
		s.logger.Warn("webhook: no endpoint configured, skipping", "event_id", ev.ID, "tenant_id", ev.TenantID)
		return
	}

	if attempt >= s.maxAttempts {
		tx, err := s.repo.BeginTx(ctx, nil)
		if err != nil {
			s.logger.Error("webhook: begin tx for dead-letter", "event_id", ev.ID, "error", err)
			return
		}
		defer func() { _ = tx.Rollback() }()
		ev.AttemptCount = attempt
		if err := s.repo.MoveToDeadLetter(ctx, tx, ev, deliverErr.Error()); err != nil {
			s.logger.Error("webhook: move to dead letter failed", "event_id", ev.ID, "error", err)
			return
		}
		if err := tx.Commit(); err != nil {
			s.logger.Error("webhook: commit dead-letter transition", "event_id", ev.ID, "error", err)
			return
		}
		s.logger.Warn("webhook: event exhausted all attempts, moved to dead letter",
			"event_id", ev.ID, "tenant_id", ev.TenantID, "attempts", attempt, "error", deliverErr)
		return
	}

	nextRetryAt := time.Now().Add(s.backoffForAttempt(attempt))
	if err := s.repo.ScheduleRetry(ctx, s.repo.db, ev.ID, attempt, nextRetryAt, deliverErr.Error()); err != nil {
		s.logger.Error("webhook: schedule retry failed", "event_id", ev.ID, "error", err)
	}
}

// deliver performs exactly one HTTP delivery attempt: sign the payload,
// POST it with the signature and timestamp as headers, and treat
// anything outside 2xx as a failure worth retrying.
func (s *Service) deliver(ctx context.Context, ev *Event) error {
	url, secret, err := s.repo.GetTenantWebhookConfig(ctx, s.repo.db, ev.TenantID)
	if err != nil {
		return err // ErrNoEndpointConfigured or a real DB error, both handled by the caller
	}

	timestamp := time.Now().UTC()
	signature := SignPayload(secret, timestamp, ev.Payload)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(ev.Payload))
	if err != nil {
		return fmt.Errorf("webhook: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Transacta-Event-Type", ev.EventType)
	req.Header.Set("X-Transacta-Timestamp", fmt.Sprintf("%d", timestamp.Unix()))
	req.Header.Set("X-Transacta-Signature", "sha256="+signature)

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrDeliveryFailed, err)
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%w: received status %d", ErrDeliveryFailed, resp.StatusCode)
	}
	return nil
}

// backoffForAttempt computes exponential backoff with jitter: base *
// 2^(attempt-1), capped at maxBackoff, then randomized within +/-20% so
// that many events failing at once (e.g. a receiver's endpoint going
// down) don't all retry at the exact same instant and re-hammer it the
// moment it recovers.
func (s *Service) backoffForAttempt(attempt int) time.Duration {
	exp := math.Pow(2, float64(attempt-1))
	backoff := time.Duration(float64(s.baseBackoff) * exp)
	if backoff > s.maxBackoff || backoff <= 0 {
		backoff = s.maxBackoff
	}
	return jitter(backoff)
}

// jitter randomizes d within [0.8*d, 1.2*d] using crypto/rand -- not for
// security here, just because it was already imported and avoids pulling
// in math/rand/v2 for one call site.
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	spread := d / 5 // 20%
	n, err := rand.Int(rand.Reader, big.NewInt(int64(2*spread)))
	if err != nil {
		return d
	}
	return d - spread + time.Duration(n.Int64())
}