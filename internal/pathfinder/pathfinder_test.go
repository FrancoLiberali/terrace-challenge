package pathfinder

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/FrancoLiberali/terrace-challenge/internal/chain"
	"github.com/FrancoLiberali/terrace-challenge/internal/pipeline"
	"github.com/FrancoLiberali/terrace-challenge/internal/pricing"
)

// dec is a shorthand for tests to build decimal.Decimal from strings.
func dec(s string) decimal.Decimal { return decimal.RequireFromString(s) }

// mkBlock returns a BlockEvent with deterministic timestamp and base fee.
func mkBlock(n uint64) chain.BlockEvent {
	return chain.BlockEvent{
		Number:    n,
		Timestamp: time.Unix(1_700_000_000+int64(n*12), 0).UTC(), //nolint:gosec // small numbers in tests
		BaseFee:   big.NewInt(1),
	}
}

// mkQuotes builds Buy/Sell quotes for sizes 1 and 10 ETH at the given
// per-unit prices. Use it to construct synthetic VenueResults quickly.
func mkQuotes(buy1, sell1, buy10, sell10 string) pricing.Quotes {
	return pricing.Quotes{
		Buy: []pricing.Quote{
			{Size: dec("1"), Side: pricing.Buy, Price: dec(buy1)},
			{Size: dec("10"), Side: pricing.Buy, Price: dec(buy10)},
		},
		Sell: []pricing.Quote{
			{Size: dec("1"), Side: pricing.Sell, Price: dec(sell1)},
			{Size: dec("10"), Side: pricing.Sell, Price: dec(sell10)},
		},
	}
}

// newTestPathfinder wires a Pathfinder with a custom logger so tests
// can inspect log lines (gap warnings, dropped errors).
func newTestPathfinder(logOut io.Writer) *Pathfinder {
	return &Pathfinder{
		venueQuotes: make(map[string]pricing.Quotes),
		out:         make(chan CandidatePath, 16),
		logger:      log.New(logOut, "", 0),
	}
}

// drainQuiet is the inactivity window collectCandidates waits before
// concluding that the pathfinder has nothing more to emit. Small enough
// to keep tests fast, large enough to absorb scheduler jitter.
const drainQuiet = 50 * time.Millisecond

// collectCandidates reads from out until it stops receiving for
// drainQuiet. Used after sending input results to drain whatever the
// pathfinder produced.
func collectCandidates(t *testing.T, out <-chan CandidatePath) []CandidatePath {
	t.Helper()
	var got []CandidatePath
	for {
		select {
		case c, ok := <-out:
			if !ok {
				return got
			}
			got = append(got, c)
		case <-time.After(drainQuiet):
			return got
		}
	}
}

func TestPathfinder_SingleVenueEmitsNoCandidates(t *testing.T) {
	p := newTestPathfinder(io.Discard)

	results := make(chan pipeline.VenueResult, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- p.Run(ctx, results) }()

	results <- pipeline.VenueResult{
		Venue:  "binance",
		Block:  mkBlock(100),
		Quotes: mkQuotes("1680", "1679", "1681", "1678"),
	}

	got := collectCandidates(t, p.Candidates())
	if len(got) != 0 {
		t.Errorf("expected 0 candidates with one venue, got %d: %+v", len(got), got)
	}

	cancel()
	<-runErr
}

