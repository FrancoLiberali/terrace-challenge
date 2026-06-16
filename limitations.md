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
- [8. Trading fees use a hardcoded schedule, not the operator's live per-account rate](#8-trading-fees-use-a-hardcoded-schedule-not-the-operators-live-per-account-rate)
- [9. Single-pool, single-fee-tier simplification](#9-single-pool-single-fee-tier-simplification)
- [10. Single CEX, single DEX, single pair](#10-single-cex-single-dex-single-pair)
- [11. The detector assumes liquidity is available at the moment of observation](#11-the-detector-assumes-liquidity-is-available-at-the-moment-of-observation)
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

### Gas-price signal: header base fee, not effective gas price

The block subscriber emits the EIP-1559 **base fee** carried in each block header — it costs zero extra RPC because the header already contains it, and it's the network-wide minimum any transaction in that block had to pay. The Profitability Evaluator's cost model consumes that value directly.

The effective gas price a real transaction would pay is:

```
effectiveGasPrice = baseFee + priorityFee
```

where the priority fee (a.k.a. "tip") is set per-transaction by the sender and competes against other pending transactions for inclusion. The base fee is a reasonable lower-bound proxy of the network's gas-price floor at a given moment, but the cost model **systematically underestimates** the gas cost of execution because it ignores the priority fee bid required to actually get included — especially during the high-volatility moments most likely to produce real arbitrage opportunities.

A production-grade cost model would estimate a competitive priority fee at the moment of submission — e.g., via `eth_feeHistory` + a percentile heuristic, or a dedicated gas oracle — and add it to the base fee before subtracting the resulting `effectiveGasPrice × gasUnits` from the spread.

### Gas units: QuoterV2 simulation, not actual mined-block usage

The DEX leg's gas units come from QuoterV2's per-call `gasEstimate` output — the contract's own simulation of the swap against the current pool state. This is more accurate than a hardcoded rule of thumb (it accounts for the actual number of ticks the swap would cross at this size, current liquidity, and current price), but it remains an estimate:

- It simulates against the pool state **at the block being queried**. By the time a hypothetical transaction was mined, other swaps in the same block could have moved the pool, changing the number of ticks the swap actually crosses.
- It does not include the **calling-contract overhead** (SwapRouter / multicall wrappers, signature checks, approval logic). A real on-chain swap through the production router adds roughly 5–15k gas above the raw pool interaction.
- It does not model **cold-vs-warm storage** costs for slots that the executing transaction would touch — those depend on what other transactions ran before it in the same block.

The net effect is that QuoterV2's estimate typically lands within 10–20% of actual mined-block gas usage, biased on the conservative (lower) side. This shares direction with the priority-fee omission described above — both make the cost model under-report gas — but the magnitude is much smaller than the priority-fee gap.

### Gas denomination: converted to USDC via the candidate's `BuyPrice`, not a dedicated reference price

The detector reports gas cost in USDC: `gasUnits × baseFee → wei → ETH → USDC`. The ETH→USDC step uses the candidate's own `BuyPrice` as the reference, which works only because the pair is ETH-USDC and `BuyPrice` happens to be "USDC per ETH" — exactly the number the gas conversion needs.

The shortcut breaks the moment the detector extends to other pairs or chains:

- **Other quote tokens** (e.g. ETH-DAI): `BuyPrice` is "DAI per ETH" — the conversion produces gas cost in DAI, not USDC.
- **Other base tokens** (e.g. WBTC-USDC): `BuyPrice` is "USDC per WBTC" — the multiplication doesn't yield a meaningful number at all (gas is in ETH, not WBTC).
- **Other chains** (Polygon, Arbitrum): gas is paid in the chain's native token (MATIC, etc.), not the asset being traded.

Architecturally the gas-cost step inside the evaluator is fusing two responsibilities: **arithmetic** (gas cost in the chain's native gas token) and **denomination** (expressing that cost in the operator's reporting currency). A multi-pair / multi-chain extension would split them — carry gas in its native unit through the evaluator and convert to the reporting currency at the alert boundary, using a gas-token reference price sourced independently of any one trade pair.

For the single-pair, single-chain scope here, the conflation is harmless: `BuyPrice` happens to be the right ETH/USDC reference. The limitation is shape, not correctness.

### Pre-funded gas reserve: assumed available, not modelled

Gas on Ethereum is paid in ETH out of the executing account's balance at the moment of submission, before any swap output arrives. The operator must hold an ETH reserve in that account — separate from the USDC trading capital — large enough to cover gas for every transaction they intend to submit. The detector assumes this reserve is present and unbounded: per-trade gas is subtracted from the spread for profitability purposes, but the operational prerequisite of *having ETH on hand to pay it* is not modelled.

For a single arb the reserve is trivial (sub-cent at current base fees). Over a long-running session at scale it is non-trivial: the reserve depletes with every transaction, must be topped up from profits, and a depleted reserve halts DEX-side execution silently.

A production trading version would either maintain an explicit gas float — monitored separately from trading capital, gated by a low-water alert — or submit DEX legs as Flashbots bundles that pay the builder bribe atomically out of the arb's own swap proceeds, dissolving the prerequisite in steady state. The bundle path requires the private-mempool stack already noted in §4.

---

## 8. Trading fee is a single static config value, not the operator's live per-account rate

The Binance taker fee is a single static config value (`binance.taker_fee_bps` in `config.yaml`, defaulting to **10 bps / 0.1%** — the [published Spot taker fee](https://www.binance.com/en/fee/schedule) for a Regular User). Pulling the value out of code into config is an improvement over a hardcoded constant — an operator can edit one line to match their own published rate without rebuilding — but it remains a single, static value, so it still does not match what every operator actually pays at every moment:

- **BNB discount**: holding BNB and enabling the discount toggle drops the taker fee to 7.5 bps (25% off).
- **VIP tier**: high-volume accounts (≥ $50M monthly volume) step down through VIP 1–9, with VIP 9 paying as little as 2.4 bps.
- **Per-market variation**: stablecoin-only pairs (e.g. USDC-USDT) often carry 0 bps; new listings may carry promotional rates. The bot only watches one market today, so this is latent rather than actively wrong — but the moment a second Binance market is added, a single config knob can no longer encode their distinct fees.
- **Schedule changes over time**: Binance revises its fee tiers periodically; a static config captures the schedule as of the last edit.

The bias is consistently **conservative**: the detector assumes the operator pays the published default rate, so the bot under-reports profit (missing opportunities a discounted operator could actually capture) rather than over-reports.

The config knob is the seam for a more accurate source. A production-grade adapter would fetch the operator's actual taker fee at startup via Binance's `/sapi/v1/asset/tradeFee` endpoint and refresh it periodically — the only change needed elsewhere is replacing the static config value with the fetched value at adapter-construction time.

---

## 9. Single-pool, single-fee-tier simplification

The detector queries one Uniswap V3 pool per run. The fee tier is operator-selectable via the `UNISWAP_POOL_FEE` env var — `500` (0.05%, the most-liquid ETH-USDC pool at `0x88e6A0c2dDD26FEEb64F039a2c41296FcB3f5640`), `3000` (0.3% at `0x8ad599c3A0ff1De082011EFDDc58f1908eb6e6D8`), or `10000` (1% at `0x7BeA39867e4169DBe237d55C8242a8f2fcDcc387`) — but the bot still observes only one tier at a time. In reality ETH-USDC has multiple Uniswap V3 pools at different fee tiers, each with its own liquidity profile and price. A real router would query all of them per block and pick the best effective price per trade size, and a real arbitrageur might split a large order across several pools at once. The detector adopts the challenge's implied scope and explicitly trades off completeness for simplicity.

---

## 10. Single CEX, single DEX, single pair

The detector observes one CEX (Binance), one DEX (Uniswap V3), one trading pair (ETH-USDC). The architecture is designed to be extensible through interface boundaries, but no other venue is wired up. Any arbitrage opportunity that exists only because of a third venue (for example, Coinbase trading differently from Binance, or Sushiswap diverging from Uniswap) is entirely invisible.

---

## 11. The detector assumes liquidity is available at the moment of observation

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
