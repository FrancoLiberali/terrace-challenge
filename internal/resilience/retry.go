package resilience

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/cenkalti/backoff/v5"
	"github.com/hashicorp/go-retryablehttp"
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
func DefaultRetryConfig() RetryConfig {
	return RetryConfig{
		MaxRetries:  defaultMaxRetries,
		InitialWait: defaultInitialWait,
		MaxWait:     defaultMaxWait,
	}
}

// NewRetryingHTTPClient returns an *http.Client whose transport
// retries transient HTTP failures (connection errors, 5xx responses,
// 408 / 429) with exponential backoff and jitter. The Retry-After
// response header is honored automatically.
//
// `timeout` is applied to each underlying attempt, not to the total
// retry budget — a single hung connection cannot consume the whole
// retry window.
//
// `logger` may be nil. When non-nil it receives retry-decision
// messages at debug level.
func NewRetryingHTTPClient(cfg RetryConfig, timeout time.Duration, logger *slog.Logger) *http.Client {
	c := retryablehttp.NewClient()
	c.RetryMax = cfg.MaxRetries
	c.RetryWaitMin = cfg.InitialWait
	c.RetryWaitMax = cfg.MaxWait
	c.HTTPClient.Timeout = timeout
	c.Logger = slogLeveledLogger{logger: logger}
	return c.StandardClient()
}

// Retry runs op with exponential backoff and jitter, honoring ctx
// cancellation. To stop retrying on a specific error, return
// backoff.Permanent(err) from op — backoff/v5 unwraps it and surfaces
// the inner error to the caller.
func Retry(ctx context.Context, op func() error, cfg RetryConfig) error {
	b := backoff.NewExponentialBackOff()
	b.InitialInterval = cfg.InitialWait
	b.MaxInterval = cfg.MaxWait
	// RandomizationFactor stays at the default 0.5 (±50% jitter).

	tries := max(cfg.MaxRetries+1, 1)
	_, err := backoff.Retry(
		ctx,
		func() (struct{}, error) { return struct{}{}, op() },
		backoff.WithBackOff(b),
		backoff.WithMaxTries(uint(tries)),
	)
	return err
}

// Permanent wraps err so Retry stops retrying and surfaces err
// directly. Use it for deterministic failures (contract reverts,
// 4xx responses other than 408/429, malformed input) where another
// attempt cannot change the outcome.
func Permanent(err error) error {
	return backoff.Permanent(err)
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
