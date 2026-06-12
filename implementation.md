# Implementation <!-- omit from toc -->

This document maps the architecture described in [`architecture.md`](./architecture.md) to its Go-level structure: package layout, interface seam locations, and code-level conventions. It is the implementation-detail counterpart to architecture.md and will grow as the code lands.

For the conceptual architecture (what components exist, how they relate, the design decisions and trade-offs), see [`architecture.md`](./architecture.md). For business context see [`business.md`](./business.md), and for known limitations see [`limitations.md`](./limitations.md).

---

## Table of Contents <!-- omit from toc -->

- [Package layout](#package-layout)
- [Conventions](#conventions)
- [Interface seams in code](#interface-seams-in-code)
- [Resilience composition pattern](#resilience-composition-pattern)
- [Numeric types for financial math](#numeric-types-for-financial-math)

---

## Package layout

```
terrace-challange/
├── cmd/
│   └── arbd/
│       └── main.go              # parse config, wire components, start runtime
├── internal/                    # not importable by external code
│   ├── chain/
│   │   ├── subscriber.go        # WebSocket newHeads + reconnect logic
│   │   ├── gas.go               # gas-price estimator
│   │   └── types.go             # BlockEvent
│   ├── cex/
│   │   ├── adapter.go           # CEXAdapter interface
│   │   └── binance/
│   │       ├── client.go        # HTTP client wrapper
│   │       └── adapter.go       # implements CEXAdapter
│   ├── dex/
│   │   ├── adapter.go           # DEXAdapter interface
│   │   └── uniswapv3/
│   │       ├── quoter.go        # QuoterV2 wrapper
│   │       └── adapter.go       # implements DEXAdapter
│   ├── pipeline/
│   │   └── coordinator.go       # per-block fan-out / fan-in / cancellation
│   ├── pricing/                 # SHARED UTILITIES used by adapters,
│   │                            # not a pipeline stage
│   │   ├── orderbook.go         # walk book → effective price (pure)
│   │   └── normalize.go         # convert raw amounts to decimal.Decimal
│   ├── pathfinder/
│   │   ├── pathfinder.go        # enumerate candidate paths (pure)
│   │   └── types.go             # CandidatePath
│   ├── arbitrage/
│   │   ├── evaluator.go         # apply cost model, emit Opportunity (pure)
│   │   └── types.go             # Opportunity, Direction
│   ├── output/
│   │   ├── sink.go              # OpportunitySink interface
│   │   └── stdout.go            # default structured-log implementation
│   ├── config/
│   │   └── config.go            # YAML loader + env var overrides
│   └── resilience/
│       ├── backoff.go           # exponential with jitter
│       ├── ratelimit.go         # token bucket per adapter
│       └── circuit.go           # circuit breaker (stub for now)
├── config.example.yaml
├── go.mod
└── (docs at repo root)
```

---

## Conventions

- **`cmd/arbd/`** is the only package that knows how the pieces fit together. Everything else is a library that knows nothing about how it is wired. This makes alternative configurations easy and tests trivial.
- **`internal/`** keeps the application code unimportable from outside the module. Standard Go convention for application code; prevents accidental coupling if the repo grows.
- **Adapters live in subdirectories named after the venue** (`cex/binance/`, `dex/uniswapv3/`). Adding a second CEX is `cex/coinbase/`; adding a second DEX is `dex/sushiswapv3/`. The interface lives in the parent package so adapters depend only on it.
- **`pricing/`, `pathfinder/`, and `arbitrage/` are pure packages** — no network, no I/O, no goroutines. They are the unit-test sweet spot. `pricing/` is imported by the adapter packages so that adapters produce the unified effective-price shape directly; it is not invoked by the pipeline as a separate stage. `pathfinder/` and `arbitrage/` are downstream pipeline stages: the first finds candidate paths, the second evaluates their profitability.
- **`resilience/` is generic** — nothing in it knows about CEX vs DEX. The adapter wrappers apply it.

---

## Interface seams in code

The architecture exposes six interface seams where mocking, swapping, or extension is expected. The table below maps each seam to its package location. Concrete Go type signatures will be added as the implementation lands.

| Interface | Package | Purpose |
|---|---|---|
| `BlockSubscriber` | `internal/chain` | Emits one block event per new block, internally handling reconnect / fallback |
| `CEXAdapter` | `internal/cex` | Given a pair and a list of trade sizes, returns the unified effective-price list (one entry per `(size, side)`) |
| `DEXAdapter` | `internal/dex` | Given a pair and a list of trade sizes, returns the unified effective-price list (one entry per `(size, side)`) |
| `OpportunitySink` | `internal/output` | Receives and emits each detected `Opportunity` |
| `RateLimiter` | `internal/resilience` | Token bucket for external API calls |
| `CircuitBreaker` | `internal/resilience` | Trip on consecutive failures, half-open after cooldown |

The two adapter interfaces (`CEXAdapter` and `DEXAdapter`) deliberately share the same shape: downstream code (coordinator, pathfinder, evaluator) does not care which venue any given price came from.

Internal components (Snapshot Coordinator, Pathfinder, Profitability Evaluator) are intentionally concrete structs and pure functions, not interfaces — abstracting them would add noise without enabling any real flexibility.

---

## Resilience composition pattern

Architecture decision 6 (in [`architecture.md`](./architecture.md#6-resilience-is-wrapped-not-embedded)) requires resilience concerns (rate limiting, circuit breaking, retries) to be applied as middleware around adapter calls, not embedded inside each adapter. The implementation uses the **decorator pattern**: each resilience concern is a wrapper struct that implements the same adapter interface as the underlying raw adapter, holds a reference to an inner adapter, and adds its concern on top.

### Three-layer structure

```
┌─────────────────────────────────────────────────┐
│   Outer wrapper: rate-limited adapter           │
│   (implements CEXAdapter)                       │
│   ┌──────────────────────────────────────────┐  │
│   │ Middle wrapper: circuit-broken adapter   │  │
│   │ (implements CEXAdapter)                  │  │
│   │   ┌────────────────────────────────────┐ │  │
│   │   │ Inner: raw Binance adapter         │ │  │
│   │   │ (implements CEXAdapter)            │ │  │
│   │   │ — only does the HTTP call          │ │  │
│   │   └────────────────────────────────────┘ │  │
│   └──────────────────────────────────────────┘  │
└─────────────────────────────────────────────────┘
```

All three layers implement the same `CEXAdapter` interface. The Snapshot Coordinator holds a reference to the outermost wrapper and is unaware of the layers underneath.

### Code shape

```go
// The contract — internal/cex/adapter.go
type CEXAdapter interface {
    EffectivePrices(ctx context.Context, pair Pair, sizes []decimal.Decimal) ([]VenuePrice, error)
}

// The raw adapter — internal/cex/binance/adapter.go
// Knows nothing about rate limits or circuit breakers.
type binanceAdapter struct {
    httpClient *http.Client
    baseURL    string
}

func (b *binanceAdapter) EffectivePrices(ctx context.Context, pair Pair, sizes []decimal.Decimal) ([]VenuePrice, error) {
    // fetch /api/v3/depth, parse the orderbook, walk the book for each size
}

// A rate-limit wrapper — internal/resilience/ratelimit.go (generic)
type rateLimitedCEX struct {
    inner   CEXAdapter
    limiter RateLimiter
}

func (r *rateLimitedCEX) EffectivePrices(ctx context.Context, pair Pair, sizes []decimal.Decimal) ([]VenuePrice, error) {
    if err := r.limiter.Wait(ctx); err != nil {
        return nil, err
    }
    return r.inner.EffectivePrices(ctx, pair, sizes)
}

// Wiring — cmd/arbd/main.go
adapter := &rateLimitedCEX{
    inner: &circuitBrokenCEX{
        inner: &binanceAdapter{httpClient: client, baseURL: cfg.Binance.BaseURL},
        cb:    breaker,
    },
    limiter: limiter,
}
```

The DEX side mirrors the same pattern with `DEXAdapter` instead of `CEXAdapter`.

### Why this shape

- **The raw adapter stays dumb**: it only knows how to talk to its venue. One mock HTTP server is enough to unit-test it.
- **Each wrapper is independently testable**: the rate-limit wrapper is tested by injecting a fake adapter that records calls; the test verifies the wrapper waits on the limiter before delegating. No real network needed.
- **Composable**: want only rate limiting? Drop the circuit-breaker wrapper. Want logging too? Add another wrapper. The wiring change is one line in `main.go`.
- **Reusable across venues**: the same `rateLimitedCEX` wrapper works for the Coinbase adapter, the Kraken adapter, etc. The Binance adapter does not bring its own rate-limit code; the generic wrapper applies it.
- **Order of stacking is intentional and explicit**: rate limit outermost (refuses to even start the work if the budget is exhausted), circuit breaker inside (short-circuits if the venue is failing), raw adapter innermost (does the work).

### HTTP transport-level concerns sit one layer lower

Retries and per-request timeouts on transient HTTP errors (5xx, network timeouts) are best applied **inside the raw adapter's HTTP client**, via a custom `http.RoundTripper` on the `http.Client`. That way every HTTP call gets the same retry / timeout policy uniformly without polluting the adapter's body code.

The split of concerns is:

| Concern | Where it lives |
|---|---|
| Per-request retry on transient failures | Inside the raw adapter's HTTP transport (`RoundTripper`) |
| Per-request timeout | Inside the raw adapter's HTTP client + per-call `context` deadline |
| Rate limiting (per adapter / per venue) | Wrapper around the adapter |
| Circuit breaking | Wrapper around the adapter |
| Structured request logging | Either layer — `RoundTripper` for HTTP-level, wrapper for adapter-level |

The DEX adapter uses `eth_call` via the `ethclient` library rather than raw HTTP, so its "transport layer" is the configured client; retries and timeouts there are applied through the client's configuration and per-call context deadlines.

---

## Numeric types for financial math

Architecture decision 8 (in [`architecture.md`](./architecture.md#8-precise-decimal-arithmetic-for-prices)) requires exact decimal arithmetic for all prices and amounts. The concrete type used is `shopspring/decimal.Decimal`.

Rationale for this specific library:

- `float64` is rejected outright: precision errors compound through walk-the-book calculations and produce phantom or missed arbitrage detections at the margin.
- `big.Float` from the standard library would be correct but its API is more cumbersome for arithmetic-heavy code.
- `shopspring/decimal` is the standard choice for financial code in Go, listed in the challenge's recommended libraries, and provides an arithmetic-friendly API.

Raw amounts from external systems are normalized to `decimal.Decimal` at the adapter boundary; nothing downstream sees the native representations.
