package idempotency

import "errors"

var (
	
	ErrMissingKey = errors.New("idempotency: Idempotency-Key header is required")

	
	ErrKeyReused = errors.New("idempotency: key reused with a different request payload")

	// ErrInFlight is returned when a request with this key is still
	// being processed by a concurrent request -- the caller should
	// retry later rather than assume failure.
	ErrInFlight = errors.New("idempotency: a request with this key is still being processed")
	
	// ErrClaimSuperseded is returned by Repository.Complete when the
	// caller's claim has since been reclaimed by another server (the
	// caller's processing lease expired before it finished, another
	// server took over, and possibly already completed the request).
	// This is not a transient failure -- retrying will never succeed,
	// since the fencing check that produced this error can only get
	// further out of date, never back in sync.
	ErrClaimSuperseded = errors.New("idempotency: claim was superseded by another server before this result could be persisted")
)