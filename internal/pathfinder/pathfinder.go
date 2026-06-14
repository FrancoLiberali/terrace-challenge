// Package pathfinder consumes the per-venue VenueResult stream produced
// by internal/pipeline and emits CandidatePaths: fully-specified
// prospective arbitrage trades (size, buy venue, sell venue, observed
// per-unit prices). It owns per-block correlation — when at least two
// venues have reported for the same block, the Pathfinder enumerates
// the cross-venue pairs at every size.
//
// The Pathfinder is also where freshness filtering lives. Per
// architecture.md decision 2, the Dispatcher does not cancel or filter
// stale-block results; the Pathfinder tracks the freshest block it has
// seen and drops VenueResults whose block is older. Results for a
// newer block evict the partial state of the previous one — we only
// correlate within the freshest block.
package pathfinder

import (
	"context"
	"log/slog"

	"github.com/shopspring/decimal"

	"github.com/FrancoLiberali/terrace-challenge/internal/chain"
	"github.com/FrancoLiberali/terrace-challenge/internal/pipeline"
	"github.com/FrancoLiberali/terrace-challenge/internal/pricing"
)

// CandidatePath is one prospective arbitrage trade: at this block, buy
// `Size` units of the base asset on BuyVenue at BuyPrice per unit, and
// sell them on SellVenue at SellPrice per unit. The Evaluator (next
// stage) applies the cost model to decide whether the candidate is
// actually profitable.
//
// BuyPrice is the venue's effective ask (what you pay to acquire Size).
// SellPrice is the venue's effective bid (what you receive selling Size).
// Both are slippage-aware — the adapters do the orderbook-walk math
// before anything reaches here.
//
// GasEstimate is the total on-chain gas the path would consume across
// both legs (the sum of the per-quote gas estimates from each leg).
// For our CEX-DEX setup exactly one leg is on-chain, so the value is
// effectively the DEX side's gas estimate. For a DEX-only path with
// two on-chain swaps it would be the sum.
type CandidatePath struct {
	Block       chain.BlockEvent
	Size        decimal.Decimal
	BuyVenue    string
	SellVenue   string
	BuyPrice    decimal.Decimal
	SellPrice   decimal.Decimal
	GasEstimate uint64
}

// Pathfinder correlates VenueResults by block and emits one CandidatePath
// per (size, direction) for each pair of venues that have reported.
type Pathfinder struct {
	// currentBlock is the freshest block number we have started
	// pairing for. Older results are dropped; newer results evict
	// venueQuotes.
	currentBlock uint64
	venueQuotes  map[string]pricing.Quotes

	out chan CandidatePath
}

// NewPathfinder returns a Pathfinder ready to be driven by Run.
// Call Candidates() once for the output channel.
func NewPathfinder() *Pathfinder {
	return &Pathfinder{
		venueQuotes: make(map[string]pricing.Quotes),
		out:         make(chan CandidatePath),
	}
}

// Candidates returns the channel CandidatePaths are emitted on. The
// channel is closed when Run returns.
func (p *Pathfinder) Candidates() <-chan CandidatePath { return p.out }

// Run consumes from the results channel and emits CandidatePaths until
// ctx is cancelled or results is closed by its producer. Each
// CandidatePath is emitted as soon as its (size, direction) becomes
// pairable — i.e., the moment the second venue for the current block
// reports clean quotes for that size.
func (p *Pathfinder) Run(ctx context.Context, results <-chan pipeline.VenueResult) error {
	defer close(p.out)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case r, ok := <-results:
			if !ok {
				return nil
			}
			p.handle(ctx, r)
		}
	}
}

