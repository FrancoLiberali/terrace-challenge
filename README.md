# terrace-challenge

[![CI](https://github.com/FrancoLiberali/terrace-challenge/actions/workflows/ci.yml/badge.svg)](https://github.com/FrancoLiberali/terrace-challenge/actions/workflows/ci.yml)
[![Quality Gate Status](https://sonarcloud.io/api/project_badges/measure?project=FrancoLiberali_terrace-challenge&metric=alert_status)](https://sonarcloud.io/summary/new_code?id=FrancoLiberali_terrace-challenge)
[![Coverage](https://sonarcloud.io/api/project_badges/measure?project=FrancoLiberali_terrace-challenge&metric=coverage)](https://sonarcloud.io/summary/new_code?id=FrancoLiberali_terrace-challenge)

Real-time CEX–DEX arbitrage detection between Binance and Uniswap V3 for the ETH-USDC pair, sampled on every Ethereum block. Built in Go as part of the Terrace Senior Software Engineer coding challenge.

The bot subscribes to Ethereum's `newHeads` stream, fetches a fee-adjusted Binance orderbook snapshot and a `QuoterV2` simulation from Uniswap V3 in parallel for each block, pairs the results, applies a cost model (trading fees + gas), and emits a structured alert whenever the net profit clears a configurable threshold.

## Quickstart

Five minutes from clone to first block evaluated.

**Prerequisites:**
- Go 1.25+ (`go version`)
- A free Alchemy / Infura key for Ethereum Mainnet — used for the HTTP RPC and the WebSocket `newHeads` subscription. Both URLs come from the same provider app.

**Steps:**

```bash
git clone https://github.com/FrancoLiberali/terrace-challenge.git
cd terrace-challenge

cp .env.example .env
# Edit .env and fill in your Alchemy HTTPS + WSS URLs.

make run
```

That's it. With `PRETTY_ALERTS=true` (the default in `.env.example`) you'll see a banner on stdout and structured slog records on stderr. Opportunities are emitted as both a structured slog event and a multi-line human-readable block.

The standalone diagnostic probes from earlier steps are also available:

```bash
make probe-binance   # walk the Binance orderbook for the configured sizes
make probe-uniswap   # one QuoterV2 eth_call per (size, side)
make probe-chain     # subscribe to newHeads and print one line per block
```

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

## Documentation

- [**CHALLENGE.md**](./CHALLENGE.md) — the original challenge specification (converted from the provided PDF) describing requirements, deliverables, and evaluation criteria.
- [**business.md**](./business.md) — business context: what the system detects, why CEX and DEX prices diverge, and why the Ethereum block is the natural clock for the design.
- [**architecture.md**](./architecture.md) — conceptual architecture: components, data flow, design decisions, trade-offs, and what a production-scale version would look like.
- [**implementation.md**](./implementation.md) — Go-level structure: package layout, interface seams in code, and the per-host resilience composition pattern.
- [**plan.md**](./plan.md) — step-by-step implementation plan: integration-first, types-on-demand, with verification per step.
- [**limitations.md**](./limitations.md) — explicit list of known limitations, risks, and missed opportunities of the simplified detection-only design, plus what a production trading extension would require.
