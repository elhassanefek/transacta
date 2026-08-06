package idempotency

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/elhassanefek/transacta/internal/tenants"
)

// fakeStore is an in-memory Store for testing the middleware's decision
// logic in isolation from any database.
type fakeStore struct {
	records map[string]*Record // keyed by tenantID+"|"+key
	// claimErr, if set, is returned by ClaimOrGet regardless of state.
	claimErr error
	// completeFailuresRemaining, if > 0, makes Complete fail that many
	// times before succeeding -- used to test completeWithRetry.
	completeFailuresRemaining int
	// completeCalls records every Complete invocation for assertions.
	completeCalls []completeCall
}

type completeCall struct {
	tenantID     uuid.UUID
	key          string
	responseCode int
	responseBody []byte
}

func newFakeStore() *fakeStore {
	return &fakeStore{records: make(map[string]*Record)}
}

func (f *fakeStore) storeKey(tenantID uuid.UUID, key string) string {
	return tenantID.String() + "|" + key
}

func (f *fakeStore) ClaimOrGet(_ context.Context, tenantID uuid.UUID, key, requestHash string, ttl time.Duration) (*Record, bool, error) {
	if f.claimErr != nil {
		return nil, false, f.claimErr
	}
	sk := f.storeKey(tenantID, key)
	if existing, ok := f.records[sk]; ok && existing.ExpiresAt.After(time.Now()) {
		return existing, false, nil
	}
	rec := &Record{
		TenantID:    tenantID,
		Key:         key,
		RequestHash: requestHash,
		Status:      StatusProcessing,
		CreatedAt:   time.Now(),
		ExpiresAt:   time.Now().Add(ttl),
	}
	f.records[sk] = rec
	return rec, true, nil
}

func (f *fakeStore) Complete(_ context.Context, tenantID uuid.UUID, key string, responseCode int, responseBody []byte) error {
	f.completeCalls = append(f.completeCalls, completeCall{tenantID, key, responseCode, responseBody})
	if f.completeFailuresRemaining > 0 {
		f.completeFailuresRemaining--
		return errors.New("simulated transient failure")
	}
	sk := f.storeKey(tenantID, key)
	if rec, ok := f.records[sk]; ok {
		rec.Status = StatusCompleted
		rec.ResponseCode = &responseCode
		rec.ResponseBody = responseBody
	}
	return nil
}

func testHandler(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
}

func withTenant(req *http.Request, tenantID uuid.UUID) *http.Request {
	return req.WithContext(tenants.WithTenantID(req.Context(), tenantID))
}

func TestMiddleware_MissingKeyRejected(t *testing.T) {
	store := newFakeStore()
	mw := Middleware(store, time.Hour)(testHandler(http.StatusCreated, "ok"))

	req := withTenant(httptest.NewRequest(http.MethodPost, "/transfers", strings.NewReader("{}")), uuid.New())
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestMiddleware_MissingTenantRejected(t *testing.T) {
	store := newFakeStore()
	mw := Middleware(store, time.Hour)(testHandler(http.StatusCreated, "ok"))

	// Deliberately not calling withTenant.
	req := httptest.NewRequest(http.MethodPost, "/transfers", strings.NewReader("{}"))
	req.Header.Set(HeaderKey, "key-1")
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
}

func TestMiddleware_FreshKeyExecutesHandlerAndCaches(t *testing.T) {
	store := newFakeStore()
	tenantID := uuid.New()
	mw := Middleware(store, time.Hour)(testHandler(http.StatusCreated, `{"id":"txn-1"}`))

	req := withTenant(httptest.NewRequest(http.MethodPost, "/transfers", strings.NewReader(`{"amount":100}`)), tenantID)
	req.Header.Set(HeaderKey, "key-1")
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusCreated)
	}
	if rec.Body.String() != `{"id":"txn-1"}` {
		t.Fatalf("body = %q, want %q", rec.Body.String(), `{"id":"txn-1"}`)
	}
	if len(store.completeCalls) != 1 {
		t.Fatalf("Complete called %d times, want 1", len(store.completeCalls))
	}
	if store.completeCalls[0].responseCode != http.StatusCreated {
		t.Fatalf("cached response code = %d, want %d", store.completeCalls[0].responseCode, http.StatusCreated)
	}
}

