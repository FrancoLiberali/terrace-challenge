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
	"log/slog"
	"math/big"
	"os"
	"os/signal"
	"strconv"
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

	// Default cost model. Trading fees are NOT here: each adapter folds
	// its venue's intrinsic fees into the Price it returns (Binance's
	// taker fee via binance.Symbol.TakerFeeBps, Uniswap V3's 0.3% pool
	// fee already in QuoterV2's output). Gas units travel per-candidate
	// from QuoterV2's per-call gasEstimate. The model just carries the
	// profitability threshold.
	defaultCostModel = arbitrage.CostModel{
		MinNetProfitUSDC: decimal.NewFromInt(1),
	}
)

func main() {
	if err := run(); err != nil {
		slog.Error("arbd exiting with error", "err", err)
		os.Exit(1)
	}
}

type envConfig struct {
	httpURL string
	wsURL   string
	pretty  bool
	level   slog.Level
}

func loadEnv() (envConfig, error) {
	if err := godotenv.Load(); err != nil {
		return envConfig{}, fmt.Errorf("load .env: %w", err)
	}
	cfg := envConfig{
		httpURL: os.Getenv("ETH_RPC_URL"),
		wsURL:   os.Getenv("ETH_RPC_WS_URL"),
	}
	if cfg.httpURL == "" {
		return envConfig{}, errors.New("ETH_RPC_URL is not set in .env (see README.md)")
	}
	if cfg.wsURL == "" {
		return envConfig{}, errors.New("ETH_RPC_WS_URL is not set in .env (see README.md)")
	}
	if raw := os.Getenv("LOG_LEVEL"); raw != "" {
		if err := cfg.level.UnmarshalText([]byte(raw)); err != nil {
			return envConfig{}, fmt.Errorf("invalid LOG_LEVEL %q: %w", raw, err)
		}
	}
	if raw := os.Getenv("PRETTY_ALERTS"); raw != "" {
		p, err := strconv.ParseBool(raw)
		if err != nil {
			return envConfig{}, fmt.Errorf("invalid PRETTY_ALERTS %q: %w", raw, err)
		}
		cfg.pretty = p
	}
	return cfg, nil
}

func run() error {
	cfg, err := loadEnv()
	if err != nil {
		return err
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: cfg.level})))

	// alertLogger emits unconditionally — the alert is the bot's
	// product, so LOG_LEVEL must not be able to suppress it.
	alertLogger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// Subscriber → Dispatcher → Pathfinder is a three-stage pipeline,
	// each stage running in its own goroutine. The main goroutine
	// consumes candidates and applies the cost model inline.
	sub := chain.NewSubscriber(cfg.wsURL)

	binanceClient := binance.NewClient(binance.DefaultBaseURL)
	binanceSn := pipeline.NewBinanceSnapshotter(binanceClient, binance.SymbolETHUSDC, tradeSizes)

	uniswapClient, err := uniswapv3.NewClient(cfg.httpURL)
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

	slog.Info("arbd starting",
		"venues", []string{"binance", "uniswap"},
		"pair", "ETH-USDC",
		"dex_pool", "uniswap_v3_0.3pct",
		"threshold_usdc", defaultCostModel.MinNetProfitUSDC.String(),
	)
	if cfg.pretty {
		fmt.Fprintf(os.Stdout,
			"arbd: detecting CEX↔DEX arbitrage on ETH-USDC (binance + uniswap v3 0.3%%)\n"+
				"      threshold: net profit > $%s USDC — Ctrl+C to stop\n\n",
			defaultCostModel.MinNetProfitUSDC.String(),
		)
	}

	consume(os.Stdout, pf.Candidates(), ev, cfg.pretty, alertLogger)

	return awaitShutdown(subErr, dispErr, pfErr)
}

func consume(w io.Writer, candidates <-chan pathfinder.CandidatePath, ev *arbitrage.Evaluator, pretty bool, alertLogger *slog.Logger) {
	total, profitable := 0, 0
	for path := range candidates {
		total++
		op := ev.Evaluate(path)
		if !ev.IsProfitable(op) {
			continue
		}
		profitable++
		emitOpportunity(w, op, pretty, alertLogger)
	}
	slog.Info("evaluation finished", "total_candidates", total, "profitable", profitable)
}

func emitOpportunity(w io.Writer, op arbitrage.Opportunity, pretty bool, alertLogger *slog.Logger) {
	alertLogger.Info("arbitrage opportunity detected",
		"block", op.Block.Number,
		"timestamp", op.Block.Timestamp.Format("2006-01-02T15:04:05Z"),
		"direction", op.BuyVenue+"→"+op.SellVenue,
		"buy_venue", op.BuyVenue,
		"sell_venue", op.SellVenue,
		"size_eth", op.Size.String(),
		"buy_price_usdc", op.BuyPrice.StringFixed(4),
		"sell_price_usdc", op.SellPrice.StringFixed(4),
		"spread_per_unit", op.SpreadPerUnit.StringFixed(4),
		"gross_profit_usdc", op.GrossProfit.StringFixed(2),
		"gas_cost_usdc", op.GasCostUSDC.StringFixed(4),
		"gas_estimate", op.GasEstimate,
		"net_profit_usdc", op.NetProfit.StringFixed(2),
		"net_profit_pct", op.NetProfitPct.StringFixed(4),
		"capital_usdc", op.CapitalUSDC.StringFixed(2),
	)
	if pretty {
		printOpportunity(w, op)
	}
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
	fmt.Fprintf(w, "Profit (post-fee): $%s  (already net of venue-intrinsic fees, gross of gas)\n", op.GrossProfit.StringFixed(2))
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
