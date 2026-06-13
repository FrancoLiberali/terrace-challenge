# Known Limitations and Risks <!-- omit from toc -->

This document is intentionally explicit about what the simplified design **does not** model, what opportunities it misses, and which production-grade concerns are deferred.

The scope of the service is **detection only**. Several limitations below would be material if the service were extended to execute trades; here they affect the interpretation of alerts but not the correctness of the detection itself.

---

## Table of Contents <!-- omit from toc -->

- [1. Block-boundary sampling misses intra-block opportunities](#1-block-boundary-sampling-misses-intra-block-opportunities)
- [2. Detected profit is an upper bound, not the realized P\&L](#2-detected-profit-is-an-upper-bound-not-the-realized-pl)
- [3. The bot does not account for execution risk on the DEX side](#3-the-bot-does-not-account-for-execution-risk-on-the-dex-side)
- [4. The public mempool exposes strategy](#4-the-public-mempool-exposes-strategy)
- [5. Execution risk is asymmetric between the two venues](#5-execution-risk-is-asymmetric-between-the-two-venues)
- [6. Chain reorganizations are not handled](#6-chain-reorganizations-are-not-handled)
- [7. The detector does not model probability of inclusion or gas auctions](#7-the-detector-does-not-model-probability-of-inclusion-or-gas-auctions)
- [8. Single-pool, single-fee-tier simplification](#8-single-pool-single-fee-tier-simplification)
- [9. Single CEX, single DEX, single pair](#9-single-cex-single-dex-single-pair)
- [10. The detector assumes liquidity is available at the moment of observation](#10-the-detector-assumes-liquidity-is-available-at-the-moment-of-observation)
- [What a production trading version would add](#what-a-production-trading-version-would-add)

---

## 1. Block-boundary sampling misses intra-block opportunities

The service samples both venues exactly once per Ethereum block. Between blocks, the Uniswap pool state is frozen — there is nothing new to observe on the DEX side. But Binance updates continuously, which means a spread can appear and disappear entirely within a 12-second window without the detector noticing.

```
T = 0s    Snapshot taken.       No arb.
T = 5s    Binance price moves.  $15/ETH spread opens.
T = 10s   Binance moves back.   Spread closes.
T = 12s   Snapshot taken.       No arb. Alert never fires.

→ A real five-second opportunity was invisible to the detector.
```

This is a deliberate simplification:

- **Atomicity**: pairing both snapshots at the same instant produces clean, auditable alerts ("at block N, the world looked like X"). A continuous Binance feed compared against a stale Uniswap snapshot would produce duplicate or stale alerts.
- **Match with execution reality**: a real trading bot would still need to wait for the next block to execute the DEX leg, so intra-block opportunities also carry the highest execution risk (see §3 and §5).

A production version would likely add a continuous Binance side-channel with debouncing, and only escalate to an alert when the intra-block spread is materially larger than typical block-boundary spreads (compensating the higher execution risk with a higher expected return).

---

## 2. Detected profit is an upper bound, not the realized P&L

The bot reports `(price difference × size) - fees - gas`. This number assumes the service could execute both legs **instantly and alone** at the prices it observed. In reality, several factors erode the realized profit:

| Factor                                  | Effect on realized profit |
|----------------------------------------|---------------------------|
| Mempool competition (MEV)               | Negative; can be severe   |
| Block-inclusion delay (up to 12s)       | Negative; price drifts during the wait |
| Slippage protection failure (tx reverts)| Gas spent, no profit captured |
| Gas price spikes during volatility      | Negative; gas estimate goes stale |
| LP additions/removals between snapshots | Small but non-zero          |

The detector does not model any of these. Alerts should therefore be read as **"there *was* an opportunity of approximately this size at block N"**, not **"a trader would have captured this much"**. A production trading version would discount alerts by an MEV-adjusted profitability factor.

---

## 3. The bot does not account for execution risk on the DEX side

The bot treats the DEX quote as the price it would receive. In reality:

- A submitted Uniswap swap waits in the **public mempool** for up to 12 seconds before the next block mines.
- During that wait, **other transactions accumulate** in the mempool, all of which will execute against the same pool when the block mines.
- The order of execution within a block is determined by an **auction**, not by arrival time. Searchers with higher priority fees (or direct Flashbots bribes) get ordered ahead of slower bidders.
- Any swap executing before yours moves the pool, so your effective price is determined by the **pool state at the moment your transaction runs**, not the state you observed when you queried.

Concretely: a $30/ETH spread observed at block N can decay to $5/ETH (or vanish entirely) by the time the swap actually executes in block N+1.

The detection bot does not model this. A production trading version would model expected MEV loss as a function of trade size and current mempool conditions.

---

## 4. The public mempool exposes strategy

If this service were extended to execute trades, every submitted transaction would be **visible to MEV searchers the moment it is broadcast**. Searchers continuously scan the public mempool, and a profitable arbitrage transaction is an obvious target:

- **Front-running**: a searcher sees your tx, submits an identical or competing tx with a higher priority fee, and captures the opportunity instead of you.
- **Sandwich attack**: a searcher submits a buy immediately before your sell (pushing the pool price down) and a sell immediately after (capturing the rebound), leaving you with a degraded fill.

The standard mitigations — **Flashbots Protect / MEV-Blocker** (private mempool submission), **Flashbots bundles** (atomic ordered execution with a builder bribe), or **intent-based architectures** (delegating execution to a solver) — are out of scope for a detection-only bot. They would be mandatory for any trading extension.

---

## 5. Execution risk is asymmetric between the two venues

The two legs of an arbitrage do not have the same execution profile:

|                                  | Binance (CEX)        | Uniswap (DEX)                          |
|----------------------------------|----------------------|----------------------------------------|
| Latency to execution             | Milliseconds         | Up to 12 seconds (next block)          |
| Price you see ≈ price you get?    | Approximately yes    | No, depends on intra-block ordering    |
| Susceptible to front-running     | No (private orderbook) | Yes (public mempool, MEV)            |
| Order can be reverted by chain   | No                   | Yes (slippage protection or tx failure)|

This asymmetry means execution risk lives almost entirely on the DEX leg. A naive `net_profit = spread × size - costs` calculation hides this asymmetry behind an average and is therefore unsuitable for production trading decisions.

---

## 6. Chain reorganizations are not handled

Ethereum can occasionally reorganize, meaning a block previously believed to be final is replaced by a different block at the same height. The detector currently treats a `newHeads` notification as authoritative, which means:

- An alert may reference a block that the chain later abandons.
- Pool state observed at a reorged block may turn out to have been hypothetical.

A production version would either:

- Wait for confirmation depth (e.g., N blocks deep before treating an observation as final), trading freshness for safety, or
- Track the canonical chain head explicitly and emit a "reorged" event if a previously-published alert's block is replaced.

This is on the spec's discussion-points list and would be addressed before any extension to live trading.

---

## 7. The detector does not model probability of inclusion or gas auctions

A real trading version would have to ask: "If I submit this arbitrage tx with priority fee X, what is the probability it gets included in block N+1?" During high-volatility moments, block space is auctioned aggressively and the probability of inclusion drops sharply for low-priority-fee transactions. The detector ignores this — it implicitly assumes guaranteed inclusion of a hypothetical trade.

This means alerts during volatile periods systematically **overestimate** capturable profit, because the periods most likely to produce large spreads are also the periods most likely to deny inclusion to your tx (or charge you a premium for it).

### What the probe currently reports as "gas price"

The `cmd/probe-chain` CLI surfaces the EIP-1559 **base fee** carried in each block header — it costs zero extra RPC because the header already contains it, and it's the network-wide minimum any transaction in that block had to pay. The probe labels the column explicitly (`baseFee=… gwei`) so the value is not mistaken for the full effective gas price.

The effective gas price a real transaction would pay is:

```
effectiveGasPrice = baseFee + priorityFee
```

where the priority fee (a.k.a. "tip") is set per-transaction by the sender and competes against other pending transactions for inclusion. For arbitrage cost estimation the base fee is a reasonable lower-bound proxy of the network's gas-price floor at a given moment, but a production-grade cost model would add an estimate of the priority fee competitive at the moment of submission (e.g., via `eth_feeHistory` + a percentile heuristic, or a dedicated gas oracle). This is one of the concrete extensions called out for the cost model in the Profitability Evaluator (see [`architecture.md`](./architecture.md)).

---

## 8. Single-pool, single-fee-tier simplification

The challenge's hints, expected output, and example code all point to the Uniswap V3 0.3% fee-tier ETH-USDC pool (`0x88e6A0c2dDD26FEEb64F039a2c41296FcB3f5640`), and the detector follows that convention by querying only that pool. In reality ETH-USDC has multiple Uniswap V3 pools at different fee tiers (0.05%, 0.3%, 1%), each with its own liquidity profile and price. A real router would query all of them per block and pick the best effective price per trade size, and a real arbitrageur might split a large order across several pools at once. The detector adopts the challenge's implied scope and explicitly trades off completeness for simplicity.

---

## 9. Single CEX, single DEX, single pair

The detector observes one CEX (Binance), one DEX (Uniswap V3), one trading pair (ETH-USDC). The architecture is designed to be extensible through interface boundaries, but no other venue is wired up. Any arbitrage opportunity that exists only because of a third venue (for example, Coinbase trading differently from Binance, or Sushiswap diverging from Uniswap) is entirely invisible.

---

## 10. The detector assumes liquidity is available at the moment of observation

The Binance orderbook is snapshotted via REST, which means it represents the state at the moment of fetch but says nothing about what will exist a few hundred milliseconds later when a real trader would act. The pool state, similarly, is the state at the queried block, not the state at execution. Both observations are point-in-time and can become stale even within a single block window.

This is a structural limitation of polling-based observation. An execution-grade system would maintain a streaming orderbook with sequence numbers and re-validate the relevant levels at the moment of order submission.

---

## What a production trading version would add

For completeness — if this detector were extended to actually trade — the following would be required at minimum:

- Private submission of DEX transactions (Flashbots Protect or similar)
- Bundled multi-transaction execution with builder bribes for ordering guarantees
- MEV-adjusted profitability filter that estimates expected loss to searchers
- Chain reorg awareness, including confirmation depth or explicit reorg handling
- Live gas auction modeling and probability-of-inclusion estimates
- Cross-pool / cross-fee-tier routing for the DEX leg
- Hot-key custody and secret management for API keys and signing keys
- Position management, balance reservation, and risk limits
- Post-trade reconciliation and partial-fill handling

None of these are implemented in this detection-only system, by design. They are documented here so the reader of this codebase understands precisely what has been built and what has been left out.
