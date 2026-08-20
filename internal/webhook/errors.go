package webhook

import "errors"

var (
	
	ErrEventNotFound = errors.New("webhook: event not found")

	
	ErrNoEndpointConfigured = errors.New("webhook: tenant has no webhook endpoint configured")

	
	ErrDeliveryFailed = errors.New("webhook: delivery attempt failed")

	
	ErrMaxAttemptsExceeded = errors.New("webhook: maximum delivery attempts exceeded")
)