package arbitrage

import (
	"math/big"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/FrancoLiberali/terrace-challenge/internal/chain"
	"github.com/FrancoLiberali/terrace-challenge/internal/pathfinder"
)

func dec(s string) decimal.Decimal { return decimal.RequireFromString(s) }

// defaultGasUnits is the per-swap gas estimate the tests use unless they
// override it explicitly. Matches the historic rule-of-thumb value so
// the expected fee/gas/net numbers in older tests remain comparable.
const defaultGasUnits uint64 = 150_000

// mkPath builds a synthetic candidate path with a default gas estimate.
// baseFee is in gwei for readability; we convert to wei here.
func mkPath(buyVenue, sellVenue string, size, buyPrice, sellPrice string, baseFeeGwei int64) pathfinder.CandidatePath {
	return mkPathGas(buyVenue, sellVenue, size, buyPrice, sellPrice, baseFeeGwei, defaultGasUnits)
}

// mkPathGas is mkPath with the gas estimate overridable. Used by tests
// that vary gas explicitly.
func mkPathGas(buyVenue, sellVenue string, size, buyPrice, sellPrice string, baseFeeGwei int64, gasUnits uint64) pathfinder.CandidatePath {
	const gweiInWei = 1_000_000_000
	baseFeeWei := new(big.Int).Mul(big.NewInt(baseFeeGwei), big.NewInt(gweiInWei))
	return pathfinder.CandidatePath{
		Block: chain.BlockEvent{
			Number:    100,
			Timestamp: time.Unix(1_700_000_000, 0).UTC(),
			BaseFee:   baseFeeWei,
		},
		Size:        dec(size),
		BuyVenue:    buyVenue,
		SellVenue:   sellVenue,
		BuyPrice:    dec(buyPrice),
		SellPrice:   dec(sellPrice),
		GasEstimate: gasUnits,
	}
}

// standardModel: Binance at 0.1%, Uniswap fee-free at this layer,
// $1 minimum profit. (Gas units now travel on each CandidatePath rather
// than living on the model.)
func standardModel() CostModel {
	return CostModel{
		VenueFeeBps:      map[string]int{"binance": 10},
		MinNetProfitUSDC: decimal.NewFromInt(1),
	}
}

func TestEvaluator_ProfitableArbAcrossVenues(t *testing.T) {
	// Buy 10 ETH on Binance at $1680, sell on Uniswap at $1690.
	// Gross: 10 × $10 = $100.
	// Binance fee: 0.1% × ($1680 × 10) = $16.80.
	// Uniswap fee: 0 (not in map).
	// Gas: 150k × 1 gwei = 150k gwei = 1.5e-4 ETH × $1680 = $0.252.
	// Net: $100 - $16.80 - $0.252 = $82.948 → profitable.
	e := NewEvaluator(standardModel())
	op := e.Evaluate(mkPath("binance", "uniswap", "10", "1680", "1690", 1))

	if !op.GrossProfit.Equal(dec("100")) {
		t.Errorf("gross: got %s, want 100", op.GrossProfit)
	}
	if !op.TradingFees.Equal(dec("16.8")) {
		t.Errorf("trading fees: got %s, want 16.8", op.TradingFees)
	}
	if !op.GasCostUSDC.Equal(dec("0.252")) {
		t.Errorf("gas: got %s, want 0.252", op.GasCostUSDC)
	}
	if !op.NetProfit.Equal(dec("82.948")) {
		t.Errorf("net: got %s, want 82.948", op.NetProfit)
	}
	if !op.CapitalUSDC.Equal(dec("16800")) {
		t.Errorf("capital: got %s, want 16800", op.CapitalUSDC)
	}
	// 82.948 / 16800 × 100 = 0.49374... (within decimal precision)
	if !op.NetProfitPct.Round(4).Equal(dec("0.4937")) {
		t.Errorf("pct: got %s, want ~0.4937", op.NetProfitPct.Round(4))
	}
	if !e.IsProfitable(op) {
		t.Error("opportunity should be profitable at $1 threshold")
	}
}

