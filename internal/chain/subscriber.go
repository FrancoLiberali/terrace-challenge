// Package chain subscribes to Ethereum's newHeads stream over WebSocket
// and emits one BlockEvent per new block. It owns the connection
// lifecycle: dial, subscribe, reconnect after drops, and clean shutdown
// on context cancellation. Consumers that care about missed blocks
// across a reconnect can compute that from BlockEvent.Number themselves
// — the subscriber does not interpret the stream.
//
// Reconnects use a small constant delay between attempts — just enough to
// avoid hammering the node during a sustained outage. The proper
// exponential-backoff-with-jitter (and the wider resilience layer: rate
// limiting, circuit breaking, retry on transient errors) will land in
// Step 6 (see plan.md / architecture.md), at which point the chain
// subscriber will use the same shared backoff utility as the adapter
// wrappers.
package chain

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
)

// BlockEvent is one block observation produced by the subscriber.
type BlockEvent struct {
	Number    uint64
	Timestamp time.Time
	// BaseFee is the EIP-1559 base fee per gas (in wei) from the block
	// header. It is NOT the full effective gas price a transaction would
	// pay (which adds a per-transaction priority fee on top); see
	// limitations.md §7 for the cost-model implications. Never nil on
	// post-London mainnet.
	BaseFee *big.Int
}

// dialer abstracts the WebSocket dial + subscribe so the reconnect state
// machine can be tested with a fake.
type dialer interface {
	Dial(ctx context.Context) (subscription, error)
}

// subscription is one live newHeads stream. Tests substitute a fake.
type subscription interface {
	Headers() <-chan *types.Header
	Err() <-chan error
	Close()
}

// Subscriber maintains a newHeads subscription over WebSocket, emitting
// one BlockEvent per new block on the channel returned by Events().
//
// Connection drops are handled internally: the subscriber redials after
// a constant delay (reconnectDelay) and resumes the subscription on the
// next available block. Missed blocks during the outage are not
// backfilled and not flagged — consumers that care can detect gaps from
// the BlockEvent.Number sequence directly.
type Subscriber struct {
	dial dialer
	out  chan BlockEvent

	// reconnectDelay is the wait between reconnect attempts. NewSubscriber
	// sets a sane default; tests in this package override it directly to
	// keep the suite fast.
	reconnectDelay time.Duration
}

const defaultReconnectDelay = 1 * time.Second

// NewSubscriber returns a Subscriber bound to the given WebSocket RPC URL.
// Call Events() once for the output stream, then Run(ctx) to drive it.
func NewSubscriber(wsURL string) *Subscriber {
	return &Subscriber{
		dial:           &ethDialer{url: wsURL},
		out:            make(chan BlockEvent),
		reconnectDelay: defaultReconnectDelay,
	}
}

// Events returns the channel block events are emitted on. The channel is
// closed when Run returns.
func (s *Subscriber) Events() <-chan BlockEvent { return s.out }

// Run blocks until ctx is cancelled, maintaining the newHeads subscription
// across reconnects. It closes the Events channel before returning so
// consumers ranging over it terminate naturally.
func (s *Subscriber) Run(ctx context.Context) error {
	// Closing s.out on the way out is what makes the consumer's
	// `for ev := range sub.Events()` loop terminate. Deferring it
	// guarantees we close on every return path (ctx cancel during
	// dial, ctx cancel during sleep, ctx cancel during stream).
	defer close(s.out)

	for {
		sub, err := s.dialWithRetry(ctx)
		if err != nil {
			return err
		}

		err = s.stream(ctx, sub)
		// Tear down the WS connection on every exit from stream(),
		// whether stream returned because of ctx cancel or because
		// the subscription died. Missing this would leak the WS
		// socket and go-ethereum's reader goroutine on every reconnect.
		sub.Close()
		// stream returns nil on ctx cancel and non-nil on subscription
		// death — but we can't trust nil-as-cancel alone, since stream
		// can also return nil if the consumer left mid-emit. Re-check
		// ctx directly: if it fired, the consumer wants out, not another
		// reconnect.
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// Subscription dropped while ctx is still alive — provider hiccup,
		// TCP reset, peer-initiated close. Loop back to redial without
		// delay: the drop itself is the timing signal (the node was alive
		// enough to send us an error, so the network just changed state),
		// and the next dial often succeeds immediately. The
		// reconnectDelay only applies to *dial-failure* retries inside
		// dialWithRetry.
		slog.Warn("subscription dropped — reconnecting", "err", err)
	}
}

