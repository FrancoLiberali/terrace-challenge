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
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/ethereum/go-ethereum/common"
	"github.com/joho/godotenv"

	"github.com/FrancoLiberali/terrace-challenge/internal/alert"
	"github.com/FrancoLiberali/terrace-challenge/internal/arbitrage"
	"github.com/FrancoLiberali/terrace-challenge/internal/cex/binance"
	"github.com/FrancoLiberali/terrace-challenge/internal/chain"
	"github.com/FrancoLiberali/terrace-challenge/internal/config"
	"github.com/FrancoLiberali/terrace-challenge/internal/dex/uniswapv3"
	"github.com/FrancoLiberali/terrace-challenge/internal/pathfinder"
	"github.com/FrancoLiberali/terrace-challenge/internal/pipeline"
	"github.com/FrancoLiberali/terrace-challenge/internal/resilience"
)

// Venue identifiers used across map keys, log fields, breaker labels,
// and the "venues" list — the canonical names the rest of the bot
// refers to each integration by.
const (
	venueBinance = "binance"
	venueUniswap = "uniswap"
)

// defaultConfigPath is where Load looks for the YAML if CONFIG_FILE is
// not set in the environment.
const defaultConfigPath = "config.yaml"

func main() {
	if err := run(); err != nil {
		slog.Error("arbd exiting with error", "err", err)
		os.Exit(1)
	}
}

type envConfig struct {
	ethRPCURL      string
	wsURL          string
	binanceBaseURL string
	uniswapPoolFee uint32
	uniswapQuoter  common.Address
	pretty         bool
	level          slog.Level
}

