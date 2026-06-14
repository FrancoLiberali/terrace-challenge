// Package resilience holds the generic resilience primitives the rest
// of the codebase composes around external dependencies: rate limiting,
// circuit breaking, and retry with exponential backoff and jitter.
//
// Each primitive is a thin wrapper around a battle-tested third-party
// library — golang.org/x/time/rate for the token bucket,
// github.com/sony/gobreaker/v2 for the circuit breaker, and
// github.com/cenkalti/backoff/v5 for backoff — so the rest of the
// codebase can depend on a small, internal surface and swap the
// underlying implementation later without rippling.
package resilience

import (
	"context"

	"golang.org/x/time/rate"
)

// RateLimiter is a token-bucket limiter. Callers Wait before every
// rate-limited operation; Wait blocks until a token is available or
// the context is cancelled.
type RateLimiter struct {
	inner *rate.Limiter
}

// NewRateLimiter returns a limiter that emits `perSecond` tokens per
// second with a maximum burst of `burst`. A burst of 1 disables
// bursting (the limiter is strict).
func NewRateLimiter(perSecond float64, burst int) *RateLimiter {
	return &RateLimiter{inner: rate.NewLimiter(rate.Limit(perSecond), burst)}
}

// Wait blocks until a token is available or ctx is cancelled. The
// caller should check the returned error and abort the operation
// rather than proceed when Wait returns non-nil.
func (r *RateLimiter) Wait(ctx context.Context) error {
	return r.inner.Wait(ctx)
}
