// Command probe-chain is a thin CLI wrapper around the chain package.
// It subscribes to Ethereum's newHeads stream over WebSocket and prints
// one line per new block (number, header timestamp, EIP-1559 base fee).
// The probe stays in the repo as an ongoing diagnostic tool — see plan.md.
//
// To exercise the reconnect path, run the probe and toggle Wi-Fi or
// briefly block the RPC endpoint with the firewall. The subscriber
// should log "subscription dropped … reconnecting" and resume cleanly
// when the network comes back, including a "potential gap" line if
// blocks were missed during the outage.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math/big"
	"os"
	"os/signal"
	"syscall"

	"github.com/joho/godotenv"

	"github.com/FrancoLiberali/terrace-challenge/internal/chain"
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("probe-chain: %v", err)
	}
}

func run() error {
	if err := godotenv.Load(); err != nil {
		return fmt.Errorf("load .env: %w", err)
	}
	wsURL := os.Getenv("ETH_RPC_WS_URL")
	if wsURL == "" {
		return errors.New("ETH_RPC_WS_URL is not set in .env (see README.md)")
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	sub := chain.NewSubscriber(wsURL)

	runErr := make(chan error, 1)
	go func() { runErr <- sub.Run(ctx) }()

	fmt.Fprintln(os.Stdout, "probe-chain: subscribed to newHeads — Ctrl+C to stop")
	for ev := range sub.Events() {
		fmt.Fprintf(os.Stdout, "block %-9d  ts=%s  baseFee=%s gwei\n",
			ev.Number,
			ev.Timestamp.Format("2006-01-02 15:04:05 MST"),
			formatGwei(ev.BaseFee),
		)
	}

	if err := <-runErr; err != nil && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("subscription: %w", err)
	}
	return nil
}

// formatGwei prints a wei amount as a fixed-point gwei string (3 decimal
// places). Returns "n/a" for nil — pre-London chains have no BaseFee
// field, though that situation doesn't apply to mainnet.
func formatGwei(wei *big.Int) string {
	if wei == nil {
		return "n/a"
	}
	const oneGweiInWei = 1_000_000_000
	// Truncate to 3 decimal places to keep the line tidy.
	gwei := new(big.Int).Mul(wei, big.NewInt(1000))
	gwei.Quo(gwei, big.NewInt(oneGweiInWei))
	whole := new(big.Int).Quo(gwei, big.NewInt(1000))
	frac := new(big.Int).Mod(gwei, big.NewInt(1000))
	return fmt.Sprintf("%s.%03d", whole.String(), frac.Int64())
}
