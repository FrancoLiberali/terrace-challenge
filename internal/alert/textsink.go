// Package alert is where the bot emits arbitrage opportunities — the
// product of the detection pipeline. TextSink combines a structured
// slog event (always emitted) with an optional human-readable block
// gated by PRETTY_ALERTS for local-dev terminals.
package alert

import (
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/shopspring/decimal"

	"github.com/FrancoLiberali/terrace-challenge/internal/arbitrage"
)

// Display-precision constants for the formatted alert output.
const (
	// dpUSDC: cents — dollars-and-cents display for USDC amounts
	// (profit, capital).
	dpUSDC = 2
	// dpPrice: tenths-of-a-cent — keeps per-unit price detail visible
	// where a small spread can be the whole opportunity.
	dpPrice = 4

	// sepWidth: width of the visual separator at the bottom of the
	// pretty alert block.
	sepWidth = 60

	// gwei <-> wei conversion helpers for the gas-price formatter.
	// 1 gwei == 1e9 wei; we render with 3-decimal precision.
	weiPerGwei         = 1_000_000_000
	gweiMilliPrecision = 1000

	// Step numbers in the multi-line execution-steps section. Fixed
	// 3-step flow: buy on venue A, transfer ETH, sell on venue B.
	stepBuy      = 1
	stepTransfer = 2
	stepSell     = 3
)

// TextSink is the default Sink. Every Emit fires a structured slog
// event (one record per opportunity, all fields keyed for log
// aggregators) and, when Pretty is true, also writes the multi-line
// human-readable block from CHALLENGE.md to Out.
//
// UniswapVenue is the venue label that should be formatted as a DEX
// swap in the execution steps (with pool address + expected output);
// UniswapPoolAddress is the resolved pool address for that venue.
// When UniswapVenue is empty, the DEX-specific formatting is skipped.
type TextSink struct {
	Logger             *slog.Logger
	Out                io.Writer
	Pretty             bool
	UniswapVenue       string
	UniswapPoolAddress common.Address
}

// Emit writes the opportunity to both channels. The slog event is
// always emitted; the multi-line block is only written when Pretty
// is true (gated by PRETTY_ALERTS in the arbd binary).
func (s *TextSink) Emit(op arbitrage.Opportunity) {
	s.Logger.Info("arbitrage opportunity detected",
		"block", op.Block.Number,
		"timestamp", op.Block.Timestamp.Format("2006-01-02T15:04:05Z"),
		"direction", op.BuyVenue+"→"+op.SellVenue,
		"buy_venue", op.BuyVenue,
		"sell_venue", op.SellVenue,
		"size_eth", op.Size.String(),
		"buy_price_usdc", op.BuyPrice.StringFixed(dpPrice),
		"sell_price_usdc", op.SellPrice.StringFixed(dpPrice),
		"spread_per_unit", op.SpreadPerUnit.StringFixed(dpPrice),
		"gross_profit_usdc", op.GrossProfit.StringFixed(dpUSDC),
		"gas_cost_usdc", op.GasCostUSDC.StringFixed(dpPrice),
		"gas_estimate", op.GasEstimate,
		"net_profit_usdc", op.NetProfit.StringFixed(dpUSDC),
		"net_profit_pct", op.NetProfitPct.StringFixed(dpPrice),
		"capital_usdc", op.CapitalUSDC.StringFixed(dpUSDC),
		"uniswap_pool", s.UniswapPoolAddress.Hex(),
	)
	if s.Pretty {
		s.printPretty(op)
	}
}