func TestMiddleware_CompletedKeyReplaysWithoutReexecuting(t *testing.T) {
	store := newFakeStore()
	tenantID := uuid.New()
	callCount := 0
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"txn-1"}`))
	})
	mw := Middleware(store, time.Hour)(handler)

	makeReq := func() *http.Request {
		req := withTenant(httptest.NewRequest(http.MethodPost, "/transfers", strings.NewReader(`{"amount":100}`)), tenantID)
		req.Header.Set(HeaderKey, "key-1")
		return req
	}

	rec1 := httptest.NewRecorder()
	mw.ServeHTTP(rec1, makeReq())

	rec2 := httptest.NewRecorder()
	mw.ServeHTTP(rec2, makeReq())

	if callCount != 1 {
		t.Fatalf("handler called %d times, want 1 (second request should replay, not re-execute)", callCount)
	}
	if rec2.Code != http.StatusCreated {
		t.Fatalf("replayed status = %d, want %d", rec2.Code, http.StatusCreated)
	}
	if rec2.Body.String() != `{"id":"txn-1"}` {
		t.Fatalf("replayed body = %q, want %q", rec2.Body.String(), `{"id":"txn-1"}`)
	}
	if rec2.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatal("expected Idempotency-Replayed header on replayed response")
	}
}

func TestMiddleware_InFlightKeyRejectedWithConflict(t *testing.T) {
	store := newFakeStore()
	tenantID := uuid.New()
	// Pre-seed a still-processing record directly, simulating a
	// concurrent request that hasn't completed yet.
	store.records[store.storeKey(tenantID, "key-1")] = &Record{
		TenantID:    tenantID,
		Key:         "key-1",
		RequestHash: requestHash(http.MethodPost, "/transfers", []byte(`{"amount":100}`)),
		Status:      StatusProcessing,
		ExpiresAt:   time.Now().Add(time.Hour),
	}

	mw := Middleware(store, time.Hour)(testHandler(http.StatusCreated, "should not run"))
	req := withTenant(httptest.NewRequest(http.MethodPost, "/transfers", strings.NewReader(`{"amount":100}`)), tenantID)
	req.Header.Set(HeaderKey, "key-1")
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusConflict)
	}
}

func TestMiddleware_ReusedKeyDifferentPayloadRejected(t *testing.T) {
	store := newFakeStore()
	tenantID := uuid.New()
	mw := Middleware(store, time.Hour)(testHandler(http.StatusCreated, "ok"))

	req1 := withTenant(httptest.NewRequest(http.MethodPost, "/transfers", strings.NewReader(`{"amount":100}`)), tenantID)
	req1.Header.Set(HeaderKey, "key-1")
	mw.ServeHTTP(httptest.NewRecorder(), req1)

	// Same key, different body -- must be rejected, not treated as a
	// duplicate of the first request.
	req2 := withTenant(httptest.NewRequest(http.MethodPost, "/transfers", strings.NewReader(`{"amount":999}`)), tenantID)
	req2.Header.Set(HeaderKey, "key-1")
	rec2 := httptest.NewRecorder()
	mw.ServeHTTP(rec2, req2)

	if rec2.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want %d", rec2.Code, http.StatusUnprocessableEntity)
	}
}

func TestRequestHash_DifferentBodyDifferentHash(t *testing.T) {
	h1 := requestHash(http.MethodPost, "/transfers", []byte(`{"amount":100}`))
	h2 := requestHash(http.MethodPost, "/transfers", []byte(`{"amount":999}`))
	if h1 == h2 {
		t.Fatal("expected different hashes for different request bodies")
	}
}

func TestRequestHash_Deterministic(t *testing.T) {
	h1 := requestHash(http.MethodPost, "/transfers", []byte(`{"amount":100}`))
	h2 := requestHash(http.MethodPost, "/transfers", []byte(`{"amount":100}`))
	if h1 != h2 {
		t.Fatal("expected identical hashes for identical inputs")
	}
}

func TestCompleteWithRetry_SucceedsAfterTransientFailures(t *testing.T) {
	store := newFakeStore()
	tenantID := uuid.New()

	// Seed a real claimed record first, same as the middleware would have
	// done before calling completeWithRetry.
	if _, claimed, err := store.ClaimOrGet(context.Background(), tenantID, "key-1", "hash-1", time.Hour); err != nil || !claimed {
		t.Fatalf("setup claim: claimed=%v err=%v", claimed, err)
	}
	store.completeFailuresRemaining = 2 // fail twice, succeed on 3rd attempt

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))

	completeWithRetry(context.Background(), store, logger, tenantID, "key-1", 201, []byte(`{"ok":true}`))

	if len(store.completeCalls) != 3 {
		t.Fatalf("Complete called %d times, want 3 (2 failures + 1 success)", len(store.completeCalls))
	}
	sk := store.storeKey(tenantID, "key-1")
	rec, ok := store.records[sk]
	if !ok {
		t.Fatal("expected a record to exist after retry succeeded")
	}
	if rec.Status != StatusCompleted {
		t.Fatalf("status = %q, want %q (retry should have eventually succeeded)", rec.Status, StatusCompleted)
	}
	if strings.Contains(logBuf.String(), "level=ERROR") {
		t.Fatal("expected no ERROR log when retry eventually succeeds")
	}
}

func TestCompleteWithRetry_LogsErrorOnExhaustion(t *testing.T) {
	store := newFakeStore()
	store.completeFailuresRemaining = 999 // never succeeds
	tenantID := uuid.New()

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))

	completeWithRetry(context.Background(), store, logger, tenantID, "key-1", 201, []byte(`{"ok":true}`))

	if len(store.completeCalls) != completeRetryAttempts {
		t.Fatalf("Complete called %d times, want %d (all attempts exhausted)", len(store.completeCalls), completeRetryAttempts)
	}
	if !strings.Contains(logBuf.String(), "level=ERROR") {
		t.Fatal("expected an ERROR log when all retries are exhausted -- this must not fail silently")
	}
	if !strings.Contains(logBuf.String(), "silently reclaimable") {
		t.Fatal("expected the error log to explain the actual risk (silent reclaim after TTL), not just 'failed'")
	}
}
