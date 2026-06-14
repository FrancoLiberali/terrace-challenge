// Package arbitrage applies a cost model to CandidatePaths produced by
// the Pathfinder and decides whether each one is profitable. Evaluation
// is a pure function over the path and the configured CostModel — no
// I/O, no goroutines.
package arbitrage

import (
	"math/big"

	"github.com/shopspring/decimal"

	"github.com/FrancoLiberali/terrace-challenge/internal/pathfinder"
)

// CostModel parameterises the profitability calculation.
type CostModel struct {
	// VenueFeeBps maps a venue name to its taker fee in basis points.
	// Binance spot is 10 bps (0.1%); a venue not in the map is treated
	// as fee-free at this layer (the DEX's 0.3% pool fee is already
	// embedded in the QuoterV2 output and is NOT included here).
	VenueFeeBps map[string]int

	// GasUnitsPerSwap is the gas estimate for the on-chain leg
	// (one Uniswap V3 swap ≈ 150,000 units).
	GasUnitsPerSwap uint64

	// MinNetProfitUSDC is the threshold IsProfitable compares NetProfit
	// against. Decimal so callers can set fractional thresholds.
	MinNetProfitUSDC decimal.Decimal
}

// Opportunity is the result of evaluating a CandidatePath against the
// cost model. CandidatePath is embedded so callers see the underlying
// venues, prices, and block context.
//
// Profit fields are USDC-denominated. NetProfitPct is the net profit
// expressed as a percentage of the capital required.
type Opportunity struct {
	pathfinder.CandidatePath

	SpreadPerUnit decimal.Decimal // SellPrice - BuyPrice
	GrossProfit   decimal.Decimal // SpreadPerUnit × Size
	TradingFees   decimal.Decimal // sum of per-venue taker fees
	GasCostUSDC   decimal.Decimal // gas estimate valued in USDC
	NetProfit     decimal.Decimal // GrossProfit - TradingFees - GasCostUSDC
	NetProfitPct  decimal.Decimal // (NetProfit / CapitalUSDC) × 100
	CapitalUSDC   decimal.Decimal // BuyPrice × Size — what you need to put up
}

// Evaluator holds the cost model and applies it to candidate paths.
type Evaluator struct {
	model CostModel
}

// NewEvaluator returns an Evaluator using the given cost model.
func NewEvaluator(model CostModel) *Evaluator {
	return &Evaluator{model: model}
}

const (
	// bpsDenominator converts basis points to a fraction (10 bps = 0.1%).
	bpsDenominator = 10000
	// weiToETHShift is the number of decimal places between wei and ETH.
	weiToETHShift = 18
)

// Evaluate computes the Opportunity for the candidate, regardless of
// profitability. Callers use IsProfitable to filter.
func (e *Evaluator) Evaluate(path pathfinder.CandidatePath) Opportunity {
	spreadPerUnit := path.SellPrice.Sub(path.BuyPrice)
	grossProfit := spreadPerUnit.Mul(path.Size)
	capital := path.BuyPrice.Mul(path.Size)

	// Per-venue taker fees: each leg's fee, valued at that leg's
	// notional. Venues not in the model contribute zero (the DEX case).
	tradingFees := e.venueFee(path.BuyVenue, capital).
		Add(e.venueFee(path.SellVenue, path.SellPrice.Mul(path.Size)))

	// Gas cost: gasUnits × baseFee → wei → ETH → USDC (valued at
	// BuyPrice as a reasonable per-block ETH→USDC reference).
	gasUSDC := e.gasCostUSDC(path.Block.BaseFee, path.BuyPrice)

	netProfit := grossProfit.Sub(tradingFees).Sub(gasUSDC)

	netProfitPct := decimal.Zero
	if capital.IsPositive() {
		netProfitPct = netProfit.Div(capital).Mul(decimal.NewFromInt(100)) //nolint:mnd // percentage scale
	}

	return Opportunity{
		CandidatePath: path,
		SpreadPerUnit: spreadPerUnit,
		GrossProfit:   grossProfit,
		TradingFees:   tradingFees,
		GasCostUSDC:   gasUSDC,
		NetProfit:     netProfit,
		NetProfitPct:  netProfitPct,
		CapitalUSDC:   capital,
	}
}

// IsProfitable reports whether the opportunity's net profit strictly
// exceeds the configured threshold.
func (e *Evaluator) IsProfitable(o Opportunity) bool {
	return o.NetProfit.GreaterThan(e.model.MinNetProfitUSDC)
}

// venueFee returns the per-venue taker fee on the given notional (USDC).
// Venues not configured contribute zero — DEX fees are baked into the
// price upstream.
func (e *Evaluator) venueFee(venue string, notional decimal.Decimal) decimal.Decimal {
	bps, ok := e.model.VenueFeeBps[venue]
	if !ok {
		return decimal.Zero
	}
	return notional.
		Mul(decimal.NewFromInt(int64(bps))).
		Div(decimal.NewFromInt(bpsDenominator))
}

// gasCostUSDC converts gasUnits × baseFee (wei) to USDC using the given
// ETH-in-USDC price as the reference. BaseFee is the EIP-1559 minimum;
// this systematically underestimates the true cost because it ignores
// the priority fee (see limitations.md §7).
func (e *Evaluator) gasCostUSDC(baseFeeWei *big.Int, ethPrice decimal.Decimal) decimal.Decimal {
	if baseFeeWei == nil {
		return decimal.Zero
	}
	gasWei := new(big.Int).Mul(big.NewInt(int64(e.model.GasUnitsPerSwap)), baseFeeWei) //nolint:gosec // gas units are small
	gasETH := decimal.NewFromBigInt(gasWei, 0).Shift(-int32(weiToETHShift))
	return gasETH.Mul(ethPrice)
}
