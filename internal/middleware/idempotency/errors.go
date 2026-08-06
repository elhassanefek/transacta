package idempotency

import "errors"

var (
	
	ErrMissingKey = errors.New("idempotency: Idempotency-Key header is required")

	
	ErrKeyReused = errors.New("idempotency: key reused with a different request payload")

	// ErrInFlight is returned when a request with this key is still
	// being processed by a concurrent request -- the caller should
	// retry later rather than assume failure.
	ErrInFlight = errors.New("idempotency: a request with this key is still being processed")
)