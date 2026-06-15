# terrace-challenge

[![CI](https://github.com/FrancoLiberali/terrace-challenge/actions/workflows/ci.yml/badge.svg)](https://github.com/FrancoLiberali/terrace-challenge/actions/workflows/ci.yml)
[![Quality Gate Status](https://sonarcloud.io/api/project_badges/measure?project=FrancoLiberali_terrace-challenge&metric=alert_status)](https://sonarcloud.io/summary/new_code?id=FrancoLiberali_terrace-challenge)
[![Coverage](https://sonarcloud.io/api/project_badges/measure?project=FrancoLiberali_terrace-challenge&metric=coverage)](https://sonarcloud.io/summary/new_code?id=FrancoLiberali_terrace-challenge)

Real-time CEX–DEX arbitrage detection between Binance and Uniswap V3 for the ETH-USDC pair, sampled on every Ethereum block. Built in Go as part of the Terrace Senior Software Engineer coding challenge.

The bot subscribes to Ethereum's `newHeads` stream, fetches a fee-adjusted Binance orderbook snapshot and a `QuoterV2` simulation from Uniswap V3 in parallel for each block, pairs the results, applies a cost model (trading fees + gas), and emits a structured alert whenever the net profit clears a configurable threshold.

## Documentation

The order below is intentional — it mirrors how I approached the challenge, and reading them in sequence is the fastest way to see not just *what* I built but *why* every decision is the way it is. The exercise isn't only "produce a working detector"; it's also a chance to show how I work, and that starts with how the problem was framed, bounded, and broken down before any code was written.

1. [**CHALLENGE.md**](./CHALLENGE.md) — **the brief I was given**, transcribed from the provided PDF. Everything that follows is my response to it; reading the brief first makes the choices in the next documents legible.
2. [**business.md**](./business.md) — **the why and the context.** Before writing a line of code I wanted to be confident I understood what the system is *for*: what an arbitrage opportunity actually is, why CEX and DEX prices diverge in the first place, and why the Ethereum block is the natural clock for the design. Implementation choices that don't trace back to a business reason are guesses; this document is the grounding everything else builds on.
3. [**limitations.md**](./limitations.md) — **what we're deliberately not modelling, and why.** The limitations fall out of the business analysis above: working through what really happens on a CEX–DEX arb makes the simplifications we're accepting visible. Writing them down explicitly turns them into a backlog — the gap between the detection challenge we're solving and a production trading system, enumerated rather than hidden. Useful as a "what changes when we move to prod" punch list.
4. [**architecture.md**](./architecture.md) — **the design that satisfies the business and respects the limitations.** Components, data flow, design decisions with their alternatives-considered, and an explicit "what a production-scale version would add" section that ties back to the same backlog from `limitations.md`.
5. [**plan.md**](./plan.md) — **how the work was sequenced.** The challenge is divided into seven phases, each independently runnable and verifiable end-to-end against real venues. Integration-first (probes against real APIs before any composition), types-on-demand (abstractions emerge from the third concrete usage, not the first), value delivered each step. Reflects the agile-style "make it work small, then make it grow" approach.
6. [**engineering.md**](./engineering.md) — **the discipline the implementation will be held to.** Testing, code review through PRs, CI, quality gates, security, reproducibility, incremental delivery. Sits before the implementation walkthrough because these practices are what shape what the implementation looks like — the test harness, the lint config, the SonarCloud gate are all in place before any code is written that has to pass through them.
7. [**implementation.md**](./implementation.md) — **Go-level structure, last.** Package layout, interface seams, and the per-host resilience composition pattern. Last because by this point every Go-specific choice should be a mechanical translation of the architecture and plan above, complying with the engineering discipline of #6 — interesting if you want to see *how* it lands in code, but not where the design lives.

## What it looks like

Real output captured from arbd running against mainnet — included so you can see the bot working without running it yourself.

### Healthy run

```
time=2026-06-15T18:30:12.401-03:00 level=INFO msg="arbd starting" venues="[binance uniswap]" pair=ETH-USDC uniswap_pool_fee=3000 threshold_usdc=1
arbd: detecting CEX↔DEX arbitrage on ETH-USDC (binance + uniswap v3 fee=3000)
      threshold: net profit > $1 USDC — Ctrl+C to stop

time=2026-06-15T18:30:30.118-03:00 level=INFO msg="evaluation finished" total_candidates=12 profitable=0
```

Steady state on mainnet today: CEX–DEX spreads on ETH-USDC don't clear the trading fees + gas budget, so the threshold filter correctly suppresses every candidate. Setting `PRETTY_ALERTS=false` swaps both the banner and the structured logs to JSON for log-aggregator ingestion.

### Sample alert

To show the emission format, the threshold was temporarily lowered so the filter wouldn't suppress non-profitable candidates. Both the structured slog event (`stderr`) and the multi-line block (`stdout`) fire per qualifying opportunity:

```
time=2026-06-15T18:34:13.932-03:00 level=INFO msg="arbitrage opportunity detected" block=25325623 timestamp=2026-06-15T21:34:11Z direction=binance→uniswap buy_venue=binance sell_venue=uniswap size_eth=1 buy_price_usdc=1818.1764 sell_price_usdc=1812.7702 spread_per_unit=-5.4061 gross_profit_usdc=-5.41 gas_cost_usdc=0.0249 gas_estimate=98690 net_profit_usdc=-5.43 net_profit_pct=-0.2987 capital_usdc=1818.18 uniswap_pool=0x8ad599c3A0ff1De082011EFDDc58f1908eb6e6D8
```

```
=== ARBITRAGE OPPORTUNITY DETECTED ===
Block Number: 25325623
Timestamp:    2026-06-15 21:34:11 UTC
Direction:    binance → uniswap  (Buy on binance, Sell on uniswap)

Trade Size:        1 ETH
Buy  Price:        $1818.1764 / ETH (effective, slippage-aware) — binance
Sell Price:        $1812.7702 / ETH (effective, slippage-aware) — uniswap
Spread per unit:   $-5.4061 / ETH (-0.2973%)

Profit (post-fee): $-5.41  (already net of venue-intrinsic fees, gross of gas)
Gas Cost (est):    $0.0249  (baseFee=0.138 gwei, ~98690 gas)
Net Profit:        $-5.43  (-0.2987%)
Capital Required:  $1818.18 USDC

Execution Steps:
  1. Buy 1 ETH on binance at ~$1818.18/ETH
     - Required capital: ~$1818.18 USDC
  2. Transfer ETH from buy venue to sell venue (operationally — bridging, transfer time, etc.)
  3. Execute Uniswap V3 swap: 1 ETH → USDC
     - Pool: 0x8ad599c3A0ff1De082011EFDDc58f1908eb6e6D8
     - Expected output: ~$1812.77 USDC
Risk factors: see limitations.md (intra-block drift, MEV on the DEX leg, gas-price spikes)
────────────────────────────────────────────────────────────
```

The Uniswap pool address shown in step 3 is derived deterministically from `(factory, WETH, USDC, fee_tier)` via CREATE2 at startup — no extra RPC call. Switching `UNISWAP_POOL_FEE` in `.env` automatically points the alert at the correct pool for the new tier.

Net profit comes out negative because the captured spread didn't actually clear the fee + gas budget — exactly the case the default `$1` threshold filters out in production. With the threshold restored, this block doesn't fire; the bot only emits when a real profitable opportunity is found.

## Quickstart

Five minutes from clone to first block evaluated. Pick the path that matches what's installed locally.

**Prerequisites (both paths):** a free Alchemy / Infura key for Ethereum Mainnet — used for the HTTP RPC and the WebSocket `newHeads` subscription. Both URLs come from the same provider app.

```bash
git clone https://github.com/FrancoLiberali/terrace-challenge.git
cd terrace-challenge

cp .env.example .env
# Edit .env and fill in your Alchemy HTTPS + WSS URLs.
```

Both paths below run arbd in the foreground until you stop it — **Ctrl+C** in an interactive shell, or wrap with `timeout` for a bounded sanity check (`timeout 30 make run`, `timeout 30 make docker-run`). SIGTERM propagates cleanly through the make → (docker →) arbd chain.

### Path A — Docker (requires Docker only)

```bash
make docker-build
make docker-run
```

`docker-build` builds a multi-stage image (Go for build, distroless for runtime, ~18MB total). `docker-run` invokes `docker run --rm --env-file .env terrace-challenge` — credentials and runtime mode flow in from your local `.env`; no secrets are baked into the image. `config.yaml` is included; override it by mounting one at `/app/config.yaml`.

### Path B — native Go (requires Go 1.25+)

```bash
make run
```

The standalone diagnostic probes are also available:

```bash
make probe-binance   # walk the Binance orderbook for the configured sizes
make probe-uniswap   # one QuoterV2 eth_call per (size, side)
make probe-chain     # subscribe to newHeads and print one line per block
```

---

With `PRETTY_ALERTS=true` (the default in `.env.example`) you'll see a banner on stdout and structured slog records on stderr. Opportunities are emitted as both a structured slog event and a multi-line human-readable block.

## Configuration

The split: **`.env` for environment bindings, `config.yaml` for behavior.**

### `.env` — environment bindings + runtime mode

Credentials, URLs, and the runtime toggles. Gitignored; copy from `.env.example`.

| Variable | Purpose |
|---|---|
| `ETH_RPC_URL` | Ethereum HTTPS RPC endpoint. Used for every on-chain `eth_call` (today only QuoterV2, but reusable by any future on-chain reader). |
| `ETH_RPC_WS_URL` | Ethereum WebSocket endpoint for the `newHeads` block subscription. |
| `BINANCE_BASE_URL` | Where Binance lives. Defaults to `https://api.binance.com`; switch to `https://api.binance.us` or a testnet URL as needed. |
| `UNISWAP_POOL_FEE` | Which Uniswap V3 pool to query for the ETH-USDC pair: `500`=0.05%, `3000`=0.3%, `10000`=1%. |
| `UNISWAP_QUOTER_ADDRESS` | QuoterV2 contract address. Mainnet canonical: `0x61fFE014bA17989E743c5F6cB21bF9697530B21e`. |
| `LOG_LEVEL` | Slog level for arbd's internal logs. `DEBUG` / `INFO` / `WARN` / `ERROR`. The arbitrage-alert event itself is emitted independently and is NOT gated by this. |
| `PRETTY_ALERTS` | When `true`, writes the multi-line opportunity block to stdout AND uses slog's TextHandler (key=value, terminal-friendly). When `false`, JSONHandler (log-aggregator-friendly), no pretty block. Off in production. |
| `CONFIG_FILE` (optional) | Path to the YAML config. Defaults to `./config.yaml`. |

### `config.yaml` — application tunables

Committed to the repo (no secrets); change in place and re-run.

- **Trade sizes** to snapshot per block (default `[1, 10, 100]` ETH).
- **Profitability threshold** in USDC.
- **Per-venue knobs**: rate limit (RPS / burst), circuit breaker (min-requests / failure-ratio / cooldown / interval), per-request timeout, Binance taker fee.
- **HTTP retry policy** (max retries, initial/max backoff).
- **Dispatcher** per-call timeout.
- **Subscriber** reconnect-backoff bounds.

Defaults are tuned conservatively against documented free-tier limits (Alchemy 330 CU/s, Binance 1200 weight/min). See `config.yaml` for inline notes per field.

## Make targets

```bash
make help            # list all targets
make test            # go test -race ./...
make lint            # golangci-lint run ./...
make build           # build all binaries into ./bin/
make run             # go run ./cmd/arbd
make probe-binance   # diagnostic: walk Binance depth
make probe-uniswap   # diagnostic: QuoterV2 simulation
make probe-chain     # diagnostic: newHeads stream
make tidy            # go mod tidy
```

