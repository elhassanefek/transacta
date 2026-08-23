package metrics

// Package metrics defines Transacta's Prometheus metrics and exposes
// them via Handler for mounting at /metrics.
//
// Metrics are registered once, at package init, against Prometheus's
// default registry -- the standard promauto pattern. Any package that
// wants to record a metric imports this package directly and calls the
// relevant exported var (e.g. metrics.WebhookDeliveredTotal.Inc()), the
// same way a shared *slog.Logger gets passed around for logging. This is
// an observability/infrastructure dependency, not a domain one -- it
// doesn't violate the "core domain packages never import outward-facing
// packages" rule, since that rule is about business-logic layering
// (ledger must not depend on webhook/auth), not about instrumentation.

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	// HTTPRequestsTotal counts every HTTP request, labeled by method,
	// route (chi's route *pattern*, e.g. "/v1/transactions/{id}" -- never
	// the resolved path, which would create one time series per UUID
	// ever seen and blow up cardinality), and status code.
	HTTPRequestsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "transacta_http_requests_total",
			Help: "Total HTTP requests, labeled by method, route pattern, and status code.",
		},
		[]string{"method", "route", "status"},
	)

	// HTTPRequestDuration observes request latency, labeled by method and
	// route pattern (status is deliberately excluded here -- histograms
	// multiply their bucket count by every label combination, and status
	// adds cardinality without much analytical value for a latency
	// histogram specifically).
	HTTPRequestDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "transacta_http_request_duration_seconds",
			Help:    "HTTP request latency in seconds, labeled by method and route pattern.",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"method", "route"},
	)

	// WebhookDeliveredTotal counts events successfully delivered on some
	// attempt (not necessarily the first).
	WebhookDeliveredTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "transacta_webhook_delivered_total",
		Help: "Total webhook events successfully delivered.",
	})

	// WebhookRetriedTotal counts delivery attempts that failed and were
	// scheduled for another attempt (not yet dead-lettered).
	WebhookRetriedTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "transacta_webhook_retried_total",
		Help: "Total webhook delivery attempts that failed and were scheduled for retry.",
	})

	// WebhookDeadLetteredTotal counts events that exhausted every retry
	// attempt without a successful delivery. This is the number an
	// operator should alert on -- a nonzero, growing rate here means
	// real events are going permanently undelivered.
	WebhookDeadLetteredTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "transacta_webhook_dead_lettered_total",
		Help: "Total webhook events that exhausted all retry attempts and were moved to dead_letter_events.",
	})

	// WebhookSkippedNoEndpointTotal counts events left pending because
	// their tenant has no webhook_url configured -- distinct from a
	// delivery failure, since no attempt was actually made.
	WebhookSkippedNoEndpointTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "transacta_webhook_skipped_no_endpoint_total",
		Help: "Total webhook events skipped because the tenant has no configured delivery endpoint.",
	})
)

// Handler returns the HTTP handler to mount at /metrics. Deliberately
// unauthenticated in this project -- Prometheus scrapers don't carry a
// bearer token by default -- so in a real deployment this endpoint
// should be restricted at the network layer (internal-only ingress,
// firewall rule), not exposed on the same public listener without
// additional protection.
func Handler() http.Handler {
	return promhttp.Handler()
}