func loadEnv() (envConfig, error) {
	// godotenv.Load is best-effort: if .env exists it's loaded into
	// the process environment, but a missing file is fine — the
	// expected case in Docker / CI where env vars are injected by
	// the runtime. A malformed .env is still surfaced.
	if err := godotenv.Load(); err != nil && !errors.Is(err, os.ErrNotExist) {
		return envConfig{}, fmt.Errorf("load .env: %w", err)
	}
	cfg := envConfig{
		ethRPCURL:      os.Getenv("ETH_RPC_URL"),
		wsURL:          os.Getenv("ETH_RPC_WS_URL"),
		binanceBaseURL: os.Getenv("BINANCE_BASE_URL"),
	}
	if cfg.ethRPCURL == "" {
		return envConfig{}, errors.New("ETH_RPC_URL is not set in .env (see README.md)")
	}
	if cfg.wsURL == "" {
		return envConfig{}, errors.New("ETH_RPC_WS_URL is not set in .env (see README.md)")
	}
	if cfg.binanceBaseURL == "" {
		return envConfig{}, errors.New("BINANCE_BASE_URL is not set in .env (see README.md)")
	}
	feeRaw := os.Getenv("UNISWAP_POOL_FEE")
	if feeRaw == "" {
		return envConfig{}, errors.New("UNISWAP_POOL_FEE is not set in .env (see README.md)")
	}
	fee, err := strconv.ParseUint(feeRaw, 10, 32)
	if err != nil {
		return envConfig{}, fmt.Errorf("invalid UNISWAP_POOL_FEE %q: %w", feeRaw, err)
	}
	cfg.uniswapPoolFee = uint32(fee)
	quoterRaw := os.Getenv("UNISWAP_QUOTER_ADDRESS")
	if quoterRaw == "" {
		return envConfig{}, errors.New("UNISWAP_QUOTER_ADDRESS is not set in .env (see README.md)")
	}
	if !common.IsHexAddress(quoterRaw) {
		return envConfig{}, fmt.Errorf("invalid UNISWAP_QUOTER_ADDRESS %q: not a hex-encoded address", quoterRaw)
	}
	cfg.uniswapQuoter = common.HexToAddress(quoterRaw)
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

func configPath() string {
	if p := os.Getenv("CONFIG_FILE"); p != "" {
		return p
	}
	return defaultConfigPath
}

func run() error {
	envCfg, err := loadEnv()
	if err != nil {
		return err
	}
	appCfg, err := config.Load(configPath())
	if err != nil {
		return err
	}
	slog.SetDefault(slog.New(newSlogHandler(envCfg.pretty, &slog.HandlerOptions{Level: envCfg.level})))

	uniswapPoolAddr := uniswapv3.PoolAddress(
		uniswapv3.UniswapV3FactoryMainnet,
		uniswapv3.WETH,
		uniswapv3.USDC,
		envCfg.uniswapPoolFee,
	)
	sink := &alert.TextSink{
		Logger:             slog.New(newSlogHandler(envCfg.pretty, nil)),
		Out:                os.Stdout,
		Pretty:             envCfg.pretty,
		UniswapVenue:       venueUniswap,
		UniswapPoolAddress: uniswapPoolAddr,
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	sub := chain.NewSubscriber(envCfg.wsURL, appCfg.Subscriber.ReconnectInitial, appCfg.Subscriber.ReconnectMax)

	binanceSn := buildBinanceSnapshotter(envCfg, appCfg)
	uniswapSn, uniswapClose, err := buildUniswapSnapshotter(envCfg, appCfg)
	if err != nil {
		return err
	}
	defer uniswapClose()

	disp := pipeline.NewDispatcher(map[string]pipeline.Snapshotter{
		venueBinance: binanceSn,
		venueUniswap: uniswapSn,
	}, appCfg.Dispatcher.CallTimeout)
	pf := pathfinder.NewPathfinder()
	ev := arbitrage.NewEvaluator(arbitrage.CostModel{MinNetProfitUSDC: appCfg.ThresholdUSDC})

	subErr := make(chan error, 1)
	go func() { subErr <- sub.Run(ctx) }()
	dispErr := make(chan error, 1)
	go func() { dispErr <- disp.Run(ctx, sub.Events()) }()
	pfErr := make(chan error, 1)
	go func() { pfErr <- pf.Run(ctx, disp.Results()) }()

	slog.Info("arbd starting",
		"venues", []string{venueBinance, venueUniswap},
		"pair", "ETH-USDC",
		"uniswap_pool_fee", envCfg.uniswapPoolFee,
		"threshold_usdc", appCfg.ThresholdUSDC.String(),
	)
	if envCfg.pretty {
		fmt.Fprintf(os.Stdout,
			"arbd: detecting CEX↔DEX arbitrage on ETH-USDC (binance + uniswap v3 fee=%d)\n"+
				"      threshold: net profit > $%s USDC — Ctrl+C to stop\n\n",
			envCfg.uniswapPoolFee, appCfg.ThresholdUSDC.String(),
		)
	}

	consume(pf.Candidates(), ev, sink)

	return awaitShutdown(subErr, dispErr, pfErr)
}

func buildBinanceSnapshotter(envCfg envConfig, appCfg config.Config) pipeline.Snapshotter {
	symbol := binance.SymbolETHUSDC
	symbol.TakerFeeBps = appCfg.Binance.TakerFeeBps
	httpClient := newHTTPClient(venueBinance, appCfg.Binance, appCfg.Retry)
	client := binance.NewClientWithHTTP(envCfg.binanceBaseURL, httpClient)
	return pipeline.NewBinanceSnapshotter(client, symbol, appCfg.TradeSizes)
}

func buildUniswapSnapshotter(envCfg envConfig, appCfg config.Config) (pipeline.Snapshotter, func(), error) {
	pool := uniswapv3.Pool{
		Base:  uniswapv3.WETH,
		Quote: uniswapv3.USDC,
		Fee:   envCfg.uniswapPoolFee,
	}
	httpClient := newHTTPClient(venueUniswap, appCfg.Uniswap, appCfg.Retry)
	client, err := uniswapv3.NewClientWithHTTP(envCfg.ethRPCURL, envCfg.uniswapQuoter, httpClient)
	if err != nil {
		return nil, nil, fmt.Errorf("connect to RPC: %w", err)
	}
	return pipeline.NewUniswapSnapshotter(client, pool, appCfg.TradeSizes), client.Close, nil
}

func newHTTPClient(venue string, vCfg config.VenueConfig, retry config.RetryConfig) *http.Client {
	return resilience.NewHTTPClient(resilience.HTTPClientConfig{
		Retry: &resilience.RetryConfig{
			MaxRetries:  retry.MaxRetries,
			InitialWait: retry.InitialWait,
			MaxWait:     retry.MaxWait,
		},
		Limiter: resilience.NewRateLimiter(venue, vCfg.RateLimitRPS, vCfg.RateLimitBurst),
		Breaker: resilience.NewCircuitBreaker(resilience.BreakerConfig{
			Name:         venue,
			MinRequests:  vCfg.Breaker.MinRequests,
			FailureRatio: vCfg.Breaker.FailureRatio,
			Cooldown:     vCfg.Breaker.Cooldown,
			Interval:     vCfg.Breaker.Interval,
			OnStateChange: func(name, from, to string) {
				slog.Warn("circuit breaker state change", "venue", name, "from", from, "to", to)
			},
		}),
		RequestTimeout: vCfg.RequestTimeout,
		Logger:         slog.Default(),
	})
}

func newSlogHandler(pretty bool, opts *slog.HandlerOptions) slog.Handler {
	if pretty {
		return slog.NewTextHandler(os.Stderr, opts)
	}
	return slog.NewJSONHandler(os.Stderr, opts)
}

func consume(candidates <-chan pathfinder.CandidatePath, ev *arbitrage.Evaluator, sink *alert.TextSink) {
	total, profitable := 0, 0
	for path := range candidates {
		total++
		op := ev.Evaluate(path)
		if !ev.IsProfitable(op) {
			continue
		}
		profitable++
		sink.Emit(op)
	}
	slog.Info("evaluation finished", "total_candidates", total, "profitable", profitable)
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
