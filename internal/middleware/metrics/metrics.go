// Package metrics (middleware) records HTTP request metrics via
// internal/metrics. A separate package from internal/metrics itself --
// that one defines *what* gets measured and exposes /metrics; this one
// is the *where* measurement happens (wrapped around every request),
// same split as internal/middleware/logging vs. the logger it uses.
package metrics

import (
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/elhassanefek/transacta/internal/metrics"
)

// Middleware records HTTPRequestsTotal and HTTPRequestDuration for every
// request.
//
// Mounting order matters here, for the identical reason documented on
// internal/middleware/logging.Middleware: this must sit BEFORE
// Recoverer (i.e. mounted with r.Use() ahead of middleware.Recoverer),
// so a recovered panic's real status code (500, written by Recoverer)
// is what gets recorded -- not zero/unwritten, which is what this
// middleware would capture if a panic unwound through its deferred
// recording before Recoverer got a chance to write a response.
//
// The route label is read from chi's routing context AFTER next.ServeHTTP
// returns, not before -- chi only populates the matched route pattern
// once routing has actually resolved the request, the same reason
// status code can only be captured after the handler runs.
func Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, statusCode: http.StatusOK}

		next.ServeHTTP(rec, r)

		route := routePattern(r)
		duration := time.Since(start).Seconds()

		metrics.HTTPRequestsTotal.WithLabelValues(r.Method, route, strconv.Itoa(rec.statusCode)).Inc()
		metrics.HTTPRequestDuration.WithLabelValues(r.Method, route).Observe(duration)
	})
}

// routePattern returns chi's matched route pattern (e.g.
// "/v1/transactions/{id}"), or "unmatched" for requests that never hit a
// registered route (404s) -- both are low-cardinality, safe metric label
// values, unlike the raw resolved path.
func routePattern(r *http.Request) string {
	if rc := chi.RouteContext(r.Context()); rc != nil {
		if pattern := rc.RoutePattern(); pattern != "" {
			return pattern
		}
	}
	return "unmatched"
}

// statusRecorder captures the status code written by downstream
// handlers. Deliberately a separate small type from
// internal/middleware/logging's identical one, rather than a shared
// dependency between two otherwise-independent middleware packages --
// each observability concern here stays fully self-contained, matching
// this codebase's existing convention (idempotency and logging each
// already have their own local recorder type).
type statusRecorder struct {
	http.ResponseWriter
	statusCode int
	wroteHead  bool
}

func (r *statusRecorder) WriteHeader(code int) {
	r.statusCode = code
	r.wroteHead = true
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if !r.wroteHead {
		r.statusCode = http.StatusOK
	}
	return r.ResponseWriter.Write(b)
}