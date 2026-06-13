package binance

import "github.com/shopspring/decimal"

// Symbol identifies a Binance market and carries the metadata needed to query
// it. Code is the wire-level identifier Binance expects (e.g. "ETHUSDC").
// EstLiquidityPerLevel is a conservative estimate of base-token units per
// orderbook level, used to choose an initial depth-endpoint tier without
// over-fetching. The estimate is per-pair because depth profiles vary
// dramatically across markets — a deep blue-chip pair like ETH-USDC has
// orders of magnitude more liquidity per level than a thin altcoin pair.
type Symbol struct {
	Code                 string
	EstLiquidityPerLevel decimal.Decimal
}

// ethusdcLiquidityPerLevel is a deliberately conservative estimate of
// base-token units per orderbook level for ETH-USDC: top-of-book often
// shows tens of ETH per level, but the tail thins out quickly. At 5
// ETH/level the depth-tier heuristic picks the cheapest tier (weight=5)
// for the configured trade sizes and only escalates when the book is
// genuinely thin.
const ethusdcLiquidityPerLevel int64 = 5

// SymbolETHUSDC is the ETH-USDC market on Binance Spot.
var SymbolETHUSDC = Symbol{
	Code:                 "ETHUSDC",
	EstLiquidityPerLevel: decimal.NewFromInt(ethusdcLiquidityPerLevel),
}
