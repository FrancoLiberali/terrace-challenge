# Business Context <!-- omit from toc -->

## Table of Contents <!-- omit from toc -->

- [What this system does](#what-this-system-does)
- [What is arbitrage?](#what-is-arbitrage)
- [Why two venues exist for the same asset](#why-two-venues-exist-for-the-same-asset)
  - [Venue A — Centralized Exchange (Binance)](#venue-a--centralized-exchange-binance)
  - [Venue B — Decentralized Exchange (Uniswap V3)](#venue-b--decentralized-exchange-uniswap-v3)
  - [Side-by-side](#side-by-side)
- [Why the two prices diverge](#why-the-two-prices-diverge)
  - [1. There is no global "true" price](#1-there-is-no-global-true-price)
  - [2. Information propagates at different speeds](#2-information-propagates-at-different-speeds)
  - [3. Different participants, different flows](#3-different-participants-different-flows)
  - [4. Friction prevents instant convergence](#4-friction-prevents-instant-convergence)
- [The role of arbitrageurs](#the-role-of-arbitrageurs)
- [Why the Ethereum block is the system clock](#why-the-ethereum-block-is-the-system-clock)
- [What gets detected, step by step](#what-gets-detected-step-by-step)
- [How this maps to Terrace's business](#how-this-maps-to-terraces-business)

---

## What this system does

This service detects **arbitrage opportunities** between a centralized exchange (CEX) — Binance — and a decentralized exchange (DEX) — Uniswap V3 — for the ETH-USDC trading pair.

It is **detection-only**. It does not execute trades. Its single responsibility is:

> Every Ethereum block (~12 seconds), take a synchronized snapshot of both venues, calculate effective execution prices for configurable trade sizes, and emit a structured alert when the spread (after fees, slippage, and gas) is large enough to be profitable.

---

## What is arbitrage?

Arbitrage is the practice of profiting from the same asset trading at two different prices on two different venues. It is one of the oldest activities in finance.

A non-crypto analogy: imagine gold trades at $2,200/oz at a jeweler downtown and $2,250/oz at a pawn shop across the river. If a trader can buy at the jeweler, transport the gold across the bridge (paying a small toll), and sell at the pawn shop, they capture the $50 spread minus transport cost. Their activity also gradually closes the gap, because their buying lifts the jeweler's price and their selling depresses the pawn shop's price.

In crypto the "two venues" are Binance and Uniswap, the "bridge toll" is Ethereum gas, and the "transport" is moving capital across the boundary between centralized and decentralized infrastructure.

---

## Why two venues exist for the same asset

Crypto has two structurally different ways of running a marketplace, and they coexist. Each discovers its own price.

### Venue A — Centralized Exchange (Binance)

Binance is a company that runs a classical **orderbook** matching engine, identical in principle to the NYSE. Users deposit funds with Binance; orders are matched in microseconds against a stack of bids and asks.

```
ASKS (sellers)
$2,251 ──── 5 ETH for sale
$2,250 ──── 12 ETH for sale   ← lowest seller
─────────── (the spread)
$2,249 ──── 8 ETH wanted      ← highest buyer
$2,248 ──── 20 ETH wanted
BIDS (buyers)
```

A market order eats levels of the book from top to bottom. Execution is fast, custody is centralized (Binance holds user funds), and the price is whatever the orderbook says at that millisecond.

### Venue B — Decentralized Exchange (Uniswap V3)

Uniswap is not a company. It is a smart contract deployed on the Ethereum blockchain. There is no orderbook. Instead, it uses an **Automated Market Maker (AMM)** model.

A pool contains two assets in some ratio — for example, 5,000 ETH and 11.25M USDC. Anyone can swap one for the other by depositing into the pool and withdrawing the counter-asset, following a fixed mathematical formula. The "price" is implied by the ratio of reserves and shifts continuously with every swap. Bigger trades drain the pool more aggressively and therefore receive worse prices: this is **slippage**.

Uniswap is slower (state only changes when an Ethereum block is mined, every ~12 seconds), decentralized (no operator), and non-custodial (funds live in the user's wallet until the moment of the swap).

### Side-by-side

|                     | CEX (Binance)                          | DEX (Uniswap V3)                          |
|---------------------|----------------------------------------|-------------------------------------------|
| Price discovery     | Orderbook of bids and asks             | AMM formula over pool reserves            |
| Update frequency    | Milliseconds                           | Once per Ethereum block (~12 seconds)     |
| Custody             | Exchange holds user funds              | User's wallet, contract-mediated          |
| Access              | Subject to KYC, region restrictions    | Anyone with a wallet, anywhere            |
| Trust model         | Trust the operator                     | Trust the deployed code                   |

---

## Why the two prices diverge

The two venues do not share a price oracle and do not coordinate. They are independent price-discovery systems running in parallel. Several forces continuously push their prices apart:

### 1. There is no global "true" price

Each venue computes its own price from its own activity. Two independent processes will inevitably diverge.

### 2. Information propagates at different speeds

A market-moving event hits Binance traders first — they refresh in milliseconds and immediately adjust their orders. Uniswap's price only updates when someone executes a swap against the pool, which requires waiting for an Ethereum block. During those seconds of lag, the two venues disagree.

### 3. Different participants, different flows

A whale dumping ETH on Binance pushes Binance's price down. A DeFi protocol auto-rebalancing on Uniswap pushes Uniswap's price up. These flows are independent and frequently move the two venues in opposite directions.

### 4. Friction prevents instant convergence

Closing a price gap requires moving capital between venues, which costs time and gas. Small gaps stay open because they are not worth the friction to close. The market only converges once the gap exceeds the cost to capture it — and that residual gap is precisely what arbitrageurs target.

---

## The role of arbitrageurs

Arbitrageurs are the mechanism that keeps prices roughly aligned across disconnected markets. When they detect a gap and execute on it, their buying lifts the cheap venue and their selling depresses the expensive venue. The very act of capturing the spread closes it.

This service is a **detector**, not a trader. It identifies the moments when such gaps exist and emits structured data about them. In a production trading version, that data would feed an execution pipeline; here it is an end in itself.

---

## Why the Ethereum block is the system clock

The DEX side has a hard physical constraint: its state only changes when an Ethereum block is mined. Between blocks, Uniswap's pool reserves are frozen — querying the pool at any moment during the 12-second window returns the same answer.

A meaningful arbitrage opportunity exists only when both legs can be evaluated at the same instant. Since the DEX is the slower leg, the **block boundary is the only point in time where the DEX state is fresh and a paired observation is well-defined**. Both venues are therefore sampled together, on every new block.

```
Block 100 mined                      Block 101 mined
T = 0s                               T = 12s
   │                                    │
   ▼                                    ▼
┌──────────────────────────────────┐ ┌──────────────────────────────────┐
│ Snapshot Binance @ T=0           │ │ Snapshot Binance @ T=12          │
│ Quote Uniswap @ block 100 state  │ │ Quote Uniswap @ block 101 state  │
│ Compare → emit alert if profitable│ │ Compare → emit alert if profitable│
└──────────────────────────────────┘ └──────────────────────────────────┘
```

This synchronized-snapshot model gives every detection a clean, auditable timestamp ("at block N, the world looked like X") and avoids the noisy stream of duplicate alerts that would arise from comparing fresh Binance data against a stale Uniswap snapshot.

---

## What gets detected, step by step

For each block:

1. Snapshot the Binance ETH-USDC orderbook (top N levels via the public REST endpoint).
2. For each configured trade size (e.g., 1, 10, 100 ETH), walk the book to compute the **effective execution price** including slippage.
3. Query Uniswap V3's QuoterV2 contract to simulate the same trade against the pool at the current block's state, again obtaining an effective price (already inclusive of pool fee and AMM slippage).
4. Estimate gas cost for the DEX leg using current network conditions.
5. For each direction (CEX→DEX and DEX→CEX) and each trade size, compute net profit:

   ```
   net_profit = (sell_side_price - buy_side_price) × size
                - cex_trading_fee
                - dex_pool_fee   (already in the quote)
                - estimated_gas_cost
   ```

6. If `net_profit > 0`, emit a structured alert describing the direction, trade size, prices on each side, the spread, the estimated profit, and the execution steps.

The output is informational. The value of the service is in cleanly observing and reasoning about these events, not in capitalizing on them.

---

## How this maps to Terrace's business

This challenge is a miniature of the problem Terrace solves at industrial scale. Their Smart Order Router watches the same kind of price discrepancies across dozens of CEXs and DEXs simultaneously, fragmenting and routing large institutional orders to maximize the effective price obtained. The detection logic here — synchronized snapshots, slippage-aware effective pricing, atomic decision points per block — is the conceptual core of any such system.
