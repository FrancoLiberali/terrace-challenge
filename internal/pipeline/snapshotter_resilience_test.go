package pipeline

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/FrancoLiberali/terrace-challenge/internal/chain"
	"github.com/FrancoLiberali/terrace-challenge/internal/pricing"
	"github.com/FrancoLiberali/terrace-challenge/internal/resilience"
)

// fnSnapshotter adapts a function into a Snapshotter. Used by the
// resilience-decorator tests to vary behavior call-by-call (e.g.,
// "fail twice, then succeed") without rebuilding a stateful fake.
type fnSnapshotter func(ctx context.Context, block chain.BlockEvent) (pricing.Quotes, error)

func (f fnSnapshotter) Snapshot(ctx context.Context, block chain.BlockEvent) (pricing.Quotes, error) {
	return f(ctx, block)
}

func okQuotes() pricing.Quotes {
	return pricing.Quotes{
		Buy:  []pricing.Quote{{Size: decimal.NewFromInt(1), Side: pricing.Buy, Price: decimal.NewFromInt(1680)}},
		Sell: []pricing.Quote{{Size: decimal.NewFromInt(1), Side: pricing.Sell, Price: decimal.NewFromInt(1679)}},
	}
}

func TestRateLimited_DelegatesAndForwardsResult(t *testing.T) {
	want := okQuotes()
	var calls atomic.Int32
	inner := fnSnapshotter(func(_ context.Context, _ chain.BlockEvent) (pricing.Quotes, error) {
		calls.Add(1)
		return want, nil
	})

	// burst=10 so no waiting in this test.
	sn := RateLimited(inner, resilience.NewRateLimiter(100, 10))
	got, err := sn.Snapshot(t.Context(), chain.BlockEvent{Number: 42})
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(got.Buy) != 1 || !got.Buy[0].Price.Equal(decimal.NewFromInt(1680)) {
		t.Errorf("expected inner quotes forwarded, got %+v", got)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("expected 1 inner call, got %d", got)
	}
}

func TestRateLimited_BlocksUntilTokenAvailable(t *testing.T) {
	var calls atomic.Int32
	inner := fnSnapshotter(func(_ context.Context, _ chain.BlockEvent) (pricing.Quotes, error) {
		calls.Add(1)
		return okQuotes(), nil
	})

	// burst=1, rate=10/s → second call waits ~100ms.
	sn := RateLimited(inner, resilience.NewRateLimiter(10, 1))
	ctx := t.Context()
	if _, err := sn.Snapshot(ctx, chain.BlockEvent{Number: 1}); err != nil {
		t.Fatalf("first call: %v", err)
	}
	start := time.Now()
	if _, err := sn.Snapshot(ctx, chain.BlockEvent{Number: 2}); err != nil {
		t.Fatalf("second call: %v", err)
	}
	if d := time.Since(start); d < 50*time.Millisecond {
		t.Errorf("second call should have waited ~100ms, waited %v", d)
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("expected 2 inner calls, got %d", got)
	}
}

func TestRateLimited_DoesNotCallInnerWhenContextCancelledMidWait(t *testing.T) {
	var calls atomic.Int32
	inner := fnSnapshotter(func(_ context.Context, _ chain.BlockEvent) (pricing.Quotes, error) {
		calls.Add(1)
		return okQuotes(), nil
	})

	// Rate=1/min, burst=1: second call would block ~1min. Cancel quickly.
	sn := RateLimited(inner, resilience.NewRateLimiter(1.0/60.0, 1))
	if _, err := sn.Snapshot(t.Context(), chain.BlockEvent{}); err != nil {
		t.Fatalf("first call: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := sn.Snapshot(ctx, chain.BlockEvent{})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("inner must not be called when rate-limit wait fails; got %d total", got)
	}
}

func TestCircuitBroken_ForwardsSuccess(t *testing.T) {
	want := okQuotes()
	inner := fnSnapshotter(func(_ context.Context, _ chain.BlockEvent) (pricing.Quotes, error) {
		return want, nil
	})
	sn := CircuitBroken(inner, resilience.NewCircuitBreaker(resilience.BreakerConfig{
		Name:             "test",
		ConsecutiveFails: 5,
		Cooldown:         time.Second,
	}))
	got, err := sn.Snapshot(t.Context(), chain.BlockEvent{})
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(got.Buy) != 1 {
		t.Errorf("expected forwarded quotes, got %+v", got)
	}
}

func TestCircuitBroken_OpensAfterConsecutiveFailures(t *testing.T) {
	var calls atomic.Int32
	boom := errors.New("provider down")
	inner := fnSnapshotter(func(_ context.Context, _ chain.BlockEvent) (pricing.Quotes, error) {
		calls.Add(1)
		return pricing.Quotes{}, boom
	})
	sn := CircuitBroken(inner, resilience.NewCircuitBreaker(resilience.BreakerConfig{
		Name:             "test",
		ConsecutiveFails: 3,
		Cooldown:         time.Hour, // long cooldown so we don't accidentally half-open
	}))

	// Drive 3 consecutive failures.
	for i := range 3 {
		_, err := sn.Snapshot(t.Context(), chain.BlockEvent{Number: uint64(i)})
		if !errors.Is(err, boom) {
			t.Fatalf("call %d: expected boom, got %v", i, err)
		}
	}

	// Next call must short-circuit: ErrOpen and inner not invoked.
	innerCallsBefore := calls.Load()
	_, err := sn.Snapshot(t.Context(), chain.BlockEvent{Number: 99})
	if !errors.Is(err, resilience.ErrOpen) {
		t.Errorf("expected ErrOpen, got %v", err)
	}
	if calls.Load() != innerCallsBefore {
		t.Errorf("inner should not be called when breaker is open; calls=%d before, %d after",
			innerCallsBefore, calls.Load())
	}
}

func TestCircuitBroken_HalfOpenProbeRecovers(t *testing.T) {
	var n atomic.Int32
	boom := errors.New("provider down")
	inner := fnSnapshotter(func(_ context.Context, _ chain.BlockEvent) (pricing.Quotes, error) {
		// First two calls fail; subsequent calls succeed.
		if n.Add(1) <= 2 {
			return pricing.Quotes{}, boom
		}
		return okQuotes(), nil
	})
	sn := CircuitBroken(inner, resilience.NewCircuitBreaker(resilience.BreakerConfig{
		Name:             "test",
		ConsecutiveFails: 2,
		Cooldown:         30 * time.Millisecond,
	}))

	_, _ = sn.Snapshot(t.Context(), chain.BlockEvent{})
	_, _ = sn.Snapshot(t.Context(), chain.BlockEvent{})
	// Breaker open; wait out the cooldown.
	time.Sleep(60 * time.Millisecond)

	// Half-open probe — should succeed and close the breaker.
	got, err := sn.Snapshot(t.Context(), chain.BlockEvent{})
	if err != nil {
		t.Fatalf("half-open probe should succeed, got %v", err)
	}
	if len(got.Buy) != 1 {
		t.Errorf("expected forwarded quotes after recovery, got %+v", got)
	}
}
