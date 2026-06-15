package chain

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"math/big"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/core/types"
)

// captureSlog redirects slog.Default to buf for the duration of t and
// restores the previous logger when the test ends. Used by tests that
// inspect log output via substring assertions.
func captureSlog(t *testing.T, buf *bytes.Buffer) {
	t.Helper()
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
}

// fakeSubscription is the in-memory sub a test feeds into the Subscriber.
// Headers() and Err() drive what the stream loop sees; Close records that
// the subscriber tore the connection down cleanly on the way out.
type fakeSubscription struct {
	headers chan *types.Header
	errs    chan error
	closed  chan struct{} // closed when Close() is invoked
}

func newFakeSubscription() *fakeSubscription {
	return &fakeSubscription{
		headers: make(chan *types.Header, 4),
		errs:    make(chan error, 1),
		closed:  make(chan struct{}),
	}
}

func (f *fakeSubscription) Headers() <-chan *types.Header { return f.headers }
func (f *fakeSubscription) Err() <-chan error             { return f.errs }
func (f *fakeSubscription) Close() {
	select {
	case <-f.closed:
	default:
		close(f.closed)
	}
}

// fakeDialer hands out a scripted sequence of dial results. Once the plan
// is exhausted, further Dial calls block on ctx so the Subscriber sits
// idle waiting for shutdown via context cancellation.
type fakeDialer struct {
	mu        sync.Mutex
	plan      []dialStep // explicit plan: each entry tells Dial what to do for that call
	planIndex int
	dialCount int
}

// dialStep is a single instruction in the dial plan. exactly one of sub or
// err is non-nil; a nil-nil step means "block until ctx is cancelled."
type dialStep struct {
	sub *fakeSubscription
	err error
}

func (f *fakeDialer) Dial(ctx context.Context) (subscription, error) {
	f.mu.Lock()
	f.dialCount++
	if f.planIndex >= len(f.plan) {
		f.mu.Unlock()
		// No further plan steps — block until ctx fires so the test can
		// shut down cleanly without the Subscriber spinning.
		<-ctx.Done()
		return nil, ctx.Err()
	}
	step := f.plan[f.planIndex]
	f.planIndex++
	f.mu.Unlock()
	if step.err != nil {
		return nil, step.err
	}
	return step.sub, nil
}

func (f *fakeDialer) dials() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.dialCount
}

// newTestSubscriber wires a Subscriber with a fake dialer and a tiny
// reconnect delay so the dial-retry path runs instantly.
func newTestSubscriber(d *fakeDialer) *Subscriber {
	return &Subscriber{
		dial:                  d,
		out:                   make(chan BlockEvent, 8),
		reconnectInitialDelay: 1 * time.Millisecond,
		reconnectMaxDelay:     10 * time.Millisecond,
	}
}

func header(n uint64, ts uint64, baseFee int64) *types.Header {
	return &types.Header{
		Number:  new(big.Int).SetUint64(n),
		Time:    ts,
		BaseFee: big.NewInt(baseFee),
	}
}

func TestSubscriber_EmitsBlocks(t *testing.T) {
	sub := newFakeSubscription()
	sub.headers <- header(100, 1_700_000_000, 25_000_000_000)
	sub.headers <- header(101, 1_700_000_012, 26_000_000_000)

	d := &fakeDialer{plan: []dialStep{{sub: sub}}}
	s := newTestSubscriber(d)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	ev := <-s.Events()
	if ev.Number != 100 || ev.BaseFee.Int64() != 25_000_000_000 {
		t.Errorf("first event: got %+v, want {100, ..., 25e9}", ev)
	}
	if ev.Timestamp.Unix() != 1_700_000_000 {
		t.Errorf("timestamp: got %v, want 1_700_000_000", ev.Timestamp.Unix())
	}

	ev = <-s.Events()
	if ev.Number != 101 || ev.BaseFee.Int64() != 26_000_000_000 {
		t.Errorf("second event: got %+v, want {101, ..., 26e9}", ev)
	}

	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Errorf("Run: got %v, want context.Canceled", err)
	}
	select {
	case <-sub.closed:
	default:
		t.Error("subscription was not Close()d on shutdown")
	}
}

func TestSubscriber_ReconnectsAfterDrop(t *testing.T) {
	first := newFakeSubscription()
	first.headers <- header(200, 1_700_000_000, 1)

	second := newFakeSubscription()
	second.headers <- header(201, 1_700_000_012, 2)

	d := &fakeDialer{plan: []dialStep{{sub: first}, {sub: second}}}
	var logBuf bytes.Buffer
	captureSlog(t, &logBuf)
	s := newTestSubscriber(d)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	if ev := <-s.Events(); ev.Number != 200 {
		t.Errorf("first event: got %d, want 200", ev.Number)
	}
	// Drop the first connection: Subscriber should redial.
	first.errs <- errors.New("connection reset")

	if ev := <-s.Events(); ev.Number != 201 {
		t.Errorf("second event after reconnect: got %d, want 201", ev.Number)
	}
	if d.dials() != 2 {
		t.Errorf("expected 2 dial attempts, got %d", d.dials())
	}
	if !strings.Contains(logBuf.String(), "subscription dropped") {
		t.Errorf("expected drop log, got: %q", logBuf.String())
	}

	cancel()
	<-done
}

func TestSubscriber_RetriesAfterDialFailures(t *testing.T) {
	good := newFakeSubscription()
	good.headers <- header(400, 1_700_000_000, 1)

	dialErr := errors.New("connection refused")
	d := &fakeDialer{plan: []dialStep{
		{err: dialErr},
		{err: dialErr},
		{err: dialErr},
		{sub: good},
	}}
	var logBuf bytes.Buffer
	captureSlog(t, &logBuf)
	s := newTestSubscriber(d)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	// After three failed dials, the fourth succeeds and we get the block.
	select {
	case ev := <-s.Events():
		if ev.Number != 400 {
			t.Errorf("got block %d after retries, want 400", ev.Number)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("never reconnected after dial failures")
	}
	if d.dials() != 4 {
		t.Errorf("expected 4 dial attempts (3 fail + 1 success), got %d", d.dials())
	}
	if !strings.Contains(logBuf.String(), "dial failed") {
		t.Errorf("expected dial-failed log, got: %q", logBuf.String())
	}

	cancel()
	<-done
}

func TestSubscriber_StopsOnContextCancel(t *testing.T) {
	// No subs queued; dialer blocks on ctx after the plan is exhausted.
	d := &fakeDialer{}
	s := newTestSubscriber(d)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	time.Sleep(10 * time.Millisecond) // let it enter the dial wait
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Run: got %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}
	// Events channel should be closed on Run return.
	if _, ok := <-s.Events(); ok {
		t.Error("Events channel should be closed after Run returns")
	}
}
