package recovery

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

func TestMiddleware_RecoversPanicAndReturns500(t *testing.T) {
	logger, _ := newTestLogger()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	})
	mw := Middleware(logger)(handler)

	req := httptest.NewRequest(http.MethodGet, "/panics", nil)
	rec := httptest.NewRecorder()

	mw.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
	if !strings.Contains(rec.Body.String(), "internal server error") {
		t.Errorf("body = %q, want it to mention an internal server error", rec.Body.String())
	}
}

func TestMiddleware_LogsPanicWithRequestID(t *testing.T) {
	logger, buf := newTestLogger()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	})

	r := chi.NewRouter()
	r.Use(chimw.RequestID)
	r.Use(Middleware(logger))
	r.Get("/panics", handler.ServeHTTP)

	req := httptest.NewRequest(http.MethodGet, "/panics", nil)
	r.ServeHTTP(httptest.NewRecorder(), req)

	line := strings.TrimSpace(buf.String())
	if line == "" {
		t.Fatal("expected a log line, got none")
	}
	var entry map[string]any
	if err := json.Unmarshal([]byte(line), &entry); err != nil {
		t.Fatalf("log output is not valid JSON: %v\noutput: %s", err, line)
	}
	if entry["panic"] != "boom" {
		t.Errorf("panic field = %v, want %q", entry["panic"], "boom")
	}
	if reqID, _ := entry["request_id"].(string); reqID == "" {
		t.Error("expected a non-empty request_id in the panic log line")
	}
	if _, ok := entry["stack"]; !ok {
		t.Error("expected a stack field in the panic log line")
	}
}

func TestMiddleware_DoesNotInterfereWithNormalRequests(t *testing.T) {
	logger, buf := newTestLogger()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("ok"))
	})
	mw := Middleware(logger)(handler)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusCreated)
	}
	if rec.Body.String() != "ok" {
		t.Errorf("body = %q, want %q", rec.Body.String(), "ok")
	}
	if buf.Len() != 0 {
		t.Errorf("expected no log output for a non-panicking request, got %q", buf.String())
	}
}

func TestMiddleware_RepanicsOnErrAbortHandler(t *testing.T) {
	logger, _ := newTestLogger()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic(http.ErrAbortHandler)
	})
	mw := Middleware(logger)(handler)

	defer func() {
		rec := recover()
		if rec != http.ErrAbortHandler {
			t.Fatalf("recovered value = %v, want http.ErrAbortHandler to propagate unhandled", rec)
		}
	}()

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	mw.ServeHTTP(httptest.NewRecorder(), req)
	t.Fatal("expected http.ErrAbortHandler to re-panic past this middleware")
}
