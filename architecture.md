# Architecture <!-- omit from toc -->

This document describes the architecture of the detection system, the design decisions behind it, and the trade-offs they imply. It is the structural counterpart to [`business.md`](./business.md) (the *why*) and [`limitations.md`](./limitations.md) (the *what's intentionally out of scope*). For the original challenge requirements, see [`CHALLENGE.md`](./CHALLENGE.md).

The architecture is deliberately a **single Go process** with no message broker and no horizontal scaling. The final section of this document describes what a production-scale version would look like and what would change.

---

## Table of Contents <!-- omit from toc -->

- [High-level overview](#high-level-overview)
- [Component responsibilities](#component-responsibilities)
- [Data flow](#data-flow)
- [Design decisions](#design-decisions)
  - [1. The Block Subscriber is the only producer](#1-the-block-subscriber-is-the-only-producer)
  - [2. Cancel old block, process new (backpressure)](#2-cancel-old-block-process-new-backpressure)
  - [3. Adapters emit a unified effective-price shape](#3-adapters-emit-a-unified-effective-price-shape)
  - [4. Adapters own multi-size handling internally](#4-adapters-own-multi-size-handling-internally)
  - [5. Path discovery is separate from profitability evaluation](#5-path-discovery-is-separate-from-profitability-evaluation)
  - [6. Resilience is wrapped, not embedded](#6-resilience-is-wrapped-not-embedded)
  - [7. Single binary, single process](#7-single-binary-single-process)
  - [8. Precise decimal arithmetic for prices](#8-precise-decimal-arithmetic-for-prices)
  - [9. Output is a pluggable sink](#9-output-is-a-pluggable-sink)
- [Trade-offs and simplifications](#trade-offs-and-simplifications)
- [What a production-scale version would add](#what-a-production-scale-version-would-add)
  - [Message broker between the block clock and adapters](#message-broker-between-the-block-clock-and-adapters)
  - [Horizontal scaling per adapter](#horizontal-scaling-per-adapter)
  - [Pathfinder and Evaluator as stateless consumers](#pathfinder-and-evaluator-as-stateless-consumers)
  - [State and history store](#state-and-history-store)
  - [Observability stack](#observability-stack)
  - [Other production concerns](#other-production-concerns)

---

## High-level overview

The system is built around the **synchronized snapshot per block** pattern motivated in [`business.md`](./business.md). The Ethereum block subscriber is the heartbeat; everything downstream reacts to its tick. On each block:

1. A block event is emitted by the block subscriber.
2. The snapshot coordinator dispatches one unit of work to each registered venue adapter in parallel — *"produce effective prices for this pair at the configured trade sizes."*
3. Each adapter handles its venue's optimal access pattern internally (the CEX does one fetch and walks the book per size; the DEX does one simulated swap per size) and returns a **unified effective-price list** — the same shape from both venues, slippage-aware, fee-adjusted.
4. The Pathfinder enumerates **candidate paths** from the paired effective-price data — each path is a fully-specified prospective arbitrage trade (size, buy venue, sell venue, observed prices).
5. The Profitability Evaluator applies CEX trading fees and gas estimates to each candidate, and emits a structured opportunity when the net spread crosses the configured threshold.
6. The output sink formats and emits each opportunity.

Pricing math (orderbook walking, quote-to-unit-price conversion) sits within the adapter layer as a shared concern — applied by each adapter as part of producing its output, not as a separate pipeline stage between adapters and detector. The shape that emerges at the adapter boundary is the unified one; nothing downstream needs to perform further normalization.

```
┌────────────────────────────────────────────────────────────────────────┐
│                          BLOCK SUBSCRIBER                              │
│              (WebSocket to Ethereum, newHeads stream)                  │
│                                                                        │
│   Wrapped in: reconnect-with-backoff, last-block tracking,             │
│               heartbeat detection, HTTP polling fallback               │
└────────────────────────────────┬───────────────────────────────────────┘
                                 │   tick: block event
                                 ▼
                  ┌──────────────────────────────┐
                  │     SNAPSHOT COORDINATOR     │
                  │                              │
                  │  Per tick, dispatch one      │
                  │  unit of work per adapter:   │
                  │  "effective prices for this  │
                  │   pair at these sizes."      │
                  │                              │
                  │  Owns per-block context and  │
                  │  cancels in-flight work when │
                  │  a newer block arrives.      │
                  └────┬─────────────────────┬───┘
                       │                     │
              ┌────────▼─────────┐  ┌────────▼─────────┐
              │   CEX ADAPTER    │  │   DEX ADAPTER    │
              │    (Binance)     │  │  (Uniswap V3)    │
              │                  │  │                  │
              │ Internally:      │  │ Internally:      │
              │ • 1 REST fetch   │  │ • N simulated    │
              │   of the top of  │  │   swap calls     │
              │   the book       │  │   (one per size) │
              │ • Walk book for  │  │ • Convert each   │
              │   each size+side │  │   quote to a     │
              │   to compute     │  │   per-unit       │
              │   effective price│  │   effective price│
              │                  │  │                  │
              │ Wrapped in:      │  │ Wrapped in:      │
              │  rate limiter,   │  │  rate limiter,   │
              │  circuit breaker │  │  circuit breaker │
              └────────┬─────────┘  └────────┬─────────┘
                       │                     │
                       │  unified            │  unified
                       │  effective-price    │  effective-price
                       │  list               │  list
                       └─────────┬───────────┘
                                 │  fan-in: paired prices for the block
                                 ▼
                  ┌──────────────────────────────┐
                  │         PATHFINDER           │
                  │                              │
                  │  Enumerate candidate paths   │
                  │  from the paired data — each │
                  │  path is a fully-specified   │
                  │  prospective arbitrage trade │
                  │  (size, buy venue, sell      │
                  │  venue, observed prices).    │
                  │                              │
                  │  At current scope: simple    │
                  │  pairing. Naturally extends  │
                  │  to multi-venue and          │
                  │  multi-hop routing.          │
                  └────────────┬─────────────────┘
                               │  list of candidate paths
                               ▼
                  ┌──────────────────────────────┐
                  │   PROFITABILITY EVALUATOR    │
                  │                              │
                  │  Per candidate path:         │
                  │   • Apply CEX trading fee    │
                  │   • Subtract estimated gas   │
                  │   • If net ≥ threshold,      │
                  │     emit Opportunity         │
                  └────────────┬─────────────────┘
                               │
                               ▼
                  ┌──────────────────────────────┐
                  │      OUTPUT SINK             │
                  │   Pluggable interface;       │
                  │   default = structured stdout│
                  └──────────────────────────────┘

         ┌─────────────────────────────────────────────────┐
         │  Cross-cutting: config, structured logger,      │
         │  root context, signal handler → graceful exit   │
         │                                                 │
         │  Pricing math (orderbook walk, quote-to-unit    │
         │  conversion) is a shared concern applied        │
         │  within each adapter, not a pipeline stage.     │
         └─────────────────────────────────────────────────┘
```

---

## Component responsibilities

| Component | Owns | Doesn't touch |
|---|---|---|
| **Block Subscriber** | WebSocket lifecycle, reconnect logic, block-number monotonicity, gap recovery, fallback to HTTP polling | Anything venue-specific |
| **Snapshot Coordinator** | Per-tick dispatch of one unit of work per adapter, paired fan-in, per-block timeout, backpressure policy | Pricing math, business rules, venue-specific access patterns |
| **CEX Adapter (Binance)** | Fetching the orderbook, walking it for each configured `(size, side)`, producing the unified effective-price list | DEX, Ethereum, what counts as "profitable" |
| **DEX Adapter (Uniswap V3)** | Issuing one simulated swap per configured size, converting each quote to a per-unit effective price, producing the unified effective-price list | CEX, blockchain subscription, what counts as "profitable" |
| **Pathfinder** | Enumerating candidate paths from the paired effective-price data. Each path is a fully-specified prospective arbitrage trade (size, buy venue, sell venue, observed prices). At current scope this is simple pairing; the abstraction extends naturally to multi-venue and multi-hop routing without disturbing downstream cost logic. | Costs, fees, thresholds |
| **Profitability Evaluator** | Applying the cost model (CEX trading fee, gas estimate) to each candidate path, evaluating against the configured threshold, and constructing the `Opportunity` when it qualifies | How the path was discovered, how data was fetched |
| **Output Sink** | Formatting and emitting alerts | Detection logic |
| **Resilience middleware** | Rate limiting, circuit breaking, retries, backoff with jitter | Domain logic |
| **Pricing math (shared concern)** | Slippage-aware walk-the-book and quote-to-unit-price conversion, applied within each adapter as part of producing the unified shape | I/O, orchestration, opportunity decisions |

The separation makes the architecture testable: the Pathfinder, the Profitability Evaluator, and the pricing math are all **pure functions** over data structures, trivially unit-tested in isolation. Adapters are isolated behind interfaces and can be mocked at the seam. The Block Subscriber is the only piece with messy real-world concerns and is correspondingly the most carefully tested.

---

## Data flow

```
Block Subscriber ─── block event ──► Snapshot Coordinator
                                            │
                                            │  dispatch in parallel,
                                            │  one unit of work per adapter:
                                            │  "produce effective prices for
                                            │   this pair at these sizes"
                                            │
                       ┌────────────────────┴────────────────────┐
                       ▼                                         ▼
              CEX Adapter (Binance)                  DEX Adapter (Uniswap V3)
              ┌──────────────────────┐               ┌──────────────────────┐
              │ 1 orderbook fetch    │               │ N simulated swaps    │
              │ + walk the book for  │               │   (one per size)     │
              │   each (size, side)  │               │ + convert each quote │
              │   to effective price │               │   to per-unit price  │
              └──────────┬───────────┘               └──────────┬───────────┘
                         │                                      │
                         │  unified effective-price list        │
                         │                                      │
                         └──────────────────┬───────────────────┘
                                            │  fan-in: paired prices for block
                                            ▼
                                       Pathfinder
                                  enumerate candidate paths
                                  from the paired data
                                            │
                                            │  list of candidate paths
                                            ▼
                                Profitability Evaluator
                                  apply fees + gas,
                                  emit Opportunity ≥ threshold
                                            │
                                            ▼
                                       Output Sink
```

Both adapters return the same shape: a list of effective prices, each entry tagged with venue, pair, size, side, and the per-unit price (slippage- and fee-applied). The Pathfinder consumes the two lists from a given block and produces candidate paths — at the current 2-venue scope, this is straightforward pairing by `(size, side)`. The Profitability Evaluator then applies the cost model to each candidate and emits opportunities that clear the threshold. Anything not pairable by the Pathfinder (e.g., a CEX entry without a matching DEX entry due to a partial failure) is logged and skipped, not crashed.

> The Go package layout and the locations of the interface seams in code are documented separately in [`implementation.md`](./implementation.md) to keep this document focused on architecture.

---

## Design decisions

### 1. The Block Subscriber is the only producer

A new block arriving is the only event that drives work. There is no separate poller for Binance or for the DEX. This is the cleanest match for the synchronized-snapshot model and produces alerts that are inherently atomic: every alert references a specific block, with both venues observed at that block.

**Alternative considered**: independent pollers per venue with a join step. Rejected because it complicates the timing semantics and creates a stale-data window between the two venues.

### 2. Cancel old block, process new (backpressure)

If block N's snapshot is still in flight when block N+1's tick arrives, **the in-flight work for block N is cancelled and N+1 is processed instead**. The freshest opportunity is the only one that matters; emitting stale alerts is worse than missing them.

Mechanism (conceptual): each snapshot job is scoped to a per-block cancellation signal that the coordinator triggers when a newer block arrives. Anything still in flight (network calls, computations) observes the signal and aborts.

**Trade-off**: under sustained slowness (RPC node lag), several consecutive blocks may be skipped. This is surfaced as a structured log warning so the operator can detect degradation.

**Alternatives considered**:

- *Queue both, process in order*: avoids losing data but creates a snowballing backlog under sustained slowness and serves alerts that no longer represent the current market.
- *Skip new block if previous is still in flight*: simpler than cancellation but discards the *fresher* observation, which is the opposite of what we want.

### 3. Adapters emit a unified effective-price shape

Both adapters produce the **same output type**: a list of effective prices, one entry per `(size, side)` for the configured trade sizes, with the per-unit price already slippage-aware and fee-adjusted. Binance's raw orderbook and Uniswap's raw amount-out are converted into this shape *inside the adapter*, before anything downstream sees them.

The pricing math itself (walking the book, dividing raw amounts) is a shared concern within the adapter layer, applied by each adapter as part of producing its output. It is not a separate pipeline stage: the math is centralized and conceptually shared between both adapters, but it is exercised inside each adapter so the data emerging at the adapter boundary is already in the canonical shape.

**Why**: letting the detector or pipeline receive two different venue-specific input shapes would break the same separation-of-concerns principle that motivates having adapters in the first place. The detector should never need to know what venue an effective price came from.

**Alternative considered**: place the unit-conversion and slippage math in a separate downstream pipeline stage that consumes raw venue-specific data emitted by each adapter, normalizes it, and only then hands a unified shape to the detector. Rejected because it would force that downstream stage to know venue-specific raw formats — undoing the separation-of-concerns that motivates having adapters at all — and it would add a pipeline stage whose only job is shape conversion.

### 4. Adapters own multi-size handling internally

Each adapter receives the **full list of configured trade sizes** as a single semantic unit of work per block, not one call per size. The CEX adapter does one orderbook fetch and walks it N times internally; the DEX adapter issues N simulated swap calls (one per size) internally.

The Snapshot Coordinator dispatches "produce effective prices for this pair at these sizes" to each adapter; it does not fan out per size.

**Why**: the two venues have structurally asymmetric optimal access patterns. For Binance, one fetch serves any number of sizes (the book is venue state at a moment in time). For Uniswap V3 via QuoterV2, each size requires its own simulated swap (the contract simulates discrete swaps; you cannot extrapolate across sizes). Pushing the per-size fan-out to the coordinator would force the CEX adapter to either re-fetch the same orderbook N times (wasteful and triple the rate-limit cost) or carry block-scoped internal cache state (which makes the adapter stateful in an unwelcome way).

**Principle**: the adapter is the boundary against an external system, and the external system's optimal access pattern should live behind that boundary. Different venues will continue to have different optimal patterns; the adapter abstracts the difference so the coordinator stays generic.

**Alternative considered**: coordinator fans out per size, adapter handles one size per call. Rejected for the reasons above.

### 5. Path discovery is separate from profitability evaluation

The pipeline downstream of the adapters is split into two components: the **Pathfinder** enumerates candidate trades from the paired effective-price data; the **Profitability Evaluator** applies the cost model and emits opportunities. At the current 2-venue scope, the Pathfinder is straightforward pairing logic — but its existence as a distinct component mirrors how production routing systems (notably Terrace's Pathfinder) decompose this problem.

**Why**: the two responsibilities grow along independent axes. Adding venues, splitting orders across venues, or supporting multi-hop routes is *path discovery* work; tuning the cost model with richer fees, dynamic gas, or MEV-adjusted scoring is *evaluation* work. Keeping them in one component would make every future extension touch the same surface; separating them now means each axis can grow without disturbing the other.

**Trade-off**: at the current scope this adds a small amount of structural overhead — one extra component in the pipeline whose logic is, today, trivial. The cost is small and the conceptual win is real: a reader of this codebase immediately sees the two responsibilities as separate concerns, and the natural extension path is explicit rather than implicit.

**Alternative considered**: a single "Arbitrage Detector" that pairs and evaluates in one step. Rejected because every future extension along either axis would re-open and complicate that single component.

### 6. Resilience is wrapped, not embedded

Rate limiting, circuit breaking, retries, and structured logging are **middleware** around adapter calls, not concerns inside each adapter. The Binance client just makes HTTP requests; a wrapper enforces the rate limit. The Uniswap client just makes `eth_call` requests; the same wrapper pattern applies.

This makes resilience composable and testable in isolation. The same circuit-breaker logic applies to any new adapter for free.

### 7. Single binary, single process

Everything runs in one Go process. Channels do the inter-component plumbing. No Kafka, no NATS, no Redis, no horizontal scaling.

This matches the spec's load (one block every 12 seconds, two external API calls per block) and avoids the over-engineering the challenge explicitly warns against. The trade-off is that this design does not scale beyond what one process can do; see the [production-scale section](#what-a-production-scale-version-would-add) for what changes when that boundary is crossed.

### 8. Precise decimal arithmetic for prices

Prices and amounts are represented using **exact decimal arithmetic** throughout the pricing and arbitrage layers. Floating-point representations are avoided entirely in financial math, even when values are small, because precision errors compound through walk-the-book calculations and produce phantom or missed arbitrage detections at the margin.

The specific numeric type chosen, and the rationale for it, is documented in [`implementation.md`](./implementation.md).

### 9. Output is a pluggable sink

The detector does not write to stdout directly. It hands each `Opportunity` to an `OpportunitySink` interface. The default implementation is structured log output to stdout; other implementations (Slack webhook, PagerDuty, Prometheus counter, Kafka producer, etc.) can be wired in without modifying detection logic.

This is the minimum required to discuss "how would you push to Slack / a monitoring system" in the interview without actually implementing it.

---

## Trade-offs and simplifications

The architecture above is correct for the scope of this challenge. It is deliberately *not* the architecture you would build for a production trading system. The following simplifications are accepted, in order of significance:

| Simplification | What it means | What we'd lose by removing it |
|---|---|---|
| **Single process** | All components share one address space and communicate in-process | At higher load, the WebSocket consumer and the adapter pool would contend for the same runtime resources. With multiple venues and pairs, a single process would saturate. |
| **No message broker** | The block subscriber speaks directly to one coordinator in the same process | A broker buys: durability across restarts, replay for backfill, multiple independent consumer groups, horizontal scaling of adapters |
| **No horizontal scaling** | Cannot run two instances safely; both would duplicate API calls and double the rate-limit footprint | The system has no leader election, no shard assignment, no deduplication of work across instances |
| **In-memory state only** | Last-processed block lives in process memory; restart loses it | A restart re-subscribes from the latest block, missing any blocks in the gap. Persisting the last-processed block to disk would let us resume cleanly. |
| **Caching stubbed** | No L1 cache for pool state or gas prices | Every block triggers a fresh `eth_call`; redundant under heavy querying. With caching, identical queries within the same block window would short-circuit. |
| **Circuit breaker as middleware skeleton, not configured per adapter** | The interface exists; concrete failure thresholds and recovery policies are left as TODOs | The system would still flap during sustained provider outages. A real config would tune trip thresholds per dependency. |
| **No tracing / metrics export** | Structured logs only; no Prometheus, no OpenTelemetry | An operator must grep logs to debug. Cannot answer "what's our p99 RPC latency?" without instrumentation. |
| **No persistence of opportunities** | Each detection is emitted and forgotten | Cannot answer "how many arb opportunities have we detected today?" without an external log aggregator capturing the stdout stream |
| **Single in-process snapshot coordinator** | All per-block work is paired in a single in-process fan-out / fan-in | Acceptable at 1 venue × 1 venue. With 10 venues, the coordinator becomes a serialization point and would itself need to be partitioned. |

Each of these is an intentional decision for a 4–10 hour exercise. None of them are bugs to be fixed; they are scope choices to be discussed.

---

## What a production-scale version would add

If this detector were to evolve into the kind of system Terrace operates — many venues, many pairs, real trading, 24/7 SLAs — the single-process architecture above would be decomposed into independent services connected by a message broker. Below is the shape such an evolution would take.

### Message broker between the block clock and adapters

A broker such as **NATS** (or **Kafka**, or **Redis Streams**) sits between the block subscriber and the adapter pool. The block subscriber publishes one `BlockEvent` per block; adapters subscribe.

```
              ┌──────────────────────┐
              │  Block Subscriber    │
              │  (one per chain)     │
              └──────────┬───────────┘
                         │  publish: BlockEvent
                         ▼
                ┌────────────────┐
                │    BROKER      │  topics: blocks.{chain}, snapshots.{venue}.{pair},
                │    (NATS)      │           opportunities
                └─┬──────────┬──┬┘
       subscribe  │          │  │
                  ▼          ▼  ▼
        ┌──────────┐  ┌──────────┐  ┌──────────┐
        │ Binance  │  │ Coinbase │  │ Uniswap  │  ... N venue adapters
        │ adapter  │  │ adapter  │  │ adapter  │      each horizontally scalable
        └────┬─────┘  └────┬─────┘  └────┬─────┘
             │             │             │
              \            │            /
               \           │           /     publish: VenuePrices (unified shape)
                \          │          /
                 ▼         ▼         ▼
            ┌────────────────────────────┐
            │       BROKER (NATS)        │
            └─────────────┬──────────────┘
                          │  subscribe: VenuePrices (effective-price list per block)
                          ▼
                ┌──────────────────────┐
                │  Pairing / Detector  │   stateless, horizontally scalable
                │  service             │
                └──────────┬───────────┘
                           │  publish: Opportunity
                           ▼
                  ┌──────────────────┐
                  │ Sinks: log, DB,  │
                  │ Slack, Prom, ... │
                  └──────────────────┘
```

**Why NATS** for this workload specifically:

- Sub-millisecond latency keeps the block-driven cadence tight
- Native pub/sub with at-least-once delivery is enough — we don't need long-term log replay
- Simple operational footprint compared to Kafka
- Subject-based hierarchies (`blocks.eth`, `snapshots.binance.ETH-USDC`) map naturally to the routing we need

For systems that *do* need replay or audit (regulatory backfill, time-travel debugging), **Kafka** would replace NATS at the cost of higher operational overhead.

### Horizontal scaling per adapter

Each adapter becomes its own service, scaled independently:

- The Binance adapter can run with 5 replicas; rate-limit budget is shared via a central token-bucket service.
- The Uniswap adapter can run with 10 replicas across multiple RPC providers (Infura, Alchemy, self-hosted) for redundancy.
- A new venue is a new deployment, not a code change in the core.

Sharding strategy: each adapter instance is assigned a subset of pairs (consistent hashing on pair ID). Block ticks are broadcast to all instances; each one only acts on its assigned pairs.

### Pathfinder and Evaluator as stateless consumers

The pairing-and-detection service subscribes to per-venue effective-price events and matches them by `(block_number, pair)`. When all expected venues for a block report in (or a timeout fires), the Pathfinder enumerates candidate paths across all reporting venues (this is where multi-venue, order-splitting, and multi-hop routing logic naturally grows), and the Profitability Evaluator publishes `Opportunity` events for those that clear the threshold.

Because both stages are stateless (their only inputs are the messages on the broker), they can be horizontally scaled with no coordination beyond the broker's consumer-group semantics.

### State and history store

Two persistent stores would be added:

- **Operational state** (PostgreSQL or Redis): last-processed block per chain, current circuit-breaker states, rate-limit counters. Survives restarts.
- **Detection history** (time-series DB such as ClickHouse, or append-only Postgres): every detected opportunity, every snapshot, every rejected near-miss. Enables analytics ("what's our detection rate by hour?", "which venues consistently provide the best DEX side?") and supports backtesting.

### Observability stack

Production needs:

- **Metrics**: Prometheus / OpenTelemetry for RPC latency p50/p99, adapter error rates, opportunities per block, circuit-breaker trips
- **Tracing**: distributed traces across the broker so a single block tick can be followed end-to-end
- **Alerting**: PagerDuty on circuit breaker open, sustained no-data, or RPC provider outage
- **Dashboards**: Grafana views of detection rate, profit distribution, latency budgets

### Other production concerns

A non-exhaustive list of additional concerns the single-process detector does not address:

- **Chain reorg handling**: confirmation-depth waiting, explicit reorg events, opportunity invalidation
- **Multi-chain support**: the same detector logic across Ethereum, Base, Arbitrum, etc., with a block-subscriber service per chain
- **Cross-pool routing**: querying all Uniswap V3 fee tiers per block (and Curve, Balancer, etc.) and picking the best
- **Authorization for trading**: KMS / HSM integration for signing keys, hot-key budgets, withdraw rate limits
- **Position management**: tracking inventory across venues, balance reservation before order submission, partial-fill reconciliation
- **Cost attribution**: per-RPC-provider cost tracking, gas-spent accounting
- **Compliance and audit**: immutable logs of every decision and every emitted opportunity, regulatory reporting

None of these belong in a detection-only 4–10 hour exercise. All of them would be required for the system Terrace itself operates.