// printPretty emits the multi-line alert block that matches the
// format shown in CHALLENGE.md.
func (s *TextSink) printPretty(op arbitrage.Opportunity) {
	w := s.Out
	directionLabel := op.BuyVenue + " → " + op.SellVenue
	fmt.Fprintln(w, "=== ARBITRAGE OPPORTUNITY DETECTED ===")
	fmt.Fprintf(w, "Block Number: %d\n", op.Block.Number)
	fmt.Fprintf(w, "Timestamp:    %s\n", op.Block.Timestamp.Format("2006-01-02 15:04:05 UTC"))
	fmt.Fprintf(w, "Direction:    %s  (Buy on %s, Sell on %s)\n", directionLabel, op.BuyVenue, op.SellVenue)
	fmt.Fprintln(w)
	fmt.Fprintf(w, "Trade Size:        %s ETH\n", op.Size.String())
	fmt.Fprintf(w, "Buy  Price:        $%s / ETH (effective, slippage-aware) — %s\n", op.BuyPrice.StringFixed(dpPrice), op.BuyVenue)
	fmt.Fprintf(w, "Sell Price:        $%s / ETH (effective, slippage-aware) — %s\n", op.SellPrice.StringFixed(dpPrice), op.SellVenue)
	fmt.Fprintf(w, "Spread per unit:   $%s / ETH (%s%%)\n",
		op.SpreadPerUnit.StringFixed(dpPrice),
		op.SpreadPerUnit.Div(op.BuyPrice).Mul(decimal.NewFromInt(percentScale)).StringFixed(dpPrice))
	fmt.Fprintln(w)
	fmt.Fprintf(w, "Profit (post-fee): $%s  (already net of venue-intrinsic fees, gross of gas)\n", op.GrossProfit.StringFixed(dpUSDC))
	fmt.Fprintf(w, "Gas Cost (est):    $%s  (baseFee=%s gwei, ~%d gas)\n",
		op.GasCostUSDC.StringFixed(dpPrice),
		formatGwei(op.Block.BaseFee),
		op.GasEstimate,
	)
	fmt.Fprintf(w, "Net Profit:        $%s  (%s%%)\n",
		op.NetProfit.StringFixed(dpUSDC),
		op.NetProfitPct.StringFixed(dpPrice),
	)
	fmt.Fprintf(w, "Capital Required:  $%s USDC\n", op.CapitalUSDC.StringFixed(dpUSDC))
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Execution Steps:")
	s.writeBuyStep(w, stepBuy, op)
	fmt.Fprintf(w, "  %d. Transfer ETH from buy venue to sell venue (operationally — bridging, transfer time, etc.)\n", stepTransfer)
	s.writeSellStep(w, stepSell, op)
	fmt.Fprintln(w, "Risk factors: see limitations.md (intra-block drift, MEV on the DEX leg, gas-price spikes)")
	fmt.Fprintln(w, strings.Repeat("─", sepWidth))
}

// writeBuyStep emits the buy leg. If the buy venue matches
// s.UniswapVenue the step is formatted as a Uniswap V3 swap with the
// pool address and required input as sub-bullets (matching the alert
// shape in CHALLENGE.md); otherwise as a CEX buy.
func (s *TextSink) writeBuyStep(w io.Writer, n int, op arbitrage.Opportunity) {
	if s.isUniswap(op.BuyVenue) {
		fmt.Fprintf(w, "  %d. Execute Uniswap V3 swap: USDC → %s ETH\n", n, op.Size.String())
		fmt.Fprintf(w, "     - Pool: %s\n", s.UniswapPoolAddress.Hex())
		fmt.Fprintf(w, "     - Required input: ~$%s USDC\n", op.CapitalUSDC.StringFixed(dpUSDC))
		return
	}
	fmt.Fprintf(w, "  %d. Buy %s ETH on %s at ~$%s/ETH\n", n, op.Size.String(), op.BuyVenue, op.BuyPrice.StringFixed(dpUSDC))
	fmt.Fprintf(w, "     - Required capital: ~$%s USDC\n", op.CapitalUSDC.StringFixed(dpUSDC))
}

// writeSellStep emits the sell leg, symmetric to writeBuyStep.
func (s *TextSink) writeSellStep(w io.Writer, n int, op arbitrage.Opportunity) {
	sellOutput := op.SellPrice.Mul(op.Size)
	if s.isUniswap(op.SellVenue) {
		fmt.Fprintf(w, "  %d. Execute Uniswap V3 swap: %s ETH → USDC\n", n, op.Size.String())
		fmt.Fprintf(w, "     - Pool: %s\n", s.UniswapPoolAddress.Hex())
		fmt.Fprintf(w, "     - Expected output: ~$%s USDC\n", sellOutput.StringFixed(dpUSDC))
		return
	}
	fmt.Fprintf(w, "  %d. Sell %s ETH on %s at ~$%s/ETH\n", n, op.Size.String(), op.SellVenue, op.SellPrice.StringFixed(dpUSDC))
	fmt.Fprintf(w, "     - Expected proceeds: ~$%s USDC\n", sellOutput.StringFixed(dpUSDC))
}

func (s *TextSink) isUniswap(venue string) bool {
	return s.UniswapVenue != "" && venue == s.UniswapVenue
}

// percentScale converts a ratio (0..1) into a percentage (0..100) for
// display.
const percentScale = 100

// formatGwei prints a wei amount as a fixed-point gwei string (3 dp).
func formatGwei(wei *big.Int) string {
	if wei == nil {
		return "n/a"
	}
	gwei := new(big.Int).Mul(wei, big.NewInt(gweiMilliPrecision))
	gwei.Quo(gwei, big.NewInt(weiPerGwei))
	whole := new(big.Int).Quo(gwei, big.NewInt(gweiMilliPrecision))
	frac := new(big.Int).Mod(gwei, big.NewInt(gweiMilliPrecision))
	return fmt.Sprintf("%s.%03d", whole.String(), frac.Int64())
}
