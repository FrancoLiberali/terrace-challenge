package pipeline

import (
	"context"
	"errors"
	"math/big"
	"sync"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/FrancoLiberali/terrace-challenge/internal/chain"
	"github.com/FrancoLiberali/terrace-challenge/internal/pricing"
)

// fakeSnapshotter is the in-memory Snapshotter the tests drive. It records
// every call, optionally blocks on a release channel so a test can hold a
// snapshot mid-flight to observe supersession, and returns canned data.
type fakeSnapshotter struct {
	mu     sync.Mutex
	calls  []chain.BlockEvent
	quotes pricing.Quotes
	err    error
	// block, if non-nil, makes Snapshot wait on it (or on ctx.Done)
	// before returning. Lets tests synchronize on "snapshot is now
	// running" without sleeping.
	block chan struct{}
	// started, if non-nil, has one value pushed on each call. Tests
	// use it to wait for a snapshot to start before sending a
	// superseding block.
	started chan struct{}
}

func (f *fakeSnapshotter) Snapshot(ctx context.Context, ev chain.BlockEvent) (pricing.Quotes, error) {
	f.mu.Lock()
	f.calls = append(f.calls, ev)
	startCh := f.started
	blockCh := f.block
	f.mu.Unlock()

	if startCh != nil {
		select {
		case startCh <- struct{}{}:
		default:
		}
	}

	if blockCh != nil {
		select {
		case <-blockCh:
		case <-ctx.Done():
			return pricing.Quotes{}, ctx.Err()
		}
	}
	return f.quotes, f.err
}

// newTestDispatcher wires a Dispatcher with a tight per-block timeout so
// the test suite runs fast even when ctx-deadline paths are exercised.
func newTestDispatcher(venues map[string]Snapshotter) *Dispatcher {
	return &Dispatcher{
		venues:  venues,
		timeout: 500 * time.Millisecond,
		out:     make(chan VenueResult, 4), // small buffer to decouple test reads
	}
}

func mkBlock(n uint64) chain.BlockEvent {
	return chain.BlockEvent{
		Number:    n,
		Timestamp: time.Unix(1_700_000_000+int64(n*12), 0).UTC(), //nolint:gosec // block numbers in tests are small
		BaseFee:   big.NewInt(1),
	}
}

