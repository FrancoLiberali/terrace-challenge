// Package binance integrates with Binance's public REST API for the ETH-USDC
// orderbook. It exposes a single high-level operation — fetching the
// slippage-aware effective per-unit price for a list of trade sizes — and
// hides the orderbook representation behind that interface. The package
// carries no resilience concerns (rate limiting, circuit breaking, retries):
// those are applied as wrappers around it.
package binance

import (
	"errors"
	"fmt"

	"github.com/shopspring/decimal"
)

// Side identifies which side of an orderbook a trade consumes.
// The zero value is intentionally invalid so an unset Side is detectable.
type Side int

// Buy consumes the ask side (lifts offers); Sell consumes the bid side
// (hits bids). The constants start at 1 so the zero value of Side reads as
// UNKNOWN rather than a silent Buy.
const (
	Buy Side = iota + 1
	Sell
)

// String returns a human-readable name for the side.
func (s Side) String() string {
	switch s {
	case Buy:
		return "BUY"
	case Sell:
		return "SELL"
	default:
		return "UNKNOWN"
	}
}

// Quote is the slippage-aware effective per-unit price for a single
// (size, side) query against an orderbook snapshot. If Err is non-nil,
// Price is zero and the caller should treat this row as "no value at this
// size and side."
type Quote struct {
	Size  decimal.Decimal
	Side  Side
	Price decimal.Decimal // quote-token per unit base
	Err   error
}

// Quotes holds the slippage-aware results from one EffectivePrices call,
// organized by side. Buy[i] and Sell[i] correspond to the i-th element of
// the input sizes slice — Buy[i] consumes the ask side at sizes[i],
// Sell[i] consumes the bid side at sizes[i]. Both slices always have
// len(sizes) entries.
type Quotes struct {
	Buy  []Quote
	Sell []Quote
}

// ErrInsufficientDepth is returned (via Quote.Err) when the orderbook does
// not contain enough liquidity to fill the requested trade size on a given side.
var ErrInsufficientDepth = errors.New("insufficient orderbook depth")

// level is one price/size point in an orderbook. Internal representation.
type level struct {
	price decimal.Decimal
	size  decimal.Decimal
}

// walkOrderbook computes the slippage-aware effective price for consuming
// `size` units of the base token against the given levels. Levels must
// already be sorted in the direction trades consume them:
//
//   - For a BUY (eating asks): ascending by price, best ask first.
//   - For a SELL (eating bids): descending by price, best bid first.
//
// Returns the volume-weighted-average price per unit and the total quote-
// token cost. ErrInsufficientDepth is returned if `size` exceeds total depth.
func walkOrderbook(levels []level, size decimal.Decimal) (effectivePrice, totalCost decimal.Decimal, err error) {
	if !size.IsPositive() {
		return decimal.Zero, decimal.Zero, fmt.Errorf("size must be positive, got %s", size)
	}
	remaining := size
	totalCost = decimal.Zero
	for _, lv := range levels {
		if !remaining.IsPositive() {
			break
		}
		take := decimal.Min(remaining, lv.size)
		totalCost = totalCost.Add(take.Mul(lv.price))
		remaining = remaining.Sub(take)
	}
	if remaining.IsPositive() {
		return decimal.Zero, decimal.Zero, ErrInsufficientDepth
	}
	effectivePrice = totalCost.Div(size)
	return effectivePrice, totalCost, nil
}