// dialWithRetry returns a live subscription, retrying after reconnectDelay
// on any non-ctx error. The retry loop is self-contained so Run() reads
// as a clean three-step state machine (dial → stream → reconnect).
//
// Returns (sub, nil) on success. Returns (nil, err) only when ctx is
// cancelled or its deadline expires — at which point Run() propagates
// the error up to the caller and shuts down.
func (s *Subscriber) dialWithRetry(ctx context.Context) (subscription, error) {
	for {
		sub, err := s.dial.Dial(ctx)
		if err == nil {
			return sub, nil
		}
		// A ctx-related error from Dial is the consumer telling us to
		// stop, not a connection failure. Propagate it directly: no log
		// line (the consumer knows they cancelled), no sleep (we're not
		// retrying).
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		slog.Warn("dial failed — retrying", "err", err, "delay", s.reconnectDelay)
		// sleepCtx returns false if ctx fires before the delay elapses,
		// meaning the consumer cancelled mid-backoff — bow out with the
		// ctx error rather than redialing.
		if !sleepCtx(ctx, s.reconnectDelay) {
			return nil, ctx.Err()
		}
	}
}

// stream consumes from the subscription until the context is cancelled or
// the subscription emits an error. Returns nil on graceful exit (ctx),
// or the subscription error otherwise.
func (s *Subscriber) stream(ctx context.Context, sub subscription) error {
	for {
		select {
		case <-ctx.Done():
			// Graceful exit. Return nil so Run() distinguishes
			// "consumer cancelled" from "subscription died" (which
			// comes through the sub.Err() arm below). The nil result
			// is correct because cancellation isn't an error
			// condition — it's the requested behavior.
			return nil
		case err := <-sub.Err():
			// go-ethereum's Subscription.Err() contract allows nil
			// to mean "subscription closed without an error reason"
			// (e.g., the server initiated a clean unsubscribe).
			// We synthesize an error so Run()'s log line and any
			// upstream error-handling have something non-nil to
			// describe the reason for the reconnect.
			if err == nil {
				return errors.New("subscription closed without error")
			}
			return err
		case h := <-sub.Headers():
			// A nil header on a successful receive means the channel
			// was closed under us — go-ethereum no longer has anything
			// to send. Treat this the same as a dropped subscription
			// so Run() redials.
			if h == nil {
				return errors.New("subscription channel closed")
			}
			// h.Time is uint64 (Ethereum stores block timestamps as
			// seconds-since-epoch in an unsigned integer because the chain
			// has no concept of "before genesis"). time.Unix takes int64
			// because Go's time package supports pre-1970 timestamps. The
			// cast bridges those two reasonable design choices.
			//
			// gosec G115 flags uint64→int64 conversions as a potential
			// overflow. For chain timestamps it's a non-issue: int64's
			// max is ~9.2×10¹⁸ seconds (year 292,277,026,596). Current
			// Ethereum timestamps are ~1.7×10⁹. The overflow condition
			// cannot occur on any plausible chain, so we suppress the
			// warning rather than adding inert runtime bounds-checking.
			ev := BlockEvent{
				Number:    h.Number.Uint64(),
				Timestamp: time.Unix(int64(h.Time), 0).UTC(), //nolint:gosec // see comment above
				BaseFee:   h.BaseFee,
			}
			if !s.emit(ctx, ev) {
				return nil
			}
		}
	}
}

