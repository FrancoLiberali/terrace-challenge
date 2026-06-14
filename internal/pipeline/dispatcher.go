// Package pipeline fans block events out to per-venue Snapshotters and
// streams their independent results onto a single channel. The
// Dispatcher does not pair, gather, or cancel — see architecture.md
// decisions 2 (stale-block handling) and the production-broker section
// for the rationale.
package pipeline

import (
	"context"
	"sync"
	"time"

	"github.com/FrancoLiberali/terrace-challenge/internal/chain"
	"github.com/FrancoLiberali/terrace-challenge/internal/pricing"
)

// Snapshotter produces a per-block effective-price snapshot from a single
// venue. Implementations bind their venue-specific configuration at
// construction time so the call site stays uniform across CEX and DEX.
type Snapshotter interface {
	Snapshot(ctx context.Context, block chain.BlockEvent) (pricing.Quotes, error)
}

// VenueResult is one venue's response for one block. Downstream consumers
// correlate these by Block.Number; the Dispatcher itself does not pair.
// When Err is non-nil, Quotes is the zero value.
type VenueResult struct {
	Venue  string
	Block  chain.BlockEvent
	Quotes pricing.Quotes
	Err    error
}

// Dispatcher fans out per-block triggers to every registered Snapshotter
// and forwards their independent results onto Results().
type Dispatcher struct {
	venues  map[string]Snapshotter
	timeout time.Duration // per-call deadline; tests override directly
	out     chan VenueResult
}

const defaultCallTimeout = 8 * time.Second

// NewDispatcher wires the given venues (keyed by venue name) with the
// default per-call timeout.
func NewDispatcher(venues map[string]Snapshotter) *Dispatcher {
	return &Dispatcher{
		venues:  venues,
		timeout: defaultCallTimeout,
		out:     make(chan VenueResult),
	}
}

// Results returns the output channel; closed when Run returns.
func (d *Dispatcher) Results() <-chan VenueResult { return d.out }

// Run reads BlockEvents from events and spawns one Snapshotter goroutine
// per venue per block. Returns when ctx is cancelled or events is closed.
func (d *Dispatcher) Run(ctx context.Context, events <-chan chain.BlockEvent) error {
	// Defers run LIFO: pending.Wait() runs BEFORE close(d.out) so
	// in-flight dispatchers exit (via ctx.Done in their emit select)
	// before the channel close, avoiding send-on-closed-channel panics.
	var pending sync.WaitGroup
	defer close(d.out)
	defer pending.Wait()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev, ok := <-events:
			if !ok {
				return nil
			}
			for name, sn := range d.venues {
				pending.Go(func() { d.dispatch(ctx, name, sn, ev) })
			}
		}
	}
}

// dispatch fires one venue's Snapshot call with its own per-call timeout
// context, then forwards the result onto d.out. The emit select's
// ctx.Done arm is for Dispatcher shutdown only.
func (d *Dispatcher) dispatch(ctx context.Context, name string, s Snapshotter, ev chain.BlockEvent) {
	callCtx, cancel := context.WithTimeout(ctx, d.timeout)
	defer cancel()

	quotes, err := s.Snapshot(callCtx, ev)
	result := VenueResult{Venue: name, Block: ev, Quotes: quotes, Err: err}
	select {
	case d.out <- result:
	case <-ctx.Done():
	}
}