func TestPathfinder_TwoVenuesEmitTwoDirectionsPerSize(t *testing.T) {
	p := newTestPathfinder(io.Discard)

	results := make(chan pipeline.VenueResult, 2)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- p.Run(ctx, results) }()

	// Binance: tight book, ~1680/1679.
	results <- pipeline.VenueResult{
		Venue:  "binance",
		Block:  mkBlock(100),
		Quotes: mkQuotes("1680.00", "1679.00", "1680.50", "1678.50"),
	}
	// Uniswap: wider book, ~1685/1675.
	results <- pipeline.VenueResult{
		Venue:  "uniswap",
		Block:  mkBlock(100),
		Quotes: mkQuotes("1685.00", "1675.00", "1686.00", "1674.00"),
	}

	got := collectCandidates(t, p.Candidates())
	if len(got) != 4 {
		t.Fatalf("expected 4 candidates (2 dirs × 2 sizes), got %d: %+v", len(got), got)
	}

	// Build a lookup by (size, buy, sell) for easy assertion.
	by := map[string]CandidatePath{}
	for _, c := range got {
		by[c.Size.String()+"|"+c.BuyVenue+"|"+c.SellVenue] = c
	}

	// Direction A at size=1: buy binance (ask=1680), sell uniswap (bid=1675) — loss
	a1 := by["1|binance|uniswap"]
	if !a1.BuyPrice.Equal(dec("1680.00")) || !a1.SellPrice.Equal(dec("1675.00")) {
		t.Errorf("1 ETH binance→uniswap: got buy=%s sell=%s, want 1680/1675", a1.BuyPrice, a1.SellPrice)
	}
	// Direction B at size=1: buy uniswap (ask=1685), sell binance (bid=1679) — loss
	b1 := by["1|uniswap|binance"]
	if !b1.BuyPrice.Equal(dec("1685.00")) || !b1.SellPrice.Equal(dec("1679.00")) {
		t.Errorf("1 ETH uniswap→binance: got buy=%s sell=%s, want 1685/1679", b1.BuyPrice, b1.SellPrice)
	}
	// Sizes 10 mirror these with the wider numbers.
	a10 := by["10|binance|uniswap"]
	if !a10.BuyPrice.Equal(dec("1680.50")) || !a10.SellPrice.Equal(dec("1674.00")) {
		t.Errorf("10 ETH binance→uniswap: got buy=%s sell=%s, want 1680.50/1674", a10.BuyPrice, a10.SellPrice)
	}

	cancel()
	<-runErr
}

func TestPathfinder_StaleBlockDropped(t *testing.T) {
	var logBuf bytes.Buffer
	p := newTestPathfinder(&logBuf)

	results := make(chan pipeline.VenueResult, 3)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- p.Run(ctx, results) }()

	// Establish block 100 as the current.
	results <- pipeline.VenueResult{Venue: "binance", Block: mkBlock(100), Quotes: mkQuotes("1680", "1679", "1680", "1679")}
	// A late result for block 99 must be dropped, NOT paired with the
	// stored binance@100 quotes.
	results <- pipeline.VenueResult{Venue: "uniswap", Block: mkBlock(99), Quotes: mkQuotes("1685", "1675", "1685", "1675")}

	got := collectCandidates(t, p.Candidates())
	if len(got) != 0 {
		t.Errorf("expected 0 candidates after stale result, got %d: %+v", len(got), got)
	}

	// Drain the Run goroutine before touching logBuf so the race
	// detector doesn't flag the bytes.Buffer read against the writes
	// happening inside the logger.
	cancel()
	<-runErr

	if !strings.Contains(logBuf.String(), "stale result for block 99") {
		t.Errorf("expected stale-drop log line, got: %q", logBuf.String())
	}
}

func TestPathfinder_NewerBlockEvictsPreviousPartialState(t *testing.T) {
	p := newTestPathfinder(io.Discard)

	results := make(chan pipeline.VenueResult, 3)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- p.Run(ctx, results) }()

	// Block 100 from binance only (no pairing yet).
	results <- pipeline.VenueResult{Venue: "binance", Block: mkBlock(100), Quotes: mkQuotes("1680", "1679", "1680", "1679")}

	// Block 101 from binance: this evicts the 100-partial-state.
	results <- pipeline.VenueResult{Venue: "binance", Block: mkBlock(101), Quotes: mkQuotes("1690", "1689", "1690", "1689")}

	// Block 101 from uniswap: must pair with binance@101 (the freshest),
	// NOT with binance@100 (which would be wrong because 100 was evicted).
	results <- pipeline.VenueResult{Venue: "uniswap", Block: mkBlock(101), Quotes: mkQuotes("1695", "1685", "1695", "1685")}

	got := collectCandidates(t, p.Candidates())
	if len(got) != 4 {
		t.Fatalf("expected 4 candidates from block 101 pairs, got %d: %+v", len(got), got)
	}
	for _, c := range got {
		if c.Block.Number != 101 {
			t.Errorf("candidate references block %d, want 101 (block 100 should have been evicted)", c.Block.Number)
		}
	}

	cancel()
	<-runErr
}

