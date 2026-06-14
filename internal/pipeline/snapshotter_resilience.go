package pipeline

import (
	"context"
	"fmt"

	"github.com/FrancoLiberali/terrace-challenge/internal/chain"
	"github.com/FrancoLiberali/terrace-challenge/internal/pricing"
	"github.com/FrancoLiberali/terrace-challenge/internal/resilience"
)

// RateLimited returns a Snapshotter that consults the limiter before
// each underlying Snapshot. If the limiter denies (ctx cancellation
// while waiting) the call is rejected before the inner snapshot is
// invoked, so the rate budget is enforced regardless of inner state.
//
// Stacking rationale: rate limiting goes outermost — it bounds our
// side of the relationship, so we don't hammer a struggling
// dependency even when the breaker below is open and returning fast
// errors.
func RateLimited(inner Snapshotter, limiter *resilience.RateLimiter) Snapshotter {
	return &rateLimitedSnapshotter{inner: inner, limiter: limiter}
}

type rateLimitedSnapshotter struct {
	inner   Snapshotter
	limiter *resilience.RateLimiter
}

func (r *rateLimitedSnapshotter) Snapshot(ctx context.Context, block chain.BlockEvent) (pricing.Quotes, error) {
	if err := r.limiter.Wait(ctx); err != nil {
		return pricing.Quotes{}, fmt.Errorf("rate-limit wait: %w", err)
	}
	return r.inner.Snapshot(ctx, block)
}

// CircuitBroken returns a Snapshotter that routes each Snapshot
// through the breaker. When the breaker is open the call short-circuits
// with resilience.ErrOpen and the inner Snapshot is never invoked.
//
// Stacking rationale: the breaker sits inside the rate limit but
// outside the raw client — it observes one outcome per high-level
// snapshot operation, not per HTTP retry. Transient blips handled by
// the inner retry transport are not reported to the breaker; only the
// final outcome (success or exhaustion) is.
func CircuitBroken(inner Snapshotter, breaker *resilience.CircuitBreaker) Snapshotter {
	return &circuitBrokenSnapshotter{inner: inner, breaker: breaker}
}

type circuitBrokenSnapshotter struct {
	inner   Snapshotter
	breaker *resilience.CircuitBreaker
}

func (c *circuitBrokenSnapshotter) Snapshot(ctx context.Context, block chain.BlockEvent) (pricing.Quotes, error) {
	var quotes pricing.Quotes
	err := c.breaker.Execute(ctx, func() error {
		q, err := c.inner.Snapshot(ctx, block)
		quotes = q
		return err
	})
	return quotes, err
}
