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

// EffectivePrices returns the slippage-aware effective per-unit price for
// each (size, side) combination against the current orderbook for `symbol`.
// Sizes must be in ascending order; the per-side processing relies on that
// invariant to exit early on the first size that exceeds available depth.
//
// Buy[i] and Sell[i] in the returned Quotes both refer to sizes[i]. Sizes
// that exceed available depth on a given side are returned with
// Quote.Err set to ErrInsufficientDepth; the top-level error is returned
// only if fetching the orderbook itself failed.
func (c *Client) EffectivePrices(ctx context.Context, symbol Symbol, sizes []decimal.Decimal) (Quotes, error) {
	// Per side, an output slice keyed by input-size index and a cursor for
	// the first index still waiting for a fill. Because sizes are ascending
	// and the book is monotonic, the first index that doesn't fit at a
	// given tier implies every later index also fails at that tier — so a
	// single integer per side captures the entire pending state.
	out := Quotes{
		Buy:  make([]Quote, len(sizes)),
		Sell: make([]Quote, len(sizes)),
	}
	buyFrom, sellFrom := 0, 0

	initialLimit := pickDepthLimit(symbol, sizes)
	for _, limit := range depthLimitTiers {
		if limit < initialLimit {
			continue
		}
		if buyFrom == len(sizes) && sellFrom == len(sizes) {
			break
		}
		bids, asks, err := c.fetchDepth(ctx, symbol.Code, limit)
		if err != nil {
			return Quotes{}, err
		}
		buyFrom = fillSide(out.Buy, sizes, buyFrom, asks, Buy)
		sellFrom = fillSide(out.Sell, sizes, sellFrom, bids, Sell)
	}
	markInsufficient(out.Buy, sizes, buyFrom, Buy)
	markInsufficient(out.Sell, sizes, sellFrom, Sell)

	return out, nil
}

// fillSide walks `levels` for sizes[from:] in ascending order, writing the
// resulting Quote into out[i]. It returns the index of the first size that
// did not fit — because sizes are ascending, every larger size will also
// have failed at this tier and must be retried at a deeper one.
func fillSide(out []Quote, sizes []decimal.Decimal, from int, levels []level, side Side) int {
	for i := from; i < len(sizes); i++ {
		price, _, err := walkOrderbook(levels, sizes[i])
		if err == nil {
			out[i] = Quote{Size: sizes[i], Side: side, Price: price}
			continue
		}
		if errors.Is(err, ErrInsufficientDepth) {
			return i
		}
		// Non-depth error (e.g., invalid size). Record and continue with
		// the rest of the sizes — the book itself is still usable.
		out[i] = Quote{Size: sizes[i], Side: side, Err: err}
	}
	return len(sizes)
}

// markInsufficient fills out[from:] with ErrInsufficientDepth quotes for the
// sizes that remained pending after the deepest fetched tier.
func markInsufficient(out []Quote, sizes []decimal.Decimal, from int, side Side) {
	for i := from; i < len(sizes); i++ {
		out[i] = Quote{Size: sizes[i], Side: side, Err: ErrInsufficientDepth}
	}
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
	if err = json.NewDecoder(r).Decode(&d); err != nil {
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
