# Implementation Plan <!-- omit from toc -->

This document captures the step-by-step plan for implementing the system described in [`architecture.md`](./architecture.md). It is an integration-first, types-on-demand plan: each step delivers a standalone runnable program that proves one external boundary works, and shared structures emerge later as composition reveals the need for them.

For the conceptual design see [`architecture.md`](./architecture.md); for the package layout and code-level conventions see [`implementation.md`](./implementation.md).

---

## Table of Contents <!-- omit from toc -->

- [Approach](#approach)
- [Steps](#steps)
  - [Step 1 — Probe Binance](#step-1--probe-binance)
  - [Step 2 — Probe Uniswap V3](#step-2--probe-uniswap-v3)
  - [Step 3 — Probe Chain](#step-3--probe-chain)
  - [Step 4 — Compose](#step-4--compose)
  - [Step 5 — Detect](#step-5--detect)
  - [Step 6 — Harden](#step-6--harden)
  - [Step 7 — Configure and polish](#step-7--configure-and-polish)
- [Why this order](#why-this-order)
- [Logistical notes](#logistical-notes)

---

## Approach

The plan is deliberately **integration-first**: the first three steps validate each external boundary (Binance, Uniswap V3, Ethereum block stream) in isolation, against real services, using the simplest possible output (printing to stdout). Only after each integration is independently verified does composition begin.

Shared types (`VenuePrice`, `CandidatePath`, `Opportunity`, etc.) and shared interfaces are **not laid down up front**. They are introduced incrementally, when composition forces them to exist. This avoids designing data shapes in the abstract and revising them once real integration constraints appear.

Each step delivers a **standalone runnable program** under `cmd/`. The early probes (`cmd/probe-binance/`, `cmd/probe-uniswap/`, `cmd/probe-chain/`) are not throwaway scaffolding — they stay in the repo as ongoing diagnostic tools so any one boundary can be re-verified independently at any time.

Tests are written alongside each step, not after. Pure logic (orderbook walking, ABI normalization, reconnect state machine) is unit-tested with hand-built fixtures and mocks. End-to-end validation against real services is part of every probe step.

---

## Steps

### Step 1 — Probe Binance

**Goal**: Confirm Binance REST integration works end-to-end and that effective-price computation is correct.

**Build**:

- `cmd/probe-binance/main.go` — fetches the ETH-USDC orderbook from `/api/v3/depth`, walks the book to compute slippage-aware effective prices for a hard-coded set of trade sizes (e.g., 1, 10, 100 ETH), prints results.

**Verify**:

- Run it. Compare printed best bid/ask against Binance's website.
- Unit-test the orderbook walk with a hand-built fixture — assert effective price matches by-hand calculation for several sizes including a size larger than the book depth.

**Initial output**: `fmt.Printf` lines with raw best bid/ask and computed effective price per configured size.

**No external credentials required.**

---

### Step 2 — Probe Uniswap V3

**Goal**: Confirm `eth_call` integration with the QuoterV2 contract works end-to-end and amount-out decoding is correct.

**Build**:

- `cmd/probe-uniswap/main.go` — connects to the configured RPC endpoint, calls QuoterV2's `quoteExactInputSingle` for ETH-USDC at the same configured trade sizes, converts the raw `amountOut` to a per-unit price in USDC/ETH, prints.

**Verify**:

- Run against mainnet. Sanity-check the quoted price against any public DEX aggregator UI.
- Unit-test ABI packing/unpacking and the decimal normalization (raw 6-decimal USDC → human-readable, raw 18-decimal WETH → human-readable) with hand-computed expected values.

**Initial output**: `fmt.Printf` lines with raw and converted quotes per size.

**Requires**: Alchemy or Infura API key (free tier is enough).

---

### Step 3 — Probe Chain

**Goal**: Confirm the WebSocket block subscription works end-to-end, including reconnect behaviour.

**Build**:

- `cmd/probe-chain/main.go` — opens a WebSocket connection to the Ethereum node, subscribes to `newHeads`, prints each block as it arrives. Implements reconnect with exponential backoff and last-block tracking so a manual disconnect (e.g., toggling Wi-Fi) recovers cleanly.

**Verify**:

- Run for a few minutes. Observe ~12s cadence of block events. Manually disconnect/reconnect the network to exercise the reconnect path.
- Unit-test the reconnect state machine using an in-memory fake WebSocket: simulate drops, simulate gaps, assert the resume logic uses the last known block.

**Initial output**: `fmt.Printf` lines per block: number, timestamp, gas price.

**Requires**: same RPC key as Step 2 (WebSocket endpoint).

---

### Step 4 — Compose

**Goal**: Wire the chain probe to the two venue probes so that every new block triggers a paired Binance + Uniswap fetch and a printed comparison. This is where shared types start to emerge.

**Build**:

- A single program (likely the start of `cmd/arbd/main.go`) that, on each new block event, dispatches a Binance fetch and a Uniswap fetch in parallel and prints a side-by-side comparison.
- **Shared types are introduced here, only as composition requires them**: a `VenuePrice` shape (or equivalent) for the unified effective-price entries, a `Pair` value object, a paired-prices structure for the per-block result.

**Verify**:

- Run against mainnet. Watch the comparison log update every ~12s.
- Unit-test the per-block pairing logic with mock adapters that return canned data.

**Initial output**: Per block, a `fmt.Printf` line summarising "CEX = X, DEX = Y, spread = Z" for each configured size and direction.

---

### Step 5 — Detect

**Goal**: Turn the paired comparison into structured arbitrage detection.

**Build**:

- Extract the candidate-pairing logic into a Pathfinder component (likely `internal/pathfinder/`).
- Introduce the Profitability Evaluator (likely `internal/arbitrage/`) which applies the CEX trading fee and a gas estimate and emits opportunities above a configurable threshold.
- Format alerts to match the example in [`CHALLENGE.md`](./CHALLENGE.md).

**Verify**:

- Unit-test Pathfinder with synthetic paired-prices inputs.
- Unit-test the Evaluator with hand-crafted candidate paths and a fixed cost model.
- Run against mainnet. Observe the pipeline emit (probably zero) opportunities; lower the threshold temporarily to confirm the output format triggers correctly.

**Output**: Structured alert per detected opportunity, in the format specified by the challenge.

---

### Step 6 — Harden

**Goal**: Add production-readiness wrappers without changing core behaviour.

**Build**:

- Resilience wrappers around each adapter (rate limit, circuit breaker) following the decorator pattern documented in [`implementation.md`](./implementation.md#resilience-composition-pattern).
- Promote adapter contracts to explicit interfaces if not already done in Step 4.
- Add structured logging (`slog` or equivalent) throughout.

**Verify**:

- Unit-test wrappers with fake inner adapters that simulate timeouts and failures.
- Smoke test the full pipeline; manually trigger a circuit-breaker open by pointing at an unreachable host briefly.

**Output**: Same as Step 5, plus structured error logs when resilience wrappers trip.

---

### Step 7 — Configure and polish

**Goal**: Move all tunables out of code and into configuration; finalise the runnable.

**Build**:

- `config.yaml` (loaded via Viper or equivalent) carrying: RPC URL(s), CEX endpoint, fee rates, trade sizes, gas estimate or gas oracle URL, profitability threshold, output format.
- Environment-variable overrides for credentials.
- Output sink as an injectable interface; default = structured stdout. Document the seam for future extension (Slack, metrics, etc.).
- `Makefile` (or `Taskfile`) with `make test`, `make run`, `make probe-binance`, `make probe-uniswap`, `make probe-chain`.
- Final README pass with setup, configuration, and run instructions.

**Verify**:

- Smoke test from a clean config. Assert no hard-coded URLs or magic numbers remain.
- All tests still pass.

**Output**: A repo that a reviewer can `git clone`, configure, and run in under five minutes.

---

## Why this order

External-integration risks are concentrated at the start. The riskiest unknowns — does the Binance API respond as expected? Does QuoterV2 return the values we think? Does the WebSocket reconnect cleanly? — are settled in steps 1–3, against real services. Everything after that is composition and refinement against integrations that are already known to work.

Types and abstractions appear only when composition forces them. Defining a `VenuePrice` shape in step 1 would be guessing; defining it in step 4 means it captures the real constraints both sides emit. The same applies to the Pathfinder, Evaluator, and resilience wrappers — each is extracted from working concrete code, not designed in the abstract.

The probe binaries remain useful permanently. `go run ./cmd/probe-binance` should keep working at the end of step 7 and serve as a quick ground-truth check for any future change to the Binance adapter.

---

## Logistical notes

- **Credentials**: only Steps 2, 3, and beyond require an RPC key (Alchemy or Infura free tier). Step 1 (Binance) needs nothing.
- **Mainnet only**: the system queries mainnet contracts and Binance's mainnet API. No testnet variant is required by the challenge.
- **Steps 1–3 are independent**: any order works, but Step 1 (Binance) is the simplest and is recommended as the warmup.
- **Time budget**: roughly 8–10 hours total for all seven steps, with Steps 1–4 forming a clean stopping point at roughly the half-way mark.
