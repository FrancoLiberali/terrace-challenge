package uniswapv3

import "github.com/shopspring/decimal"

// Side identifies the direction of a simulated swap, denominated in the
// pool's base asset. The zero value is intentionally invalid so an unset
// Side is detectable.
type Side int

// Buy means "acquire `size` units of Base, paying Quote." Sell means
// "send `size` units of Base, receive Quote." The constants start at 1
// so the zero value of Side reads as UNKNOWN rather than a silent Buy.
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
// (size, side) simulated swap against a Uniswap V3 pool. If Err is non-nil,
// Price is zero and the caller should treat this row as "no value at this
// size and side."
type Quote struct {
	Size  decimal.Decimal
	Side  Side
	Price decimal.Decimal // quote token per unit base
	Err   error
}

// Quotes holds the results of one EffectivePrices call, organized by side.
// Buy[i] and Sell[i] correspond to the i-th element of the input sizes
// slice. Both slices always have len(sizes) entries.
type Quotes struct {
	Buy  []Quote
	Sell []Quote
}