// emit sends ev to the consumer on s.out. Returns true on success, false
// if ctx fired while we were waiting for a reader — in which case the
// caller should stop streaming and let Run() exit cleanly.
//
// Why a select rather than a naked `s.out <- ev`: s.out is unbuffered
// (backpressure is intentional — we want consumer stalls to be visible),
// so a naked send would block forever if the consumer goroutine already
// exited. Without the ctx-Done arm, Run() could hang on shutdown if the
// consumer left mid-emit. The cost is one possibly-lost BlockEvent on
// shutdown, which is fine — we're shutting down anyway.
func (s *Subscriber) emit(ctx context.Context, ev BlockEvent) bool {
	select {
	case s.out <- ev:
		return true
	case <-ctx.Done():
		return false
	}
}

// sleepCtx sleeps for d or until ctx is cancelled; returns true if the
// full duration elapsed, false if ctx fired first.
//
// Why NewTimer + defer Stop instead of `<-time.After(d)`: time.After parks
// a Timer in the runtime that doesn't get released until the timer
// actually fires. If ctx.Done() wins the select, the underlying timer
// keeps running pointlessly until d elapses — small leak, but it adds up
// across many reconnect cycles. NewTimer with `defer Stop()` releases
// the timer the moment we exit the function. (Go 1.23 made time.After
// smarter about this; NewTimer is the portable form.)
//
// Why bool return rather than error: the only "error" path here is ctx
// cancellation, which the caller can read directly from ctx.Err(). A
// bool keeps the call site one-liner-clean:
//
//	if !sleepCtx(ctx, d) { return ctx.Err() }
//
// — and makes the intent ("did we sleep the whole way?") explicit at
// the call site instead of buried in an error type.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// ethDialer is the production implementation of dialer, backed by
// go-ethereum's ethclient over WebSocket.
type ethDialer struct{ url string }

func (e *ethDialer) Dial(ctx context.Context) (subscription, error) {
	client, err := ethclient.DialContext(ctx, e.url)
	if err != nil {
		return nil, fmt.Errorf("dial RPC: %w", err)
	}
	// Buffered between go-ethereum's WS reader goroutine and our stream()
	// loop. Unbuffered would mean any transient consumer pause stalls the
	// wire reader → TCP backpressure → eventual forced reconnect. 16 slots
	// at ~12s/block is ~3 minutes of headroom — enough for GC pauses and
	// downstream hiccups, small enough not to hoard stale blocks.
	ch := make(chan *types.Header, 16) //nolint:mnd // see comment above
	sub, err := client.SubscribeNewHead(ctx, ch)
	if err != nil {
		// SubscribeNewHead failed AFTER the WS connection was opened.
		// We have to close the client manually here, otherwise we leak
		// both the TCP socket and the reader goroutine ethclient already
		// started internally.
		client.Close()
		return nil, fmt.Errorf("subscribe newHeads: %w", err)
	}
	return &ethSubscription{client: client, sub: sub, headers: ch}, nil
}

// ethSubscription wraps go-ethereum's subscription primitives into the
// minimal subscription interface this package uses.
type ethSubscription struct {
	client  *ethclient.Client
	sub     ethereum.Subscription
	headers chan *types.Header
}

func (e *ethSubscription) Headers() <-chan *types.Header { return e.headers }
func (e *ethSubscription) Err() <-chan error             { return e.sub.Err() }

// Close tears down both layers of the subscription. Both calls are
// necessary: Unsubscribe stops go-ethereum's per-subscription reader
// goroutine, and client.Close releases the underlying WS connection.
// Missing Unsubscribe orphans the reader; missing client.Close leaks
// the TCP socket.
func (e *ethSubscription) Close() {
	e.sub.Unsubscribe()
	e.client.Close()
}
