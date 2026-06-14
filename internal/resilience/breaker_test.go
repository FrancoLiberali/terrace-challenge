package resilience

import (
	"context"
	"errors"
	"testing"
	"time"
)

func newTestBreaker(consecutive uint32, cooldown time.Duration) *CircuitBreaker {
	return NewCircuitBreaker(BreakerConfig{
		Name:             "test",
		ConsecutiveFails: consecutive,
		Cooldown:         cooldown,
	})
}

func TestCircuitBreaker_OpensAfterConsecutiveFails(t *testing.T) {
	b := newTestBreaker(3, time.Hour) // long cooldown so we don't accidentally half-open
	boom := errors.New("boom")

	for i := range 3 {
		if err := b.Execute(t.Context(), func() error { return boom }); !errors.Is(err, boom) {
			t.Fatalf("call %d: expected boom, got %v", i, err)
		}
	}
	// Breaker should now be open — next call short-circuits without invoking op.
	called := false
	err := b.Execute(t.Context(), func() error {
		called = true
		return nil
	})
	if !errors.Is(err, ErrOpen) {
		t.Errorf("expected ErrOpen after threshold, got %v", err)
	}
	if called {
		t.Error("op was invoked while breaker was open")
	}
}

func TestCircuitBreaker_ConsecutiveResetsOnSuccess(t *testing.T) {
	b := newTestBreaker(3, time.Hour)
	boom := errors.New("boom")

	// 2 failures (one short of threshold)…
	_ = b.Execute(t.Context(), func() error { return boom })
	_ = b.Execute(t.Context(), func() error { return boom })
	// …one success resets the consecutive counter.
	if err := b.Execute(t.Context(), func() error { return nil }); err != nil {
		t.Fatalf("success call: %v", err)
	}
	// Two more failures should NOT trip the breaker (consecutive count was reset).
	_ = b.Execute(t.Context(), func() error { return boom })
	_ = b.Execute(t.Context(), func() error { return boom })
	if err := b.Execute(t.Context(), func() error { return nil }); err != nil {
		t.Errorf("breaker tripped after non-consecutive failures: %v", err)
	}
}

func TestCircuitBreaker_HalfOpenAllowsProbeAfterCooldown(t *testing.T) {
	b := newTestBreaker(2, 50*time.Millisecond)
	boom := errors.New("boom")

	// Trip the breaker.
	_ = b.Execute(t.Context(), func() error { return boom })
	_ = b.Execute(t.Context(), func() error { return boom })

	// Wait out the cooldown — breaker should transition to half-open.
	time.Sleep(100 * time.Millisecond)

	// A successful probe closes the breaker.
	if err := b.Execute(t.Context(), func() error { return nil }); err != nil {
		t.Fatalf("half-open probe should succeed: %v", err)
	}
	// Closed again — normal calls work.
	if err := b.Execute(t.Context(), func() error { return nil }); err != nil {
		t.Errorf("expected closed-state success, got %v", err)
	}
}

func TestCircuitBreaker_HalfOpenReopensOnFailure(t *testing.T) {
	b := newTestBreaker(2, 50*time.Millisecond)
	boom := errors.New("boom")

	_ = b.Execute(t.Context(), func() error { return boom })
	_ = b.Execute(t.Context(), func() error { return boom })
	time.Sleep(100 * time.Millisecond)

	// The half-open probe fails — breaker should re-open.
	if err := b.Execute(t.Context(), func() error { return boom }); !errors.Is(err, boom) {
		t.Fatalf("probe should surface inner err, got %v", err)
	}
	// Next call before cooldown sees ErrOpen.
	err := b.Execute(t.Context(), func() error { return nil })
	if !errors.Is(err, ErrOpen) {
		t.Errorf("expected ErrOpen after re-open, got %v", err)
	}
}

func TestCircuitBreaker_OnStateChangeFires(t *testing.T) {
	var got []string
	b := NewCircuitBreaker(BreakerConfig{
		Name:             "test",
		ConsecutiveFails: 1,
		Cooldown:         50 * time.Millisecond,
		OnStateChange: func(_ string, from, to string) {
			got = append(got, from+"->"+to)
		},
	})
	boom := errors.New("boom")

	_ = b.Execute(context.Background(), func() error { return boom })
	if len(got) == 0 || got[0] != "closed->open" {
		t.Errorf("expected closed->open transition recorded, got %v", got)
	}
}
