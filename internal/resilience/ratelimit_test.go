package resilience

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRateLimiter_AllowsBurst(t *testing.T) {
	// burst=3 means the first 3 Wait calls succeed without blocking.
	rl := NewRateLimiter(1, 3)
	ctx := t.Context()
	start := time.Now()
	for i := range 3 {
		if err := rl.Wait(ctx); err != nil {
			t.Fatalf("wait %d: %v", i, err)
		}
	}
	if d := time.Since(start); d > 50*time.Millisecond {
		t.Errorf("burst should not block; spent %v", d)
	}
}

func TestRateLimiter_BlocksAfterBurstExhausted(t *testing.T) {
	// burst=1, rate=10/s → after the first Wait the second must wait ~100ms.
	rl := NewRateLimiter(10, 1)
	ctx := t.Context()
	if err := rl.Wait(ctx); err != nil {
		t.Fatalf("first wait: %v", err)
	}
	start := time.Now()
	if err := rl.Wait(ctx); err != nil {
		t.Fatalf("second wait: %v", err)
	}
	if d := time.Since(start); d < 50*time.Millisecond {
		t.Errorf("second wait should have blocked for ~100ms, blocked %v", d)
	}
}

func TestRateLimiter_RespectsContextCancel(t *testing.T) {
	// burst=1, rate=1/min → second Wait would block for ~1 minute. Cancel
	// the context immediately; Wait must return ctx.Err quickly.
	rl := NewRateLimiter(1.0/60.0, 1)
	ctx, cancel := context.WithCancel(t.Context())
	if err := rl.Wait(ctx); err != nil {
		t.Fatalf("first wait: %v", err)
	}

	cancel()
	start := time.Now()
	err := rl.Wait(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got %v", err)
	}
	if d := time.Since(start); d > 100*time.Millisecond {
		t.Errorf("Wait took %v to honor cancel; expected near-immediate", d)
	}
}
