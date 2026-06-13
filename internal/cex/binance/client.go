package binance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/shopspring/decimal"
)

// DefaultBaseURL is the production Binance Spot API endpoint.
const DefaultBaseURL = "https://api.binance.com"

// defaultHTTPTimeout is a backstop timeout applied to the underlying
// http.Client. The caller's context deadline is the primary mechanism;
// this is belt-and-suspenders so a missing context deadline cannot stall
// the process indefinitely.
const defaultHTTPTimeout = 15 * time.Second

// depthLimitTiers are the Binance depth-endpoint limits, ascending in both
// depth and rate-limit cost (weight=5/25/50 for 100/500/1000 levels per side).
var depthLimitTiers = []int{100, 500, 1000}

// pickDepthLimit picks the smallest depth-endpoint tier that, given the
// symbol's per-level liquidity estimate, should cover the largest requested
// trade size. Falls back to the deepest tier if no smaller one is sufficient.
func pickDepthLimit(symbol Symbol, sizes []decimal.Decimal) int {
	maxSize := decimal.Zero
	for _, s := range sizes {
		if s.GreaterThan(maxSize) {
			maxSize = s
		}
	}
	if maxSize.IsZero() || !symbol.EstLiquidityPerLevel.IsPositive() {
		return depthLimitTiers[0]
	}
	levelsNeeded := maxSize.Div(symbol.EstLiquidityPerLevel).Ceil().IntPart()
	for _, t := range depthLimitTiers {
		if int64(t) >= levelsNeeded {
			return t
		}
	}
	return depthLimitTiers[len(depthLimitTiers)-1]
}

// Client fetches data from Binance's public REST endpoints. It is intentionally
// thin: no rate limiting, no retries, no circuit breaking — those concerns
// belong to wrapper layers above the adapter.
type Client struct {
	baseURL    string
	httpClient *http.Client
}

// NewClient constructs a Client bound to the given base URL. The HTTP client
// has a backstop timeout; callers should still pass a context with a deadline.
func NewClient(baseURL string) *Client {
	return &Client{
		baseURL:    baseURL,
		httpClient: &http.Client{Timeout: defaultHTTPTimeout},
	}
}

// EffectivePrices returns a Snapshot containing the raw top-of-book and the
// slippage-aware effective per-unit price for each (size, side) combination
// against the current orderbook for `symbol`.
//
// The Quotes slice has 2*len(sizes) entries — for each input size, one
// BUY (consuming asks) followed by one SELL (consuming bids), in input order.
// Sizes that exceed available depth on a given side are returned with
// Quote.Err set to ErrInsufficientDepth; the top-level error is returned
// only if fetching the orderbook itself failed.
func (c *Client) EffectivePrices(ctx context.Context, symbol Symbol, sizes []decimal.Decimal) (Snapshot, error) {
	snap := Snapshot{Quotes: make([]Quote, 2*len(sizes))}
	pending := make([]bool, 2*len(sizes))
	for i := range pending {
		pending[i] = true
	}

	// Pick the initial depth tier from the symbol's density estimate.
	// Escalate to deeper tiers only for (size, side) combinations that
	// still report insufficient depth — successful answers from earlier
	// tiers are kept, not recomputed.
	initialLimit := pickDepthLimit(symbol, sizes)
	firstFetch := true
	for _, limit := range depthLimitTiers {
		if limit < initialLimit {
			continue
		}
		bids, asks, err := c.fetchDepth(ctx, symbol.Code, limit)
		if err != nil {
			return Snapshot{}, err
		}
		if firstFetch {
			snap.BestBid = topPrice(bids)
			snap.BestAsk = topPrice(asks)
			firstFetch = false
		}
		anyPending := false
		for i, sz := range sizes {
			tryResolve(snap.Quotes, pending, 2*i, sz, Buy, asks)
			tryResolve(snap.Quotes, pending, 2*i+1, sz, Sell, bids)
			if pending[2*i] || pending[2*i+1] {
				anyPending = true
			}
		}
		if !anyPending {
			return snap, nil
		}
	}
	// Anything still pending after the deepest tier is genuinely beyond
	// the orderbook's available depth.
	for i, sz := range sizes {
		if pending[2*i] {
			snap.Quotes[2*i] = Quote{Size: sz, Side: Buy, Err: ErrInsufficientDepth}
		}
		if pending[2*i+1] {
			snap.Quotes[2*i+1] = Quote{Size: sz, Side: Sell, Err: ErrInsufficientDepth}
		}
	}
	return snap, nil
}

// tryResolve attempts to compute the quote at the given index against the
// supplied levels. If the walk succeeds, or fails with a non-depth error,
// the result is recorded in out[idx] and pending[idx] is cleared. If the
// walk fails with ErrInsufficientDepth, the slot is left pending so a deeper
// tier can retry it.
func tryResolve(out []Quote, pending []bool, idx int, size decimal.Decimal, side Side, levels []level) {
	if !pending[idx] {
		return
	}
	price, _, err := walkOrderbook(levels, size)
	if err == nil {
		out[idx] = Quote{Size: size, Side: side, Price: price}
		pending[idx] = false
		return
	}
	if !errors.Is(err, ErrInsufficientDepth) {
		out[idx] = Quote{Size: size, Side: side, Err: err}
		pending[idx] = false
	}
}

// topPrice returns the first level's price, or decimal.Zero if levels is empty.
func topPrice(levels []level) decimal.Decimal {
	if len(levels) == 0 {
		return decimal.Zero
	}
	return levels[0].price
}

// fetchDepth fetches the orderbook for `symbol` with at most `limit` levels
// per side. Bids are returned highest-price first; asks lowest-price first.
func (c *Client) fetchDepth(ctx context.Context, symbol string, limit int) (bids, asks []level, err error) {
	url := fmt.Sprintf("%s/api/v3/depth?symbol=%s&limit=%d", c.baseURL, symbol, limit)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, nil, err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, nil, fmt.Errorf("binance returned %d: %s", resp.StatusCode, body)
	}
	return parseDepthResponse(resp.Body)
}

// depthResponse is the raw JSON shape from Binance's /api/v3/depth endpoint.
type depthResponse struct {
	LastUpdateID uint64      `json:"lastUpdateId"`
	Bids         [][2]string `json:"bids"`
	Asks         [][2]string `json:"asks"`
}

func parseDepthResponse(r io.Reader) (bids, asks []level, err error) {
	var d depthResponse
	if err := json.NewDecoder(r).Decode(&d); err != nil {
		return nil, nil, fmt.Errorf("decode depth response: %w", err)
	}
	bids, err = parseLevels(d.Bids)
	if err != nil {
		return nil, nil, fmt.Errorf("parse bids: %w", err)
	}
	asks, err = parseLevels(d.Asks)
	if err != nil {
		return nil, nil, fmt.Errorf("parse asks: %w", err)
	}
	return bids, asks, nil
}

func parseLevels(raw [][2]string) ([]level, error) {
	out := make([]level, 0, len(raw))
	for i, lv := range raw {
		price, err := decimal.NewFromString(lv[0])
		if err != nil {
			return nil, fmt.Errorf("level %d price %q: %w", i, lv[0], err)
		}
		size, err := decimal.NewFromString(lv[1])
		if err != nil {
			return nil, fmt.Errorf("level %d size %q: %w", i, lv[1], err)
		}
		out = append(out, level{price: price, size: size})
	}
	return out, nil
}
