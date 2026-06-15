package resilience

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	// Purpose-built HTTP retry transport: Retry-After honoring, body
	// rewinding, idempotency rules. Drives the retry loop inside the
	// per-host http.Client.
	"github.com/hashicorp/go-retryablehttp"
	"github.com/sony/gobreaker/v2"
)

// RetryConfig parameterises both the HTTP transport retrier and the
// generic operation retrier. Defaults are conservative — five
// attempts, half-second initial delay, ten-second cap — and suitable
// for transient network blips, not for masking provider-side outages
// (the circuit breaker is the right layer for that).
type RetryConfig struct {
	MaxRetries  int           // number of retries AFTER the first attempt (0 disables)
	InitialWait time.Duration // first backoff interval
	MaxWait     time.Duration // cap on any single backoff interval
}

// Default tuning for transient-network retries. Conservative: enough
// retries to ride out a brief blip, not so many that we mask a
// provider-side outage (the circuit breaker is the right layer for
// that).
const (
	defaultMaxRetries  = 4
	defaultInitialWait = 500 * time.Millisecond
	defaultMaxWait     = 10 * time.Second
)

// DefaultRetryConfig returns a config that retries up to 4 times with
// exponential backoff starting at 500ms and capped at 10s. The
// underlying libraries add jitter automatically.
func DefaultRetryConfig() *RetryConfig {
	return &RetryConfig{
		MaxRetries:  defaultMaxRetries,
		InitialWait: defaultInitialWait,
		MaxWait:     defaultMaxWait,
	}
}

// HTTPClientConfig parameterises NewHTTPClient. Retry, Limiter and
// Breaker are each optional — pass nil to skip that layer.
// RequestTimeout applies to each individual attempt, not the total
// retry budget; a single hung connection cannot consume the entire
// window.
type HTTPClientConfig struct {
	Retry          *RetryConfig
	Limiter        *RateLimiter
	Breaker        *CircuitBreaker
	RequestTimeout time.Duration
	Logger         *slog.Logger
}

// NewHTTPClient returns an *http.Client whose transport composes the
// resilience stack for a single external host: retry → circuit
// breaker → rate limit → real HTTP. The breaker short-circuits
// before a rate token is consumed, the rate limit gates real network
// I/O, and retry sits outermost so each attempt sees the freshest
// state of the underlying layers. ErrOpen surfaces from the breaker
// as a permanent error to the retry layer — the retry loop does not
// burn budget on a known-open breaker.
func NewHTTPClient(cfg HTTPClientConfig) *http.Client {
	var transport http.RoundTripper
	transport = http.DefaultTransport
	if cfg.Limiter != nil {
		transport = &rateLimitTransport{inner: transport, limiter: cfg.Limiter}
	}
	if cfg.Breaker != nil {
		transport = &circuitBreakerTransport{inner: transport, breaker: cfg.Breaker}
	}
	if cfg.Retry == nil {
		return &http.Client{Transport: transport, Timeout: cfg.RequestTimeout}
	}

	c := retryablehttp.NewClient()
	c.HTTPClient = &http.Client{Transport: transport, Timeout: cfg.RequestTimeout}
	c.RetryMax = cfg.Retry.MaxRetries
	c.RetryWaitMin = cfg.Retry.InitialWait
	c.RetryWaitMax = cfg.Retry.MaxWait
	c.Logger = slogLeveledLogger{logger: cfg.Logger}
	// Don't retry once the breaker has opened — the inner transport
	// will fail-fast with ErrOpen until the cooldown elapses, so
	// retries are wasted budget.
	c.CheckRetry = func(ctx context.Context, resp *http.Response, err error) (bool, error) {
		if errors.Is(err, gobreaker.ErrOpenState) || errors.Is(err, gobreaker.ErrTooManyRequests) {
			return false, err
		}
		return retryablehttp.DefaultRetryPolicy(ctx, resp, err)
	}
	return c.StandardClient()
}

// rateLimitTransport gates each outbound request through the limiter
// before delegating to the inner transport.
type rateLimitTransport struct {
	inner   http.RoundTripper
	limiter *RateLimiter
}

func (t *rateLimitTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := t.limiter.Wait(req.Context()); err != nil {
		return nil, err
	}
	return t.inner.RoundTrip(req)
}

// circuitBreakerTransport gates each outbound request through the
// breaker. When the breaker is open the request short-circuits with
// gobreaker.ErrOpenState (which surfaces unchanged to the retry
// layer so it can be classified as permanent).
//
// 5xx responses are reported to the breaker as failures even though
// Go's HTTP semantics surface them as (resp, nil). Without this, a
// server consistently returning 500 would never trip the breaker. The
// 5xx marker error is swallowed on the way out so the response itself
// still flows upstream — retryablehttp's CheckRetry will see the 5xx
// status and decide whether to retry.
type circuitBreakerTransport struct {
	inner   http.RoundTripper
	breaker *CircuitBreaker
}

var errServerError = errors.New("server returned 5xx")

func (t *circuitBreakerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var resp *http.Response
	err := t.breaker.Execute(req.Context(), func() error {
		r, e := t.inner.RoundTrip(req) //nolint:bodyclose // transport passes the body upstream; caller owns Close
		resp = r
		if e != nil {
			return e
		}
		if r != nil && r.StatusCode >= http.StatusInternalServerError {
			return errServerError
		}
		return nil
	})
	if errors.Is(err, errServerError) {
		return resp, nil
	}
	return resp, err
}

// slogLeveledLogger adapts an *slog.Logger to retryablehttp's
// LeveledLogger interface. Used internally by NewRetryingHTTPClient
// so retry decisions are visible in the structured-log stream when a
// logger is wired in.
type slogLeveledLogger struct {
	logger *slog.Logger
}

func (l slogLeveledLogger) Error(msg string, kv ...any) { l.log(slog.LevelError, msg, kv) }
func (l slogLeveledLogger) Warn(msg string, kv ...any)  { l.log(slog.LevelWarn, msg, kv) }
func (l slogLeveledLogger) Info(msg string, kv ...any)  { l.log(slog.LevelInfo, msg, kv) }
func (l slogLeveledLogger) Debug(msg string, kv ...any) { l.log(slog.LevelDebug, msg, kv) }

func (l slogLeveledLogger) log(level slog.Level, msg string, kv []any) {
	if l.logger == nil {
		return
	}
	l.logger.Log(context.Background(), level, msg, kv...)
}
