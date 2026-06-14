// Command arbd subscribes to Ethereum's newHeads stream, dispatches
// per-block snapshot work to the Binance and Uniswap V3 adapters in
// parallel, pairs the results via the Pathfinder, evaluates each
// candidate against the cost model, and prints structured arbitrage
// alerts above the configured threshold.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/joho/godotenv"
	"github.com/shopspring/decimal"

	"github.com/FrancoLiberali/terrace-challenge/internal/arbitrage"
	"github.com/FrancoLiberali/terrace-challenge/internal/cex/binance"
	"github.com/FrancoLiberali/terrace-challenge/internal/chain"
	"github.com/FrancoLiberali/terrace-challenge/internal/dex/uniswapv3"
	"github.com/FrancoLiberali/terrace-challenge/internal/pathfinder"
	"github.com/FrancoLiberali/terrace-challenge/internal/pipeline"
)

// Hardcoded for now; configuration lands in Step 7.
var (
	tradeSizes = []decimal.Decimal{
		decimal.NewFromInt(1),
		decimal.NewFromInt(10),
		decimal.NewFromInt(100),
	}

	// Default cost model. Binance spot taker fee is 0.1% = 10 bps;
	// Uniswap V3's 0.3% pool fee is already embedded in QuoterV2's
	// output so the venue doesn't appear in the fee map. Gas units
	// for the DEX leg travel on each CandidatePath, sourced from
	// QuoterV2's per-call gasEstimate output.
	defaultCostModel = arbitrage.CostModel{
		VenueFeeBps:      map[string]int{"binance": 10},
		MinNetProfitUSDC: decimal.NewFromInt(1),
	}
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("arbd: %v", err)
	}
}

func run() error {
	if err := godotenv.Load(); err != nil {
		return fmt.Errorf("load .env: %w", err)
	}
	httpURL := os.Getenv("ETH_RPC_URL")
	if httpURL == "" {
		return errors.New("ETH_RPC_URL is not set in .env (see README.md)")
	}
	wsURL := os.Getenv("ETH_RPC_WS_URL")
	if wsURL == "" {
		return errors.New("ETH_RPC_WS_URL is not set in .env (see README.md)")
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// Subscriber → Dispatcher → Pathfinder is a three-stage pipeline,
	// each stage running in its own goroutine. The main goroutine
	// consumes candidates and applies the cost model inline.
	sub := chain.NewSubscriber(wsURL)

	binanceClient := binance.NewClient(binance.DefaultBaseURL)
	binanceSn := pipeline.NewBinanceSnapshotter(binanceClient, binance.SymbolETHUSDC, tradeSizes)

	uniswapClient, err := uniswapv3.NewClient(httpURL)
	if err != nil {
		return fmt.Errorf("connect to RPC: %w", err)
	}
	defer uniswapClient.Close()
	uniswapSn := pipeline.NewUniswapSnapshotter(uniswapClient, uniswapv3.PoolETHUSDC03, tradeSizes)

	disp := pipeline.NewDispatcher(map[string]pipeline.Snapshotter{
		"binance": binanceSn,
		"uniswap": uniswapSn,
	})
	pf := pathfinder.NewPathfinder()
	ev := arbitrage.NewEvaluator(defaultCostModel)

	subErr := make(chan error, 1)
	go func() { subErr <- sub.Run(ctx) }()
	dispErr := make(chan error, 1)
	go func() { dispErr <- disp.Run(ctx, sub.Events()) }()
	pfErr := make(chan error, 1)
	go func() { pfErr <- pf.Run(ctx, disp.Results()) }()

	fmt.Fprintf(os.Stdout,
		"arbd: detecting CEX↔DEX arbitrage on ETH-USDC (binance + uniswap v3 0.3%%)\n"+
			"      threshold: net profit > $%s USDC — Ctrl+C to stop\n\n",
		defaultCostModel.MinNetProfitUSDC.String(),
	)

	consume(os.Stdout, pf.Candidates(), ev)

	return awaitShutdown(subErr, dispErr, pfErr)
}

// consume drives the final stage of the pipeline: for every CandidatePath
// the Pathfinder emits, evaluate it against the cost model and print a
// structured alert when net profit clears the threshold. Returns when
// the upstream channel closes (i.e., the Pathfinder's Run has returned).
func consume(w io.Writer, candidates <-chan pathfinder.CandidatePath, ev *arbitrage.Evaluator) {
	total, profitable := 0, 0
	for path := range candidates {
		total++
		op := ev.Evaluate(path)
		if !ev.IsProfitable(op) {
			continue
		}
		profitable++
		printOpportunity(w, op)
	}
	fmt.Fprintf(w, "\narbd: evaluated %d candidates, emitted %d above threshold\n", total, profitable)
}

// awaitShutdown collects each pipeline stage's Run result. ctx-cancel
// errors are the expected clean-exit path (SIGINT); anything else is
// propagated to the caller.
func awaitShutdown(subErr, dispErr, pfErr <-chan error) error {
	if err := <-subErr; err != nil && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("subscriber: %w", err)
	}
	if err := <-dispErr; err != nil && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("dispatcher: %w", err)
	}
	if err := <-pfErr; err != nil && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("pathfinder: %w", err)
	}
	return nil
}

