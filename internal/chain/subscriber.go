// Package chain subscribes to Ethereum's newHeads stream over WebSocket
// and emits one BlockEvent per new block. It owns the connection
// lifecycle: dial, subscribe, reconnect after drops, last-block tracking
// for gap detection on reconnect, and clean shutdown on context
// cancellation.
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
	"log"
	"math/big"
	"sync/atomic"
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
// a constant delay (reconnectDelay) and resumes the subscription. A
// gap-detection log line fires when the first block after a reconnect
// skips one or more numbers past the last seen block — missed headers
// are not backfilled; that decision belongs to the consumer.
type Subscriber struct {
	dial dialer
	out  chan BlockEvent

	// lastBlock tracks the most recent block number seen, used to log
	// gap warnings when a fresh subscription resumes past it.
	lastBlock atomic.Uint64

	// reconnectDelay is the wait between reconnect attempts. NewSubscriber
	// sets a sane default; tests in this package override it directly to
	// keep the suite fast.
	reconnectDelay time.Duration

	logger *log.Logger
}

const defaultReconnectDelay = 1 * time.Second

// NewSubscriber returns a Subscriber bound to the given WebSocket RPC URL.
// Call Events() once for the output stream, then Run(ctx) to drive it.
func NewSubscriber(wsURL string) *Subscriber {
	return &Subscriber{
		dial:           &ethDialer{url: wsURL},
		out:            make(chan BlockEvent),
		reconnectDelay: defaultReconnectDelay,
		logger:         log.Default(),
	}
}

// Events returns the channel block events are emitted on. The channel is
// closed when Run returns.
func (s *Subscriber) Events() <-chan BlockEvent { return s.out }

// Run blocks until ctx is cancelled, maintaining the newHeads subscription
// across reconnects. It closes the Events channel before returning so
// consumers ranging over it terminate naturally.
func (s *Subscriber) Run(ctx context.Context) error {
	defer close(s.out)

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		sub, err := s.dial.Dial(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return err
			}
			s.logger.Printf("chain: dial failed: %v; retrying in %v", err, s.reconnectDelay)
			if !sleepCtx(ctx, s.reconnectDelay) {
				return ctx.Err()
			}
			continue
		}

		err = s.stream(ctx, sub)
		sub.Close()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// Subscription dropped while ctx is still alive. Loop back to
		// redial; no delay here — the drop itself is the timing signal,
		// and the next dial may well succeed immediately.
		s.logger.Printf("chain: subscription dropped: %v; reconnecting", err)
	}
}

// stream consumes from the subscription until the context is cancelled or
// the subscription emits an error. Returns nil on graceful exit (ctx),
// or the subscription error otherwise.
func (s *Subscriber) stream(ctx context.Context, sub subscription) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-sub.Err():
			if err == nil {
				return errors.New("subscription closed without error")
			}
			return err
		case h := <-sub.Headers():
			if h == nil {
				return errors.New("subscription channel closed")
			}
			n := h.Number.Uint64()
			if last := s.lastBlock.Load(); last != 0 && n > last+1 {
				s.logger.Printf("chain: potential gap: missed blocks %d..%d", last+1, n-1)
			}
			s.lastBlock.Store(n)
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
				Number:    n,
				Timestamp: time.Unix(int64(h.Time), 0).UTC(), //nolint:gosec // see comment above
				BaseFee:   h.BaseFee,
			}
			select {
			case s.out <- ev:
			case <-ctx.Done():
				return nil
			}
		}
	}
}

// sleepCtx sleeps for d or until ctx is cancelled; returns true if the
// full duration elapsed, false if ctx fired first.
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
	ch := make(chan *types.Header, 16) //nolint:mnd // small buffer; one slot per block, 16 ≈ 3 minutes of mainnet headroom
	sub, err := client.SubscribeNewHead(ctx, ch)
	if err != nil {
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
func (e *ethSubscription) Close() {
	e.sub.Unsubscribe()
	e.client.Close()
}
