# Implementation <!-- omit from toc -->

This document maps the architecture described in [`architecture.md`](./architecture.md) to its Go-level structure: package layout, interface seam locations, and code-level conventions. It is the implementation-detail counterpart to architecture.md and will grow as the code lands.

For the conceptual architecture (what components exist, how they relate, the design decisions and trade-offs), see [`architecture.md`](./architecture.md). For business context see [`business.md`](./business.md), and for known limitations see [`limitations.md`](./limitations.md).

---

## Table of Contents <!-- omit from toc -->

- [Package layout](#package-layout)
- [Conventions](#conventions)
- [Streaming pipeline and concurrency model](#streaming-pipeline-and-concurrency-model)
- [Resilience composition pattern](#resilience-composition-pattern)
- [Defensive correctness at boundaries](#defensive-correctness-at-boundaries)
- [Encapsulation: venue specifics stay in the adapter](#encapsulation-venue-specifics-stay-in-the-adapter)
- [Senior Engineer Requirements: what we addressed and what we deferred](#senior-engineer-requirements-what-we-addressed-and-what-we-deferred)
- [Numeric types for financial math](#numeric-types-for-financial-math)

---

## Package layout

```
terrace-challenge/
├── cmd/
│   ├── arbd/main.go                  # wire components, start runtime
│   ├── probe-binance/main.go         # diagnostic: Binance effective prices
│   ├── probe-uniswap/main.go         # diagnostic: QuoterV2 effective prices
│   └── probe-chain/main.go           # diagnostic: WS newHeads + reconnect
├── internal/                         # not importable by external code
│   ├── chain/
│   │   └── subscriber.go             # WS newHeads + reconnect; BlockEvent type
│   ├── cex/binance/
│   │   ├── client.go                 # HTTP client; EffectivePrices
│   │   ├── orderbook.go              # walk book → fee-adjusted price (pure)
│   │   └── symbols.go                # Symbol metadata, taker fee
│   ├── dex/uniswapv3/
│   │   ├── client.go                 # eth_call wrapper; EffectivePrices
│   │   ├── abi.go                    # QuoterV2 ABI
│   │   ├── decimals.go               # raw ↔ human amount conversion
│   │   └── tokens.go                 # Token / Pool metadata
│   ├── pricing/quote.go              # unified Quote / Quotes types
│   ├── pipeline/
│   │   ├── dispatcher.go             # per-block fan-out, Snapshotter iface
│   │   ├── binance.go                # Binance Snapshotter implementation
│   │   └── uniswap.go                # Uniswap Snapshotter implementation
│   ├── pathfinder/pathfinder.go      # candidate-path enumeration (pure)
│   ├── arbitrage/evaluator.go        # cost model → Opportunity (pure)
│   ├── alert/textsink.go             # structured + optional pretty output
│   ├── config/config.go              # YAML loader (config.yaml)
│   └── resilience/
│       ├── ratelimit.go              # token bucket (wraps x/time/rate)
│       ├── breaker.go                # circuit breaker (wraps gobreaker)
│       └── retry.go                  # NewHTTPClient + transport wrappers
├── config.yaml
├── go.mod
└── (docs at repo root)
```

---

## Conventions

- **`cmd/arbd/`** is the only package that knows how the pieces fit together. Everything else is a library that knows nothing about how it is wired. This makes alternative configurations easy and tests trivial.
- **`internal/`** keeps the application code unimportable from outside the module. Standard Go convention for application code; prevents accidental coupling if the repo grows.
- **Adapters live in subdirectories named after the venue** (`cex/binance/`, `dex/uniswapv3/`). Adding a second CEX is `cex/coinbase/`; adding a second DEX is `dex/sushiswapv3/`. The Snapshotter interface that unifies them lives in `pipeline/`, which is the only consumer.
- **`pricing/`, `pathfinder/`, and `arbitrage/` are pure packages** — no network, no I/O, no goroutines. They are the unit-test sweet spot. `pricing/` is imported by the adapter packages so that adapters produce the unified effective-price shape directly. `pathfinder/` and `arbitrage/` are downstream pipeline stages: the first finds candidate paths, the second evaluates their profitability.
- **`resilience/` is generic** — nothing in it knows about CEX vs DEX. It exposes a `NewHTTPClient` factory that the adapters' HTTP clients use; the resilience layers live at the HTTP transport, one stack per external host.

---

## Streaming pipeline and concurrency model

The pipeline is four stages connected by channels — each stage runs in its own goroutine, owns its output channel, and closes it on `defer` when its `Run` returns:

```
Subscriber  ──BlockEvent──▶  Dispatcher  ──VenueResult──▶  Pathfinder  ──CandidatePath──▶  Evaluator
```

The Dispatcher consumes anything that implements `Snapshotter` (one method: `Snapshot(ctx, BlockEvent) (Quotes, error)`) — the only polymorphic seam in the pipeline. `BinanceSnapshotter` and `UniswapSnapshotter` are concrete implementations bound to their venue config at construction time, registered with the Dispatcher via a `map[string]Snapshotter` in `main.go`; adding a venue is one new implementation plus one map entry. Every other stage (Dispatcher, Pathfinder, Evaluator) is a concrete struct or pure function — abstractable later if real flexibility shows up.

Downstream `for x := range stage.Out()` loops terminate naturally when the upstream `Run` returns — no separate done-channel, no signalling protocol on top of the channel send. The Subscriber close-on-defer guarantees this even on the ctx-cancel exit path.

**Failure scope is per-venue, not per-block.** A `VenueResult` carries `Quotes` *or* an `Err` — one venue's HTTP timeout in a given block doesn't poison the other venue's snapshot. The Pathfinder simply doesn't have anything to pair against for that block-venue pair, logs the error with venue identity, and moves on. The next block is a fresh evaluation. The same shape recurses one layer down: a `pricing.Quote` carries a per-row `Err`, so a venue snapshot can succeed overall but have one size mark insufficient depth without losing the other rows.

**Parallel calls inside a Snapshot.** A single Uniswap Snapshot fires 2N concurrent `eth_call`s — one per `(size, direction)` — through Go 1.25's `sync.WaitGroup.Go`: no manual `wg.Add/Done`, no goroutine wrapper at the call site, the abstraction is just *"run this and wait."* HTTP/2 multiplexes the lot over a single TCP connection to the RPC provider, so the apparent parallelism is one syscall's worth of work at the kernel level.

**Context-cancellation is the only shutdown signal.** A single SIGTERM-handling context flows through every `Run(ctx, ...)`. When ctx cancels, every stage's `select { case <-ctx.Done(): return ctx.Err() }` arm fires, the stage closes its output channel on the way out, and downstream stages see channel close on their read. Cleanly draining the whole pipeline at SIGTERM is the same code path as cleanly draining a single test channel.

**Goroutine ownership stays local.** Every long-running goroutine is owned by exactly one stage, and the stage's `Run` is the only thing that can spawn or join it. The Dispatcher does spawn per-block per-venue goroutines via `pending.WaitGroup`, but `defer pending.Wait()` running before `defer close(d.out)` (deferred order is LIFO) means in-flight dispatchers always exit before the output channel closes, avoiding send-on-closed-channel panics by construction. Failure modes are local: a leak in stage X tracks back to stage X.

---

## Resilience composition pattern

Architecture decision 6 (in [`architecture.md`](./architecture.md#6-resilience-is-wrapped-not-embedded)) requires resilience concerns — rate limiting, circuit breaking, retries — to be applied as middleware around external dependencies, not embedded inside each adapter. The implementation uses the **decorator pattern at the HTTP transport layer**: each concern is an `http.RoundTripper` (or surrounding helper) that wraps the next, composed into a single `*http.Client` returned by `resilience.NewHTTPClient`. Each adapter accepts that `*http.Client` and uses it for every outbound call.

### Four-layer structure (per host)

```
┌─────────────────────────────────────────────────┐
│   retry  (hashicorp/go-retryablehttp)           │
│   ┌──────────────────────────────────────────┐  │
│   │ circuit breaker  (sony/gobreaker)        │  │
│   │   ┌────────────────────────────────────┐ │  │
│   │   │ rate limit  (golang.org/x/time/rate)│ │  │
│   │   │   ┌──────────────────────────────┐ │ │  │
│   │   │   │ real http.Transport          │ │ │  │
│   │   │   └──────────────────────────────┘ │ │  │
│   │   └────────────────────────────────────┘ │  │
│   └──────────────────────────────────────────┘  │
└─────────────────────────────────────────────────┘
```

One stack per external host. Binance and the Uniswap RPC provider each get their own breaker, rate budget, and retry policy — failures in one venue cannot trip the other's breaker, and partial results from healthy endpoints in a future multi-endpoint Snapshotter survive.

### Why HTTP-transport layer, not the Snapshotter

A Snapshotter-level shape was considered — wrapping each `pipeline.Snapshotter` with `RateLimited` + `CircuitBroken` decorators that implement the same interface as the inner one. Two problems ruled it out:

- **Rate limit would be misaligned with the venue's actual quota.** A Snapshotter wraps a logical "fetch quotes for these sizes" operation, which fires multiple HTTP calls underneath. With retries inside the raw client, one Snapshot that retried 4 times would consume only 1 rate token — so the effective HTTP rate during failure bursts could reach 5× the nominal budget.
- **Breaker granularity would be wrong for partial results.** A Snapshotter that internally fetches from multiple endpoints (multi-pool DEX queries, multi-RPC failover, multi-CEX aggregation in a future extension) should be able to return partial results when only some endpoints are sick. A Snapshot-level breaker would fail the whole Snapshot on one bad endpoint.

Per-host HTTP transport avoids both: each HTTP call is exactly one rate token, and each external dependency has its own breaker.

### Stacking rationale

- **Retry outermost** so it sees the final outcome of each attempt (real success, transient blip, breaker rejection) and decides whether to keep going.
- **Breaker inside retry** so its `ErrOpenState` surfaces to the retry layer, where `CheckRetry` classifies it as permanent and aborts. Retries don't burn budget on a known-open breaker.
- **Rate limit innermost** so it gates actual network I/O; calls rejected by the breaker never consume a token.
- **Real transport** innermost does the work.

### Classification details

- The breaker transport translates 5xx HTTP responses into breaker-counted failures. Go's HTTP semantics return `(resp_500, nil)` for 5xx, so without this a server consistently returning 500 would never trip the breaker. The 5xx marker error is swallowed on the way out so the response itself still flows upstream — retryablehttp's policy sees the status code and decides whether to retry.
- `IsSuccessful` skips `context.Canceled` (caller decision, e.g., SIGTERM mid-call — nothing about the dependency went wrong) but counts `context.DeadlineExceeded` (provider didn't respond within our configured timeout — that IS a health signal).
- `CheckRetry` recognises `gobreaker.ErrOpenState` and returns `(false, err)` immediately, propagating the breaker error upstream without further attempts.

### Wiring

`resilience.NewHTTPClient` is constructed once per external host in `cmd/arbd/main.go`, with per-venue rate / breaker / retry parameters. The resulting `*http.Client` is passed into `binance.NewClientWithHTTP` and into Uniswap via `rpc.DialOptions(ctx, url, rpc.WithHTTPClient(c))` + `ethclient.NewClient(rpc)`, so every JSON-RPC eth_call inherits the four-layer stack.

The Snapshotter implementations (`pipeline.BinanceSnapshotter`, `pipeline.UniswapSnapshotter`) are unaware of the resilience layers — they just call the raw client whose HTTP transport carries them.

### Why this shape

- **Generic and library-agnostic from the adapter's POV.** Both adapters depend only on `*http.Client`. Swapping the underlying breaker library is a change inside `internal/resilience/` with no ripple.
- **Each layer is independently testable.** The breaker has its own state-transition tests; the retry transport is exercised by an `httptest` server that 503s twice then succeeds; the rate limit gates by elapsed time.
- **Composable per call site.** `HTTPClientConfig` fields are optional — pass nil for any layer to skip it. Tests construct clients with just `Retry` (no breaker, no limiter); production wires all four.
- **Reusable across venues.** The same factory works for any HTTP-speaking dependency; the Uniswap JSON-RPC client uses it via `rpc.WithHTTPClient`.

### What stays outside this stack

- **Per-call timeouts** apply via `HTTPClientConfig.RequestTimeout` (per attempt, not per total retry budget — a single hung connection cannot consume the whole window) and the caller's `context` deadline.
- **Structured request logging** flows through retryablehttp's `LeveledLogger` interface, adapted to slog inside `resilience.NewHTTPClient`. Operator-facing logs (breaker state changes, rate-limit waits, breaker rejections) live in the wrapper types and emit through slog directly.

---

## Defensive correctness at boundaries

A handful of places where boundary checks earn their keep — each one motivated by a failure mode that would otherwise be silent.

- **Cross-venue size alignment in the Pathfinder.** Within a single venue, `Buy[i].Size == Sell[i].Size` by adapter construction — both slices are written in the same loop. Across venues, that index correspondence is a *system* invariant (both Snapshotters are constructed with the same `tradeSizes` slice in `cmd/arbd`), not a *contract* on `pricing.Quotes` itself. The Pathfinder verifies both `len(r.Buy) == len(other.Buy)` and each `r.Buy[i].Size.Equal(other.Buy[i].Size)` before pairing; mismatches log a structured WARN and skip rather than silently producing mis-paired candidates. Belt-and-braces against a future venue whose configured size set drifts from the others'.
- **Gas units come from QuoterV2, not a constant.** Each per-call `gasEstimate` slot in QuoterV2's return tuple is the contract's own simulation against the current pool state. The Pathfinder sums the two legs onto each `CandidatePath`, so the Evaluator uses real per-trade gas (≈95k for 1 ETH at the current pool, ≈125k for 100 ETH as more ticks cross) rather than the rule-of-thumb 150k that ships in most arbitrage examples.

---

## Encapsulation: venue specifics stay in the adapter

A core architectural rule shapes the adapter contracts: **downstream code never reasons about a venue's intrinsic cost structure.** The implementation realises that rule at the type-system level.

- **Trading fees are folded into `pricing.Quote.Price` at the adapter.** Binance's `EffectivePrices` walks the orderbook and then applies `(1 ± TakerFeeBps/10000)` to the per-unit price before returning. Uniswap's QuoterV2 already returns post-fee prices (the pool's fee is encoded on-chain and applied by the contract). The Pathfinder and Evaluator see `Quote.Price` as *what the trader actually pays per unit*, not a raw orderbook mid that needs further adjustment. Adding a new CEX is a `TakerFeeBps` field on its `Symbol` struct — no cost-model code touched.
- **Gas estimates travel per-candidate, not per-venue.** Each `pricing.Quote` carries a `GasEstimate uint64` populated by the venue adapter (QuoterV2's per-call output for Uniswap, zero for off-chain Binance). The Pathfinder sums the two legs onto `CandidatePath.GasEstimate`. The Evaluator multiplies that by `BaseFee` to get gas-in-ETH and converts to USDC. The cost model has no per-venue gas knowledge; it just adds the gas it was given.
- **Per-row `Quote.Err` carries depth-exhaustion alongside successful sizes.** A venue snapshot can succeed overall but have one size that exceeded available orderbook depth — that row's `Err` is set, others remain valid. Downstream consumers iterate without losing the partial result. The same "errors flow alongside successes" pattern recurses at the `VenueResult` layer (per-venue failures inside a per-block fan-out) and at the alert layer (the structured slog event always fires; the multi-line block is optional).

A grep for `binance` or `uniswap` in the downstream packages (`pathfinder/`, `arbitrage/`, `pricing/`) returns zero hits — venue identity is just a string label on `VenueResult` and `CandidatePath`, used for alert formatting but never branched on.

---

## Senior Engineer Requirements: what we addressed and what we deferred

The brief lists six "Senior Engineer Requirements" beyond the core detector ([`CHALLENGE.md`](./CHALLENGE.md#senior-engineer-requirements)). This section walks through how each was handled — including the items where the deliberate choice was to *not* implement them, with the reasoning behind that.

### 1. Caching strategy — deliberately not implemented

The brief asks for a multi-layered cache covering pool state, gas estimates, and orderbook data.

For the current detector, the Ethereum block is the bot's clock, which makes caching structurally unhelpful. Every block triggers a fresh snapshot; Uniswap V3 pool state changes within a block the moment a swap settles; Binance's orderbook is continuously updated; gas prices move per block via EIP-1559. There's no time window in which a cached value would be both fresh enough to use and old enough to be worth caching: any TTL ≤ block time produces no hit rate, any TTL > block time risks acting on stale data.

Where caching starts to make sense is the future graph-based shape described in architecture.md's [multi-feature evolution](./architecture.md#multi-feature-evolution-stateful-market-graph--per-service-derived-state) — at that point caching is worth revisiting. In the current single-consumer / block-clock shape there's nothing useful to cache.

### 2. WebSocket management & connection resiliency — implemented

| Sub-requirement | Status |
|---|---|
| Persistent WS to Ethereum node | ✓ `chain.Subscriber` maintains a persistent `newHeads` subscription |
| Heartbeat / ping mechanism | Delegated to go-ethereum's WS client, which handles WS-protocol ping/pong with the provider; we don't surface an application-level heartbeat |
| Reconnect with exponential backoff + jitter | ✓ `cenkalti/backoff/v5` driving the dial-retry loop (1s initial, 30s cap, ±50% jitter) |
| Track last block + resume without gaps | Partial — block numbers are monotonic and consumers can detect gaps from the `BlockEvent.Number` sequence, but missed blocks during an outage are not backfilled. Detection-only system: discussed in [`limitations.md`](./limitations.md) §1 (block-boundary sampling) and §6 (chain reorgs) |
| Connection cleanup | ✓ `defer sub.Close()` on every reconnect path |

### 3. Concurrency & performance — implemented

| Sub-requirement | Status |
|---|---|
| Goroutines + channels used effectively | ✓ The whole pipeline is channels between per-stage `Run` goroutines (see [Streaming pipeline and concurrency model](#streaming-pipeline-and-concurrency-model) above) |
| Worker pools for API calls | Not used — the workload is one snapshot per venue per block (≤ 6 concurrent `eth_call`s on the DEX side). Ad-hoc goroutines via `sync.WaitGroup.Go` are simpler than a fixed pool at this throughput; a pool would add bookkeeping for no benefit |
| Backpressure | The Dispatcher's `select { case d.out <- result: case <-ctx.Done(): }` propagates blockage upstream without dropping data; the Pathfinder's freshness filter ensures a slow venue can't poison fresher block evaluations |
| Avoid race conditions | ✓ `go test -race` mandatory in CI; per-venue result channels are single-writer by construction |
| Graceful shutdown with ctx cancellation | ✓ One `signal.NotifyContext` flows through every `Run`; every stage closes its output channel on the way out |

### 4. Rate limiting & resiliency — implemented

All four items live in `internal/resilience/`, composed into a single `*http.Client` per external host. The four-layer transport stack and its design rationale are in [Resilience composition pattern](#resilience-composition-pattern) above.

| Sub-requirement | Implementation |
|---|---|
| Rate limiting per external API | Token-bucket via `golang.org/x/time/rate`, per-host instance — each HTTP call consumes exactly one token, including retry attempts |
| Exponential backoff + jitter for retries | `hashicorp/go-retryablehttp` with Retry-After honouring, body rewinding, and ±50% jitter |
| Circuit breaker | `sony/gobreaker/v2` with failure-ratio policy (`MinRequests=20`, `FailureRatio=0.2`, `Interval=1m`); `IsSuccessful` distinguishes caller cancellation (`Canceled`) from provider slowness (`DeadlineExceeded`) so SIGTERM doesn't cosmetically trip the breaker |
| Metrics / observability hooks | Structured `slog` throughout with per-venue identifiers; `OnStateChange` emits breaker transitions at WARN; rate-limit waits and breaker rejections log at DEBUG. The brief's *"even if just structured logging"* exit is taken |

### 5. Configuration & extensibility — implemented

| Sub-requirement | Status |
|---|---|
| Support multiple trading pairs (design for ETH-USDC, but extensible) | Pair tokens are exported constants (`uniswapv3.WETH`, `uniswapv3.USDC`) consumed from `cmd/arbd`. Adding a second pair is a new constant + a wiring change. The downstream pipeline (Pathfinder, Evaluator) is pair-agnostic — `grep -r 'WETH\|USDC' internal/pathfinder internal/arbitrage` returns zero hits |
| Configurable trade sizes | ✓ `config.yaml` field `trade_sizes`; loader rejects empty / non-positive values at startup |
| Pluggable exchange adapters (interface-based) | ✓ `pipeline.Snapshotter` is the unification seam — one method, `Snapshot(ctx, BlockEvent) (Quotes, error)`. Adding `cex/coinbase/` or `dex/sushiswapv3/` is a new package implementing that interface plus a `pipeline.NewXxxSnapshotter` call in `main.go`. Downstream code never branches on venue identity |
| Configuration via file or environment | ✓ `config.yaml` for behavior (trade sizes, profit threshold, per-venue resilience tuning) + `.env` for environment bindings (URLs, addresses, runtime mode). The split is documented in [`README.md`](./README.md#configuration) |

### 6. Data modelling & architecture — implemented

| Sub-requirement | Status |
|---|---|
| Clear separation of concerns | The package layout mirrors the responsibility chart: `internal/chain/` (block subscription), `internal/cex/binance/` + `internal/dex/uniswapv3/` (raw adapters), `internal/pipeline/` (per-block fan-out + Snapshotter interface), `internal/pathfinder/` (correlation), `internal/arbitrage/` (cost model — pure), `internal/alert/` (output formatting), `internal/resilience/` (cross-cutting). `cmd/arbd/` is the only package that knows how the pieces fit together |
| Well-defined interfaces | One unification seam (`Snapshotter`) plus two configuration seams (`*http.Client` via `resilience.NewHTTPClient`, `BreakerConfig.OnStateChange`). Other components stay concrete — abstracting them would add noise without enabling real flexibility |
| Error types and handling | Per-row `pricing.Quote.Err` (depth exhaustion at a specific size); per-venue `pipeline.VenueResult.Err` (HTTP timeout, RPC error). Errors flow alongside successes at every layer — partial results survive |
| Testability | 87.5% statement coverage on `internal/` (cmd/ excluded as wiring; exercised by live smoke tests per phase); race detector in CI; the decoupled package layout means every component is unit-testable in isolation. The pure packages (`pricing/`, `pathfinder/`, `arbitrage/`) are particularly inexpensive to test thoroughly |

---

## Numeric types for financial math

Architecture decision 8 (in [`architecture.md`](./architecture.md#8-precise-decimal-arithmetic-for-prices)) requires exact decimal arithmetic for all prices and amounts. The concrete type used is `shopspring/decimal.Decimal`.

Rationale for this specific library:

- `float64` is rejected outright: precision errors compound through walk-the-book calculations and produce phantom or missed arbitrage detections at the margin.
- `big.Float` from the standard library would be correct but its API is more cumbersome for arithmetic-heavy code.
- `shopspring/decimal` is the standard choice for financial code in Go, listed in the challenge's recommended libraries, and provides an arithmetic-friendly API.

Raw amounts from external systems are normalized to `decimal.Decimal` at the adapter boundary; nothing downstream sees the native representations.