// handle integrates one VenueResult into the per-block state and emits
// any newly-pairable CandidatePaths.
func (p *Pathfinder) handle(ctx context.Context, r pipeline.VenueResult) {
	n := r.Block.Number

	// Freshness filter: drop results whose block is older than the
	// freshest we have started pairing for.
	if n < p.currentBlock {
		slog.Warn("dropping stale result", "block", n, "current", p.currentBlock, "venue", r.Venue)
		return
	}
	// A fresher block evicts the previous block's partial state.
	// We only correlate within the freshest block.
	if n > p.currentBlock {
		p.currentBlock = n
		clear(p.venueQuotes)
	}

	// Venue-level errors (HTTP, RPC, timeout): record nothing, log,
	// move on. Other venues for the same block can still pair among
	// themselves.
	if r.Err != nil {
		slog.Warn("skipping venue due to error", "venue", r.Venue, "block", n, "err", r.Err)
		return
	}

	// Pair this venue with every venue we have already stored for the
	// current block.
	for otherVenue, otherQuotes := range p.venueQuotes {
		p.emitPairs(ctx, r, otherVenue, otherQuotes)
	}

	// Record this venue so subsequent venues can pair with it.
	p.venueQuotes[r.Venue] = r.Quotes
}

// emitPairs enumerates both arbitrage directions at every size shared
// between the two venues, emitting a CandidatePath per (size, direction)
// where both legs have clean quotes.
//
// Within a single venue, the adapter writes Buy[i] and Sell[i] from the
// same input size, so Buy[i].Size == Sell[i].Size by construction.
// Across venues that alignment is a system invariant — both
// snapshotters are configured with the same size set at startup — not
// a contract on pricing.Quotes itself, so we verify it explicitly:
// length mismatch skips the entire pair, per-index size mismatch skips
// that index. Either is logged. Silent truncation (the old min-of-len
// shortcut) would have masked mis-paired candidates.
func (p *Pathfinder) emitPairs(ctx context.Context, r pipeline.VenueResult, otherVenue string, otherQuotes pricing.Quotes) {
	if len(r.Quotes.Buy) != len(otherQuotes.Buy) {
		slog.Error("size-set length mismatch — skipping pair",
			"block", r.Block.Number,
			"venue_a", r.Venue, "len_a", len(r.Quotes.Buy),
			"venue_b", otherVenue, "len_b", len(otherQuotes.Buy))
		return
	}
	for i := range r.Quotes.Buy {
		size := r.Quotes.Buy[i].Size
		if !size.Equal(otherQuotes.Buy[i].Size) {
			slog.Error("size mismatch — skipping index",
				"index", i, "block", r.Block.Number,
				"venue_a", r.Venue, "size_a", size,
				"venue_b", otherVenue, "size_b", otherQuotes.Buy[i].Size)
			continue
		}

		// Direction A: buy on r.Venue, sell on otherVenue.
		// Requires a clean ask on r.Venue AND a clean bid on otherVenue.
		if r.Quotes.Buy[i].Err == nil && otherQuotes.Sell[i].Err == nil {
			p.emit(ctx, CandidatePath{
				Block:       r.Block,
				Size:        size,
				BuyVenue:    r.Venue,
				SellVenue:   otherVenue,
				BuyPrice:    r.Quotes.Buy[i].Price,
				SellPrice:   otherQuotes.Sell[i].Price,
				GasEstimate: r.Quotes.Buy[i].GasEstimate + otherQuotes.Sell[i].GasEstimate,
			})
		}
		// Direction B: buy on otherVenue, sell on r.Venue.
		if otherQuotes.Buy[i].Err == nil && r.Quotes.Sell[i].Err == nil {
			p.emit(ctx, CandidatePath{
				Block:       r.Block,
				Size:        size,
				BuyVenue:    otherVenue,
				SellVenue:   r.Venue,
				BuyPrice:    otherQuotes.Buy[i].Price,
				SellPrice:   r.Quotes.Sell[i].Price,
				GasEstimate: otherQuotes.Buy[i].GasEstimate + r.Quotes.Sell[i].GasEstimate,
			})
		}
	}
}

// emit sends c on p.out, with ctx-Done as the shutdown escape. Same
// pattern as chain.Subscriber.emit and pipeline.Dispatcher's send path.
func (p *Pathfinder) emit(ctx context.Context, c CandidatePath) {
	select {
	case p.out <- c:
	case <-ctx.Done():
	}
}
