package resilience

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// fastRetryConfig is the standard test config: still exponential and
// jittered (so we exercise the real backoff math) but with sub-second
// intervals so the tests finish quickly.
func fastRetryConfig() RetryConfig {
	return RetryConfig{
		MaxRetries:  3,
		InitialWait: 5 * time.Millisecond,
		MaxWait:     50 * time.Millisecond,
	}
}

func TestRetry_SucceedsOnFirstAttempt(t *testing.T) {
	var calls atomic.Int32
	err := Retry(t.Context(), func() error {
		calls.Add(1)
		return nil
	}, fastRetryConfig())
	if err != nil {
		t.Fatalf("Retry: %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("expected 1 call, got %d", got)
	}
}

func TestRetry_RetriesTransientUntilSuccess(t *testing.T) {
	var calls atomic.Int32
	transient := errors.New("transient")
	err := Retry(t.Context(), func() error {
		n := calls.Add(1)
		if n < 3 {
			return transient
		}
		return nil
	}, fastRetryConfig())
	if err != nil {
		t.Fatalf("Retry: %v", err)
	}
	if got := calls.Load(); got != 3 {
		t.Errorf("expected 3 calls, got %d", got)
	}
}

func TestRetry_StopsOnPermanent(t *testing.T) {
	var calls atomic.Int32
	deterministic := errors.New("deterministic")
	err := Retry(t.Context(), func() error {
		calls.Add(1)
		return Permanent(deterministic)
	}, fastRetryConfig())
	if !errors.Is(err, deterministic) {
		t.Errorf("expected deterministic error to surface, got %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("Permanent should fire exactly once, got %d", got)
	}
}

func TestRetry_GivesUpAfterMaxRetries(t *testing.T) {
	var calls atomic.Int32
	transient := errors.New("transient")
	err := Retry(t.Context(), func() error {
		calls.Add(1)
		return transient
	}, fastRetryConfig())
	if !errors.Is(err, transient) {
		t.Errorf("expected transient surfaced as final, got %v", err)
	}
	// MaxRetries=3 means up to 4 total attempts.
	if got := calls.Load(); got != 4 {
		t.Errorf("expected 4 attempts (1 + 3 retries), got %d", got)
	}
}

func TestRetry_HonorsContextCancel(t *testing.T) {
	cfg := RetryConfig{
		MaxRetries:  10,
		InitialWait: time.Second, // long enough that ctx cancel wins
		MaxWait:     time.Second,
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	var calls atomic.Int32
	err := Retry(ctx, func() error {
		calls.Add(1)
		return errors.New("transient")
	}, cfg)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got %v", err)
	}
}

func TestRetryingHTTPClient_RetriesOn5xx(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := calls.Add(1)
		if n < 3 {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := NewRetryingHTTPClient(fastRetryConfig(), 5*time.Second, nil)
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

func TestRetryingHTTPClient_DoesNotRetryOn4xx(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		http.Error(w, "nope", http.StatusBadRequest)
	}))
	defer srv.Close()

	c := NewRetryingHTTPClient(fastRetryConfig(), 5*time.Second, nil)
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
