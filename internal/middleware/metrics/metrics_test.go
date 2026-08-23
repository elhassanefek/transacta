package metrics

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	appmetrics "github.com/elhassanefek/transacta/internal/metrics"
)

// counterValue reads the current value of a single-combination counter
// from a CounterVec, for asserting on in tests without scraping the
// whole /metrics text output.
func counterValue(t *testing.T, vec *prometheus.CounterVec, labels ...string) float64 {
	t.Helper()
	m := &dto.Metric{}
	c, err := vec.GetMetricWithLabelValues(labels...)
	if err != nil {
		t.Fatalf("GetMetricWithLabelValues: %v", err)
	}
	if err := c.Write(m); err != nil {
		t.Fatalf("write metric: %v", err)
	}
	return m.GetCounter().GetValue()
}

func TestMiddleware_RecordsRequestWithRoutePattern_NotResolvedPath(t *testing.T) {
	r := chi.NewRouter()
	r.Use(Middleware)
	r.Get("/v1/transactions/{id}", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	before := counterValue(t, appmetrics.HTTPRequestsTotal, http.MethodGet, "/v1/transactions/{id}", "200")

	req := httptest.NewRequest(http.MethodGet, "/v1/transactions/11111111-1111-1111-1111-111111111111", nil)
	r.ServeHTTP(httptest.NewRecorder(), req)

	after := counterValue(t, appmetrics.HTTPRequestsTotal, http.MethodGet, "/v1/transactions/{id}", "200")
	if after != before+1 {
		t.Fatalf("counter for route pattern = %v, want %v -- request must be recorded under the PATTERN, not the resolved path with a real UUID in it", after, before+1)
	}
}

func TestMiddleware_RecordsUnmatchedRouteFor404(t *testing.T) {
	r := chi.NewRouter()
	r.Use(Middleware)
	r.Get("/known", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })

	before := counterValue(t, appmetrics.HTTPRequestsTotal, http.MethodGet, "unmatched", "404")

	req := httptest.NewRequest(http.MethodGet, "/this-route-does-not-exist", nil)
	r.ServeHTTP(httptest.NewRecorder(), req)

	after := counterValue(t, appmetrics.HTTPRequestsTotal, http.MethodGet, "unmatched", "404")
	if after != before+1 {
		t.Fatalf("counter for unmatched route = %v, want %v", after, before+1)
	}
}

func TestMiddleware_MountedBeforeRecoverer_RecordsActualRecoveredStatus(t *testing.T) {
	r := chi.NewRouter()
	r.Use(chimw.RequestID)
	r.Use(Middleware)
	r.Use(chimw.Recoverer)
	r.Get("/panics-metrics-test", func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	})

	before := counterValue(t, appmetrics.HTTPRequestsTotal, http.MethodGet, "/panics-metrics-test", "500")

	req := httptest.NewRequest(http.MethodGet, "/panics-metrics-test", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("client-visible status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}

	after := counterValue(t, appmetrics.HTTPRequestsTotal, http.MethodGet, "/panics-metrics-test", "500")
	if after != before+1 {
		t.Fatalf("counter for recovered-panic route = %v, want %v -- mounting before Recoverer must record the real 500, not a 0 status", after, before+1)
	}
}