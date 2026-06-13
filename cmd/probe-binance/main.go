// Command probe-binance is a thin CLI wrapper around the binance package.
// It fetches the slippage-aware effective ETH-USDC prices from Binance for a
// fixed set of trade sizes and prints them. The probe stays in the repo as
// an ongoing diagnostic tool — see plan.md.
package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"time"

	"github.com/FrancoLiberali/terrace-challange/internal/cex/binance"
	"github.com/shopspring/decimal"
)

const requestTimeout = 10 * time.Second

var tradeSizes = []decimal.Decimal{
	decimal.NewFromInt(1),
	decimal.NewFromInt(10),
	decimal.NewFromInt(100),
}

func main() {
	if err := run(); err != nil {
		log.Fatalf("probe-binance: %v", err)
	}
}

func run() error {
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()

	client := binance.NewClient(binance.DefaultBaseURL)
	snap, err := client.EffectivePrices(ctx, binance.SymbolETHUSDC, tradeSizes)
	if err != nil {
		return fmt.Errorf("fetch effective prices: %w", err)
	}
	printSnapshot(os.Stdout, tradeSizes, snap)
	return nil
}

// printSnapshot renders the raw top-of-book and the per-size effective prices.
// The Quotes slice is laid out as interleaved Buy/Sell rows by size (the order
// EffectivePrices guarantees), so a direct indexed loop is enough.
func printSnapshot(w io.Writer, sizes []decimal.Decimal, snap binance.Snapshot) {
	fmt.Fprintln(w, "Binance ETH-USDC")
	fmt.Fprintf(w, "  top of book: bid $%s / ask $%s (spread $%s)\n",
		snap.BestBid.StringFixed(2),
		snap.BestAsk.StringFixed(2),
		snap.BestAsk.Sub(snap.BestBid).StringFixed(2),
	)
	fmt.Fprintln(w, "  effective prices (slippage-aware):")
	fmt.Fprintf(w, "    %-14s   %-22s   %-22s\n", "Size", "BUY (eat asks)", "SELL (eat bids)")
	for i, sz := range sizes {
		fmt.Fprintf(w, "    %-14s   %-22s   %-22s\n",
			sz.String()+" ETH",
			formatQuote(snap.Quotes[2*i]),
			formatQuote(snap.Quotes[2*i+1]),
		)
	}
}

func formatQuote(q binance.Quote) string {
	if q.Err != nil {
		return "insufficient depth"
	}
	return "$" + q.Price.StringFixed(4) + "/ETH"
}
