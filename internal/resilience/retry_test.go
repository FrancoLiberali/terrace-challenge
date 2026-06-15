package resilience

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sony/gobreaker/v2"
)

// fastRetryConfig: exponential and jittered (real backoff math)
// but sub-second so the suite runs quickly.
func fastRetryConfig() *RetryConfig {
	return &RetryConfig{
		MaxRetries:  3,
		InitialWait: 5 * time.Millisecond,
		MaxWait:     50 * time.Millisecond,
	}
}

func TestNewHTTPClient_RetriesOn5xx(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) < 3 {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := NewHTTPClient(HTTPClientConfig{Retry: fastRetryConfig(), RequestTimeout: 5 * time.Second})
	resp, err := c.Get(srv.URL)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200 after retries, got %d", resp.StatusCode)
	}
	if got := calls.Load(); got != 3 {
		t.Errorf("expected 3 attempts on server, got %d", got)
	}
}

func TestNewHTTPClient_DoesNotRetryOn4xx(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		http.Error(w, "nope", http.StatusBadRequest)
	}))
	defer srv.Close()

	c := NewHTTPClient(HTTPClientConfig{Retry: fastRetryConfig(), RequestTimeout: 5 * time.Second})
	resp, err := c.Get(srv.URL)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 surfaced unmodified, got %d", resp.StatusCode)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("expected exactly 1 attempt on 4xx, got %d", got)
	}
}

func TestNewHTTPClient_RateLimitGatesRequests(t *testing.T) {
	// 10 RPS, burst 1 → second request waits ~100ms. Verify the
	// rate-limit transport actually gated by measuring elapsed time.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := NewHTTPClient(HTTPClientConfig{
		Retry:          fastRetryConfig(),
		Limiter:        NewRateLimiter("test", 10, 1),
		RequestTimeout: 5 * time.Second,
	})
	resp1, err := c.Get(srv.URL)
	if err != nil {
		t.Fatalf("first get: %v", err)
	}
	_ = resp1.Body.Close()

	start := time.Now()
	resp2, err := c.Get(srv.URL)
	if err != nil {
		t.Fatalf("second get: %v", err)
	}
	_ = resp2.Body.Close()
	if d := time.Since(start); d < 50*time.Millisecond {
		t.Errorf("second request should have been rate-limited (~100ms), waited %v", d)
	}
}

func TestNewHTTPClient_OpenBreakerShortCircuitsWithoutRetry(t *testing.T) {
	// Pre-trip the breaker by feeding it three failures, then verify
	// that the next HTTP call short-circuits with ErrOpen and does NOT
	// burn the retry budget on a known-open breaker. Server should see
	// only the trip-priming calls.
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	breaker := NewCircuitBreaker(BreakerConfig{
		Name:             "test",
		ConsecutiveFails: 2,
		Cooldown:         time.Hour, // stays open for the duration of the test
	})
	c := NewHTTPClient(HTTPClientConfig{
		// Retry nil — each Get is one attempt, deterministic toward
		// the breaker's trip threshold.
		Breaker:        breaker,
		RequestTimeout: 5 * time.Second,
	})

	// Two 500s trip the breaker.
	for range 2 {
		resp, _ := c.Get(srv.URL)
		if resp != nil {
			_ = resp.Body.Close()
		}
	}
	primingCalls := calls.Load()
	if primingCalls != 2 {
		t.Fatalf("expected 2 priming calls, got %d", primingCalls)
	}

	// Re-enable retries and try once more — the breaker is open so the
	// retry loop must short-circuit immediately without hitting the
	// server again.
	c = NewHTTPClient(HTTPClientConfig{
		Retry:          fastRetryConfig(),
		Breaker:        breaker,
		RequestTimeout: 5 * time.Second,
	})
	resp, err := c.Get(srv.URL)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if err == nil {
		t.Fatal("expected error when breaker is open")
	}
	if calls.Load() != primingCalls {
		t.Errorf("server should not have been hit after breaker opened; got %d additional calls",
			calls.Load()-primingCalls)
	}
	if errors.Is(err, gobreaker.ErrOpenState) {
		// Good — surfaced unchanged through retryablehttp.
		return
	}
	if !strings.Contains(err.Error(), "circuit breaker is open") {
		t.Errorf("expected ErrOpenState (or wrapped) in error chain, got %v", err)
	}
}