func TestDispatcher_StreamsVenueResults(t *testing.T) {
	cexQ := pricing.Quotes{Buy: []pricing.Quote{{Size: decimal.NewFromInt(1), Side: pricing.Buy, Price: decimal.NewFromInt(1680)}}}
	dexQ := pricing.Quotes{Buy: []pricing.Quote{{Size: decimal.NewFromInt(1), Side: pricing.Buy, Price: decimal.NewFromInt(1683)}}}

	cex := &fakeSnapshotter{quotes: cexQ}
	dex := &fakeSnapshotter{quotes: dexQ}
	disp := newTestDispatcher(map[string]Snapshotter{"binance": cex, "uniswap": dex})

	events := make(chan chain.BlockEvent, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runErr := make(chan error, 1)
	go func() { runErr <- disp.Run(ctx, events) }()

	events <- mkBlock(100)

	// Both venues should emit their own VenueResult — order is not
	// guaranteed, so collect both then assert by name.
	seen := map[string]VenueResult{}
	for range 2 {
		select {
		case r := <-disp.Results():
			seen[r.Venue] = r
		case <-time.After(time.Second):
			t.Fatalf("only got %d results, want 2", len(seen))
		}
	}

	cexR, ok := seen["binance"]
	if !ok {
		t.Fatal("no binance result")
	}
	if cexR.Block.Number != 100 || cexR.Err != nil {
		t.Errorf("binance result: %+v", cexR)
	}
	if len(cexR.Quotes.Buy) != 1 || !cexR.Quotes.Buy[0].Price.Equal(decimal.NewFromInt(1680)) {
		t.Errorf("binance Quotes.Buy: got %+v, want price 1680", cexR.Quotes.Buy)
	}

	dexR, ok := seen["uniswap"]
	if !ok {
		t.Fatal("no uniswap result")
	}
	if dexR.Block.Number != 100 || dexR.Err != nil {
		t.Errorf("uniswap result: %+v", dexR)
	}
	if len(dexR.Quotes.Buy) != 1 || !dexR.Quotes.Buy[0].Price.Equal(decimal.NewFromInt(1683)) {
		t.Errorf("uniswap Quotes.Buy: got %+v, want price 1683", dexR.Quotes.Buy)
	}

	cancel()
	<-runErr
}

func TestDispatcher_EmitsErrorsAsResults(t *testing.T) {
	cexErr := errors.New("binance 503")
	dexErr := errors.New("rpc rate limited")
	cex := &fakeSnapshotter{err: cexErr}
	dex := &fakeSnapshotter{err: dexErr}
	disp := newTestDispatcher(map[string]Snapshotter{"binance": cex, "uniswap": dex})

	events := make(chan chain.BlockEvent, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- disp.Run(ctx, events) }()

	events <- mkBlock(200)

	seen := map[string]error{}
	for range 2 {
		select {
		case r := <-disp.Results():
			seen[r.Venue] = r.Err
		case <-time.After(time.Second):
			t.Fatalf("only got %d results, want 2", len(seen))
		}
	}
	if !errors.Is(seen["binance"], cexErr) {
		t.Errorf("binance Err: got %v, want %v", seen["binance"], cexErr)
	}
	if !errors.Is(seen["uniswap"], dexErr) {
		t.Errorf("uniswap Err: got %v, want %v", seen["uniswap"], dexErr)
	}

	cancel()
	<-runErr
}

func TestDispatcher_FastVenueDoesNotWaitForSlowOne(t *testing.T) {
	// Demonstrates the headline benefit of streaming: a fast venue's
	// result is observable immediately, even though a slow venue for
	// the same block is still pending. With a synchronous Coordinator
	// the consumer would have to wait for both.
	fastQ := pricing.Quotes{Buy: []pricing.Quote{{Size: decimal.NewFromInt(1), Side: pricing.Buy, Price: decimal.NewFromInt(1680)}}}
	slowQ := pricing.Quotes{Buy: []pricing.Quote{{Size: decimal.NewFromInt(1), Side: pricing.Buy, Price: decimal.NewFromInt(1683)}}}

	hold := make(chan struct{})
	fast := &fakeSnapshotter{quotes: fastQ}
	slow := &fakeSnapshotter{quotes: slowQ, block: hold}
	disp := newTestDispatcher(map[string]Snapshotter{"fast": fast, "slow": slow})

	events := make(chan chain.BlockEvent, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- disp.Run(ctx, events) }()

	events <- mkBlock(300)

	// fast's result must arrive while slow is still hanging.
	select {
	case r := <-disp.Results():
		if r.Venue != "fast" {
			t.Errorf("first result: got venue %q, want %q (slow venue should still be hanging)", r.Venue, "fast")
		}
	case <-time.After(time.Second):
		t.Fatal("fast venue's result never arrived")
	}

	// Release the slow one; its result follows.
	close(hold)
	select {
	case r := <-disp.Results():
		if r.Venue != "slow" {
			t.Errorf("second result: got venue %q, want %q", r.Venue, "slow")
		}
	case <-time.After(time.Second):
		t.Fatal("slow venue's result never arrived")
	}

	cancel()
	<-runErr
}

func TestDispatcher_EmitsForBothBlocksWhenSecondArrivesMidFlight(t *testing.T) {
	// Documents the streaming/broker-shaped behavior: when block 401
	// arrives while block 400 is still in flight, BOTH blocks' results
	// are emitted. The Dispatcher does not cancel in-flight work — the
	// publisher of a broker topic cannot reach into a subscriber's
	// process to cancel it, and we mirror that constraint here.
	//
	// Freshness ("the latest block wins") is the Pathfinder's job; it
	// will discard results whose block number is older than the
	// freshest one it has seen. See architecture.md decision 2.
	hold := make(chan struct{})
	cexStarted := make(chan struct{}, 8)
	dexStarted := make(chan struct{}, 8)

	cexQ := pricing.Quotes{Buy: []pricing.Quote{{Size: decimal.NewFromInt(1), Side: pricing.Buy, Price: decimal.NewFromInt(1680)}}}
	dexQ := pricing.Quotes{Buy: []pricing.Quote{{Size: decimal.NewFromInt(1), Side: pricing.Buy, Price: decimal.NewFromInt(1683)}}}

	cex := &fakeSnapshotter{quotes: cexQ, block: hold, started: cexStarted}
	dex := &fakeSnapshotter{quotes: dexQ, block: hold, started: dexStarted}
	disp := newTestDispatcher(map[string]Snapshotter{"binance": cex, "uniswap": dex})

	events := make(chan chain.BlockEvent)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- disp.Run(ctx, events) }()

	// Block 400 fires; wait for both venues to enter Snapshot.
	events <- mkBlock(400)
	<-cexStarted
	<-dexStarted

	// Block 401 arrives while block 400 is still hanging. The
	// Dispatcher does NOT cancel block 400 — it fans out block 401's
	// triggers in parallel.
	events <- mkBlock(401)
	<-cexStarted
	<-dexStarted

	// Release the hold; both blocks' calls now return their canned
	// quotes (the hold was shared) and emit independently.
	close(hold)

	got := map[uint64]int{}
	deadline := time.After(time.Second)
	for got[400]+got[401] < 4 {
		select {
		case r := <-disp.Results():
			got[r.Block.Number]++
		case <-deadline:
			t.Fatalf("got %d results in time, want 4 (2 per block): %+v", got[400]+got[401], got)
		}
	}

	if got[400] != 2 {
		t.Errorf("got %d result(s) for block 400, want 2 (Dispatcher does not cancel on supersession)", got[400])
	}
	if got[401] != 2 {
		t.Errorf("got %d result(s) for block 401, want 2 (one per venue)", got[401])
	}

	cancel()
	<-runErr
}

func TestDispatcher_StopsOnContextCancel(t *testing.T) {
	cex := &fakeSnapshotter{}
	dex := &fakeSnapshotter{}
	disp := newTestDispatcher(map[string]Snapshotter{"binance": cex, "uniswap": dex})

	events := make(chan chain.BlockEvent)
	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- disp.Run(ctx, events) }()

	cancel()

	select {
	case err := <-runErr:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Run: got %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}
	if _, ok := <-disp.Results(); ok {
		t.Error("Results channel should be closed after Run returns")
	}
}

func TestDispatcher_StopsOnEventsClose(t *testing.T) {
	cex := &fakeSnapshotter{}
	dex := &fakeSnapshotter{}
	disp := newTestDispatcher(map[string]Snapshotter{"binance": cex, "uniswap": dex})

	events := make(chan chain.BlockEvent)
	ctx := t.Context()
	runErr := make(chan error, 1)
	go func() { runErr <- disp.Run(ctx, events) }()

	close(events)

	select {
	case err := <-runErr:
		if err != nil {
			t.Errorf("Run after events close: got %v, want nil", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not return after events close")
	}
	if _, ok := <-disp.Results(); ok {
		t.Error("Results channel should be closed after Run returns")
	}
}
