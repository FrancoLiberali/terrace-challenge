// Package pricing carries the unified types venue adapters use to express
// slippage-aware effective-price quotes. Every adapter (CEX, DEX, future)
// returns the same Quotes shape so the snapshot coordinator, pathfinder,
// and evaluator can treat all venues uniformly.
package pricing

import "github.com/shopspring/decimal"

// Side identifies the direction of a trade, denominated in the pair's base
// asset. The zero value is intentionally invalid so an unset Side is
// detectable.
type Side int

// Buy means "acquire `size` units of Base, paying Quote." Sell means
// "send `size` units of Base, receive Quote." The constants start at 1
// so the zero value of Side reads as UNKNOWN rather than a silent Buy.
const (
	Buy Side = iota + 1
	Sell
)

// sideUnknown is the label String() returns for any Side value that isn't
// one of the named constants (including the zero value).
const sideUnknown = "UNKNOWN"

// String returns a human-readable name for the side.
func (s Side) String() string {
	switch s {
	case Buy:
		return "BUY"
	case Sell:
		return "SELL"
	default:
		return sideUnknown
	}
}

// Quote is the slippage-aware effective per-unit price for a single
// (size, side) query against a venue's current state. If Err is non-nil,
// Price and GasEstimate are zero and the caller should treat this row
// as "no value at this size and side."
//
// GasEstimate is the on-chain gas the trade would consume on this venue.
// On-chain venues (e.g., Uniswap V3 via QuoterV2) populate it; off-chain
// venues (e.g., Binance REST) leave it at the zero value because the
// trade does not touch a blockchain.
type Quote struct {
	Size        decimal.Decimal
	Side        Side
	Price       decimal.Decimal // quote-token per unit base
	GasEstimate uint64          // gas units to execute on the venue's chain; 0 for off-chain venues
	Err         error
}

// Quotes holds the per-side results an adapter returns for one snapshot.
// Buy[i] and Sell[i] correspond to the i-th element of the input sizes
// slice. Both slices always have len(sizes) entries.
type Quotes struct {
	Buy  []Quote
	Sell []Quote
}