// printOpportunity formats an arbitrage alert matching the style of the
// example in CHALLENGE.md (block, timestamp, direction, prices, profit,
// execution steps).
func printOpportunity(w io.Writer, op arbitrage.Opportunity) {
	directionLabel := op.BuyVenue + " → " + op.SellVenue
	fmt.Fprintln(w, "=== ARBITRAGE OPPORTUNITY DETECTED ===")
	fmt.Fprintf(w, "Block Number: %d\n", op.Block.Number)
	fmt.Fprintf(w, "Timestamp:    %s\n", op.Block.Timestamp.Format("2006-01-02 15:04:05 UTC"))
	fmt.Fprintf(w, "Direction:    %s  (Buy on %s, Sell on %s)\n", directionLabel, op.BuyVenue, op.SellVenue)
	fmt.Fprintln(w)
	fmt.Fprintf(w, "Trade Size:        %s ETH\n", op.Size.String())
	fmt.Fprintf(w, "Buy  Price:        $%s / ETH (effective, slippage-aware) — %s\n", op.BuyPrice.StringFixed(4), op.BuyVenue)
	fmt.Fprintf(w, "Sell Price:        $%s / ETH (effective, slippage-aware) — %s\n", op.SellPrice.StringFixed(4), op.SellVenue)
	fmt.Fprintf(w, "Spread per unit:   $%s / ETH (%s%%)\n",
		op.SpreadPerUnit.StringFixed(4),
		op.SpreadPerUnit.Div(op.BuyPrice).Mul(decimal.NewFromInt(100)).StringFixed(4))
	fmt.Fprintln(w)
	fmt.Fprintf(w, "Gross Profit:      $%s\n", op.GrossProfit.StringFixed(2))
	fmt.Fprintf(w, "Trading Fees:      $%s\n", op.TradingFees.StringFixed(2))
	fmt.Fprintf(w, "Gas Cost (est):    $%s  (baseFee=%s gwei, ~%d gas)\n",
		op.GasCostUSDC.StringFixed(4),
		formatGwei(op.Block.BaseFee),
		op.GasEstimate,
	)
	fmt.Fprintf(w, "Net Profit:        $%s  (%s%%)\n",
		op.NetProfit.StringFixed(2),
		op.NetProfitPct.StringFixed(4),
	)
	fmt.Fprintf(w, "Capital Required:  $%s USDC\n", op.CapitalUSDC.StringFixed(2))
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Execution Steps:")
	fmt.Fprintf(w, "  1. Buy  %s ETH on %s at ~$%s/ETH (capital: ~$%s)\n",
		op.Size.String(), op.BuyVenue, op.BuyPrice.StringFixed(2), op.CapitalUSDC.StringFixed(2))
	fmt.Fprintf(w, "  2. Move ETH to the venue that sells (operationally — bridging, transfer time, etc.)\n")
	fmt.Fprintf(w, "  3. Sell %s ETH on %s at ~$%s/ETH (expected: ~$%s)\n",
		op.Size.String(), op.SellVenue, op.SellPrice.StringFixed(2),
		op.SellPrice.Mul(op.Size).StringFixed(2),
	)
	fmt.Fprintln(w, "Risk factors: see limitations.md (intra-block drift, MEV on the DEX leg, gas-price spikes)")
	fmt.Fprintln(w, strings.Repeat("─", 60))
}

// formatGwei prints a wei amount as a fixed-point gwei string (3 dp).
// Duplicated from probe-chain — small enough that a shared helper isn't
// worth a new package yet.
func formatGwei(wei *big.Int) string {
	if wei == nil {
		return "n/a"
	}
	const oneGweiInWei = 1_000_000_000
	gwei := new(big.Int).Mul(wei, big.NewInt(1000))
	gwei.Quo(gwei, big.NewInt(oneGweiInWei))
	whole := new(big.Int).Quo(gwei, big.NewInt(1000))
	frac := new(big.Int).Mod(gwei, big.NewInt(1000))
	return fmt.Sprintf("%s.%03d", whole.String(), frac.Int64())
}