func TestPathfinder_VenueErrorSkipsPairing(t *testing.T) {
	var logBuf bytes.Buffer
	p := newTestPathfinder(&logBuf)

	results := make(chan pipeline.VenueResult, 3)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- p.Run(ctx, results) }()

	// First venue reports cleanly.
	results <- pipeline.VenueResult{Venue: "binance", Block: mkBlock(100), Quotes: mkQuotes("1680", "1679", "1680", "1679")}
	// Second venue has a top-level error: no quotes to pair with.
	results <- pipeline.VenueResult{Venue: "uniswap", Block: mkBlock(100), Err: errors.New("rpc timeout")}

	got := collectCandidates(t, p.Candidates())
	if len(got) != 0 {
		t.Errorf("expected 0 candidates when 2nd venue errored, got %d: %+v", len(got), got)
	}

	// See the comment in TestPathfinder_StaleBlockDropped for why we
	// drain the Run goroutine before reading logBuf.
	cancel()
	<-runErr

	if !strings.Contains(logBuf.String(), "uniswap") || !strings.Contains(logBuf.String(), "rpc timeout") {
		t.Errorf("expected log mentioning uniswap rpc timeout, got: %q", logBuf.String())
	}
}

func TestPathfinder_PerSizeErrorSkipsOnlyAffectedDirection(t *testing.T) {
	p := newTestPathfinder(io.Discard)

	results := make(chan pipeline.VenueResult, 2)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- p.Run(ctx, results) }()

	// Binance: 1 ETH BUY has error (insufficient depth for the ask side),
	// everything else clean.
	bin := mkQuotes("1680", "1679", "1680", "1679")
	bin.Buy[0].Err = errors.New("insufficient depth")
	bin.Buy[0].Price = decimal.Zero
	results <- pipeline.VenueResult{Venue: "binance", Block: mkBlock(100), Quotes: bin}

	// Uniswap clean.
	results <- pipeline.VenueResult{Venue: "uniswap", Block: mkBlock(100), Quotes: mkQuotes("1685", "1675", "1685", "1675")}

	got := collectCandidates(t, p.Candidates())
	// Expected: 3 candidates.
	//   size=1:  direction A (buy=binance) DROPPED because binance.Buy[0] is errored.
	//   size=1:  direction B (buy=uniswap, sell=binance.Sell[0]) clean → 1 candidate.
	//   size=10: both directions clean → 2 candidates.
	if len(got) != 3 {
		t.Fatalf("expected 3 candidates (1 size=1 + 2 size=10), got %d: %+v", len(got), got)
	}
	for _, c := range got {
		if c.Size.Equal(dec("1")) && c.BuyVenue == "binance" {
			t.Errorf("size=1 binance→uniswap should have been dropped (binance.Buy[0] errored), got %+v", c)
		}
	}

	cancel()
	<-runErr
}

func TestPathfinder_StopsOnContextCancel(t *testing.T) {
	p := newTestPathfinder(io.Discard)
	results := make(chan pipeline.VenueResult)

	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- p.Run(ctx, results) }()

	cancel()

	select {
	case err := <-runErr:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Run: got %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}
	if _, ok := <-p.Candidates(); ok {
		t.Error("Candidates channel should be closed after Run returns")
	}
}

func TestPathfinder_StopsOnResultsClose(t *testing.T) {
	p := newTestPathfinder(io.Discard)
	results := make(chan pipeline.VenueResult)

	ctx := t.Context()
	runErr := make(chan error, 1)
	go func() { runErr <- p.Run(ctx, results) }()

	close(results)

	select {
	case err := <-runErr:
		if err != nil {
			t.Errorf("Run after results close: got %v, want nil", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not return after results close")
	}
	if _, ok := <-p.Candidates(); ok {
		t.Error("Candidates channel should be closed after Run returns")
	}
}
