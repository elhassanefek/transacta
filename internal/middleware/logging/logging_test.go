package logging

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
)

func newTestLogger() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewJSONHandler(&buf, nil)), &buf
}

func parseLogLine(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	var entry map[string]any
	line := strings.TrimSpace(buf.String())
	if line == "" {
		t.Fatal("expected a log line, got none")
	}
	if err := json.Unmarshal([]byte(line), &entry); err != nil {
		t.Fatalf("log output is not valid JSON: %v\noutput: %s", err, line)
	}
	return entry
}

func TestMiddleware_LogsMethodPathAndStatus(t *testing.T) {
	logger, buf := newTestLogger()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
	})
	mw := Middleware(logger)(handler)

	req := httptest.NewRequest(http.MethodPost, "/v1/transfers", nil)
	mw.ServeHTTP(httptest.NewRecorder(), req)

	entry := parseLogLine(t, buf)
	if entry["method"] != "POST" {
		t.Errorf("method = %v, want POST", entry["method"])
	}
	if entry["path"] != "/v1/transfers" {
		t.Errorf("path = %v, want /v1/transfers", entry["path"])
	}
	if entry["status"] != float64(http.StatusCreated) {
		t.Errorf("status = %v, want %d", entry["status"], http.StatusCreated)
	}
}

func TestMiddleware_DefaultsToStatus200WhenHandlerNeverCallsWriteHeader(t *testing.T) {
	logger, buf := newTestLogger()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok")) // no explicit WriteHeader call
	})
	mw := Middleware(logger)(handler)

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	mw.ServeHTTP(httptest.NewRecorder(), req)

	entry := parseLogLine(t, buf)
	if entry["status"] != float64(http.StatusOK) {
		t.Errorf("status = %v, want %d (Go's default when WriteHeader is never called explicitly)", entry["status"], http.StatusOK)
	}
}

func TestMiddleware_IncludesDurationAndRemoteAddr(t *testing.T) {
	logger, buf := newTestLogger()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mw := Middleware(logger)(handler)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "192.0.2.1:12345"
	mw.ServeHTTP(httptest.NewRecorder(), req)

	entry := parseLogLine(t, buf)
	if _, ok := entry["duration_ms"]; !ok {
		t.Error("expected duration_ms field in log output")
	}
	if entry["remote_addr"] != "192.0.2.1:12345" {
		t.Errorf("remote_addr = %v, want 192.0.2.1:12345", entry["remote_addr"])
	}
}

func TestMiddleware_IncludesRequestIDFromChiMiddleware(t *testing.T) {
	logger, buf := newTestLogger()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// Chain RequestID -> Middleware, same order documented as required.
	chain := chimw.RequestID(Middleware(logger)(handler))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	chain.ServeHTTP(httptest.NewRecorder(), req)

	entry := parseLogLine(t, buf)
	reqID, _ := entry["request_id"].(string)
	if reqID == "" {
		t.Error("expected a non-empty request_id when chained after chi's RequestID middleware")
	}
}

// TestMiddleware_MountedBeforeRecoverer_LogsActualRecoveredStatus proves
// the documented ordering requirement is actually correct: with
// Middleware mounted BEFORE Recoverer (Recoverer closer to the handler),
// a panicking handler's recovered 500 response is what gets logged --
// not a zero/unwritten status.
func TestMiddleware_MountedBeforeRecoverer_LogsActualRecoveredStatus(t *testing.T) {
	logger, buf := newTestLogger()

	r := chi.NewRouter()
	r.Use(chimw.RequestID)
	r.Use(Middleware(logger))
	r.Use(chimw.Recoverer)
	r.Get("/panics", func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	})

	req := httptest.NewRequest(http.MethodGet, "/panics", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("client-visible status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}

	entry := parseLogLine(t, buf)
	if entry["status"] != float64(http.StatusInternalServerError) {
		t.Fatalf("logged status = %v, want %d -- this is exactly the bug the documented mount order prevents: "+
			"logging outside Recoverer would see status 0 here, not the real 500 the client got",
			entry["status"], http.StatusInternalServerError)
	}
}