func TestEvaluator_FeesAppliedOnBothCEXLegs(t *testing.T) {
	// Hypothetical CEX → CEX path (both venues incur taker fees).
	// Buy 1 ETH on binance at $1680, sell on coinbase at $1690.
	// binance fee: 0.1% × 1680 = $1.68
	// coinbase fee: 0.2% × 1690 = $3.38
	// gas: 0 (we pass baseFee=0 so the term vanishes).
	model := standardModel()
	model.VenueFeeBps["coinbase"] = 20 // 0.2%

	e := NewEvaluator(model)
	op := e.Evaluate(mkPath("binance", "coinbase", "1", "1680", "1690", 0))

	if !op.TradingFees.Equal(dec("5.06")) {
		t.Errorf("trading fees: got %s, want 5.06 (1.68 binance + 3.38 coinbase)", op.TradingFees)
	}
	// Net = 10 - 5.06 - 0 = 4.94.
	if !op.NetProfit.Equal(dec("4.94")) {
		t.Errorf("net: got %s, want 4.94", op.NetProfit)
	}
}

func TestEvaluator_FeeFreeWhenVenueNotInModel(t *testing.T) {
	// Uniswap → Sushiswap, neither in the model — both fee-free at
	// this layer. Net = gross - gas only.
	e := NewEvaluator(standardModel())
	op := e.Evaluate(mkPath("uniswap", "sushi", "1", "1680", "1690", 1))

	if !op.TradingFees.Equal(decimal.Zero) {
		t.Errorf("trading fees: got %s, want 0", op.TradingFees)
	}
}

func TestEvaluator_LossMakingNotProfitable(t *testing.T) {
	// Buy at 1685, sell at 1680 — gross is negative. Even before
	// fees and gas this isn't profitable.
	e := NewEvaluator(standardModel())
	op := e.Evaluate(mkPath("binance", "uniswap", "1", "1685", "1680", 1))

	if !op.SpreadPerUnit.Equal(dec("-5")) {
		t.Errorf("spread: got %s, want -5", op.SpreadPerUnit)
	}
	if op.NetProfit.IsPositive() {
		t.Errorf("net should be negative, got %s", op.NetProfit)
	}
	if e.IsProfitable(op) {
		t.Error("opportunity should not be profitable")
	}
}

func TestEvaluator_GasCostScalesWithBaseFee(t *testing.T) {
	// Compare gas cost at baseFee=10 gwei vs 1 gwei: should be 10× higher.
	e := NewEvaluator(standardModel())
	lo := e.Evaluate(mkPath("binance", "uniswap", "1", "1680", "1690", 1))
	hi := e.Evaluate(mkPath("binance", "uniswap", "1", "1680", "1690", 10))

	ratio := hi.GasCostUSDC.Div(lo.GasCostUSDC)
	if !ratio.Round(4).Equal(dec("10")) {
		t.Errorf("gas ratio (10 gwei vs 1 gwei): got %s, want 10", ratio.Round(4))
	}
}

func TestEvaluator_NilBaseFeeTreatedAsZero(t *testing.T) {
	// Defensive: a candidate carrying a nil BaseFee (pre-London chain?
	// fixture mistake?) should not panic.
	path := mkPath("binance", "uniswap", "1", "1680", "1690", 1)
	path.Block.BaseFee = nil

	e := NewEvaluator(standardModel())
	op := e.Evaluate(path)
	if !op.GasCostUSDC.Equal(decimal.Zero) {
		t.Errorf("nil baseFee should produce zero gas cost, got %s", op.GasCostUSDC)
	}
}

func TestEvaluator_NetProfitPctOnZeroCapital(t *testing.T) {
	// Defensive: zero buy price → zero capital → pct division by zero.
	// Evaluator must return 0 pct rather than panic or NaN.
	e := NewEvaluator(standardModel())
	op := e.Evaluate(mkPath("binance", "uniswap", "1", "0", "10", 0))
	if !op.NetProfitPct.Equal(decimal.Zero) {
		t.Errorf("expected 0 pct on zero capital, got %s", op.NetProfitPct)
	}
}

func TestEvaluator_IsProfitableHonorsThreshold(t *testing.T) {
	// Net profit = $10 - $1.68 - $0.252 = $8.068. With a $5 threshold,
	// profitable. With a $10 threshold, not.
	path := mkPath("binance", "uniswap", "1", "1680", "1690", 1)

	above := standardModel()
	above.MinNetProfitUSDC = decimal.NewFromInt(5)
	if !NewEvaluator(above).IsProfitable(NewEvaluator(above).Evaluate(path)) {
		t.Error("should be profitable at $5 threshold")
	}

	below := standardModel()
	below.MinNetProfitUSDC = decimal.NewFromInt(10)
	if NewEvaluator(below).IsProfitable(NewEvaluator(below).Evaluate(path)) {
		t.Error("should NOT be profitable at $10 threshold")
	}
}
