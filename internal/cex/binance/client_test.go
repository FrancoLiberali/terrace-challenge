package binance

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/FrancoLiberali/terrace-challenge/internal/pricing"
	"github.com/FrancoLiberali/terrace-challenge/internal/resilience"
)

func TestClient_EffectivePrices(t *testing.T) {
	const body = `{
		"lastUpdateId": 1234567890,
		"bids": [["2249.50", "8.2"], ["2249.40", "12.0"]],
		"asks": [["2250.10", "3.5"], ["2250.20", "7.0"]]
	}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v3/depth" {
			t.Errorf("path: got %q, want /api/v3/depth", r.URL.Path)
		}
		if got := r.URL.Query().Get("symbol"); got != SymbolETHUSDC.Code {
			t.Errorf("symbol param: got %q, want %q", got, SymbolETHUSDC.Code)
		}
		// For the configured sizes (1, 10) the density heuristic should pick
		// the cheapest tier — verify the wire request matches.
		if got := r.URL.Query().Get("limit"); got != "100" {
			t.Errorf("limit param: got %q, want %q", got, "100")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	client := NewClient(srv.URL)
	quotes, err := client.EffectivePrices(context.Background(), SymbolETHUSDC, []decimal.Decimal{dec("1"), dec("10")})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got, want := len(quotes.Buy), 2; got != want {
		t.Fatalf("quotes.Buy len: got %d, want %d", got, want)
	}
	if got, want := len(quotes.Sell), 2; got != want {
		t.Fatalf("quotes.Sell len: got %d, want %d", got, want)
	}

	// Buy[i] and Sell[i] correspond to sizes[i].
	// Expected prices are post-fee: SymbolETHUSDC.TakerFeeBps is 10 bps.
	// BUY inflates by × 1.001; SELL discounts by × 0.999. See applyTakerFee.
	checks := []struct {
		q         pricing.Quote
		wantSize  string
		wantSide  pricing.Side
		wantPrice string
		comment   string
	}{
		// 1 ETH BUY:  ask=2250.10 → walk VWAP = 2250.10 → ×1.001 = 2252.3501
		{quotes.Buy[0], "1", pricing.Buy, "2252.3501", "1 ETH BUY"},
		// 1 ETH SELL: bid=2249.50 → walk VWAP = 2249.50 → ×0.999 = 2247.2505
		{quotes.Sell[0], "1", pricing.Sell, "2247.2505", "1 ETH SELL"},
		// 10 ETH BUY: 3.5@2250.10 + 6.5@2250.20 = 22501.65 → /10 = 2250.165 → ×1.001 = 2252.415165
		{quotes.Buy[1], "10", pricing.Buy, "2252.415165", "10 ETH BUY"},
		// 10 ETH SELL: 8.2@2249.50 + 1.8@2249.40 = 22494.82 → /10 = 2249.482 → ×0.999 = 2247.232518
		{quotes.Sell[1], "10", pricing.Sell, "2247.232518", "10 ETH SELL"},
	}
	for _, c := range checks {
		if !c.q.Size.Equal(dec(c.wantSize)) {
			t.Errorf("%s: size got %s, want %s", c.comment, c.q.Size, c.wantSize)
		}
		if c.q.Side != c.wantSide {
			t.Errorf("%s: side got %v, want %v", c.comment, c.q.Side, c.wantSide)
		}
		if c.q.Err != nil {
			t.Errorf("%s: unexpected Err: %v", c.comment, c.q.Err)
		}
		if !c.q.Price.Equal(dec(c.wantPrice)) {
			t.Errorf("%s: price got %s, want %s", c.comment, c.q.Price, c.wantPrice)
		}
	}
}

func TestClient_EffectivePrices_PerRowInsufficientDepth(t *testing.T) {
	// Each case configures one side as shallow (2 ETH) and the other deep
	// (12 ETH), then asks for a 10 ETH trade. The shallow side must fail
	// with ErrInsufficientDepth; the deep side must succeed at its top
	// level price. Both sides are tested so a regression that mixes up
	// Buy and Sell slots fails fast.
	cases := []struct {
		name      string
		body      string
		failSide  pricing.Side
		succSide  pricing.Side
		succPrice string
	}{
		{
			name:      "buy fails / sell succeeds",
			body:      `{"lastUpdateId":1,"bids":[["2249.50","12.0"]],"asks":[["2250.10","2.0"]]}`,
			failSide:  pricing.Buy,
			succSide:  pricing.Sell,
			succPrice: "2247.2505", // 2249.50 × (1 - 0.001)
		},
		{
			name:      "sell fails / buy succeeds",
			body:      `{"lastUpdateId":1,"bids":[["2249.50","2.0"]],"asks":[["2250.10","12.0"]]}`,
			failSide:  pricing.Sell,
			succSide:  pricing.Buy,
			succPrice: "2252.3501", // 2250.10 × (1 + 0.001)
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			client := NewClient(srv.URL)
			quotes, err := client.EffectivePrices(context.Background(), SymbolETHUSDC, []decimal.Decimal{dec("10")})
			if err != nil {
				t.Fatalf("unexpected top-level error: %v", err)
			}
			if len(quotes.Buy) != 1 || len(quotes.Sell) != 1 {
				t.Fatalf("expected one Buy and one Sell quote, got %d/%d", len(quotes.Buy), len(quotes.Sell))
			}
			fail, succ := quotes.Buy[0], quotes.Sell[0]
			if tc.failSide == pricing.Sell {
				fail, succ = quotes.Sell[0], quotes.Buy[0]
			}
			if !errors.Is(fail.Err, ErrInsufficientDepth) {
				t.Errorf("%s side: expected ErrInsufficientDepth, got %v", tc.failSide, fail.Err)
			}
			if fail.Side != tc.failSide {
				t.Errorf("failing slot side: got %v, want %v", fail.Side, tc.failSide)
			}
			if succ.Err != nil {
				t.Errorf("%s side: expected success, got %v", tc.succSide, succ.Err)
			}
			if succ.Side != tc.succSide {
				t.Errorf("succeeding slot side: got %v, want %v", succ.Side, tc.succSide)
			}
			if !succ.Price.Equal(dec(tc.succPrice)) {
				t.Errorf("%s price: got %s, want %s", tc.succSide, succ.Price, tc.succPrice)
			}
		})
	}
}

func TestClient_EffectivePrices_NonOKStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"code":-1100,"msg":"bad symbol"}`))
	}))
	defer srv.Close()

	client := NewClient(srv.URL)
	badSym := Symbol{Code: "BADSYM", EstLiquidityPerLevel: decimal.NewFromInt(1)}
	_, err := client.EffectivePrices(context.Background(), badSym, []decimal.Decimal{dec("1")})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "400") {
		t.Errorf("error should mention status 400, got: %v", err)
	}
}

func TestClient_NewClientWithHTTP_RetriesTransient5xx(t *testing.T) {
	// Server fails twice with 503 then returns valid depth JSON. Wired
	// with a binance.Client that uses resilience.NewRetryingHTTPClient,
	// EffectivePrices should succeed (proving the injected retrying
	// transport actually retried the failed attempts).
	const body = `{
		"lastUpdateId": 1,
		"bids": [["2249.50", "8.2"]],
		"asks": [["2250.10", "3.5"]]
	}`
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) < 3 {
			http.Error(w, "boom", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	httpClient := resilience.NewHTTPClient(resilience.HTTPClientConfig{
		Retry: &resilience.RetryConfig{
			MaxRetries:  3,
			InitialWait: 5 * time.Millisecond,
			MaxWait:     20 * time.Millisecond,
		},
		RequestTimeout: 2 * time.Second,
	})
	client := NewClientWithHTTP(srv.URL, httpClient)

	quotes, err := client.EffectivePrices(context.Background(), SymbolETHUSDC, []decimal.Decimal{dec("1")})
	if err != nil {
		t.Fatalf("EffectivePrices: %v", err)
	}
	if len(quotes.Buy) != 1 {
		t.Fatalf("expected 1 quote, got %d", len(quotes.Buy))
	}
	if got := calls.Load(); got != 3 {
		t.Errorf("expected 3 server hits (2 retries + success), got %d", got)
	}
}

func TestPickDepthLimit(t *testing.T) {
	cases := []struct {
		name    string
		density string
		sizes   []string
		want    int
	}{
		{"empty sizes returns smallest tier", "0.5", nil, 100},
		{"small size in dense market", "0.5", []string{"1"}, 100},
		{"size needing exactly 100", "0.5", []string{"50"}, 100},
		{"size just over 100 bumps to 500", "0.5", []string{"50.5"}, 500},
		{"size needing exactly 500", "0.5", []string{"250"}, 500},
		{"size just over 500 bumps to 1000", "0.5", []string{"250.5"}, 1000},
		{"size beyond 1000 clamps to deepest tier", "0.5", []string{"600"}, 1000},
		{"thin market needs deeper tier for same size", "0.01", []string{"5"}, 500},
		{"max element wins among multiple sizes", "0.5", []string{"1", "10", "200"}, 500},
		{"non-positive density falls back to smallest tier", "0", []string{"100"}, 100},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sym := Symbol{Code: "X", EstLiquidityPerLevel: decimal.RequireFromString(tc.density)}
			sizes := make([]decimal.Decimal, len(tc.sizes))
			for i, s := range tc.sizes {
				sizes[i] = decimal.RequireFromString(s)
			}
			if got := pickDepthLimit(sym, sizes); got != tc.want {
				t.Errorf("pickDepthLimit: got %d, want %d", got, tc.want)
			}
		})
	}
}

// TestClient_EffectivePrices_EscalatesAndPreservesSuccessfulQuotes verifies
// two behaviors at once:
//   - the initial heuristic tier is escalated to a deeper tier when the
//     orderbook turns out thinner than the per-pair density assumed;
//   - quotes that succeeded at the initial tier are NOT recomputed against
//     the deeper-tier data (their per-unit price stays as the first answer).
//
// We provoke this by configuring an overly optimistic per-pair density so
// pickDepthLimit picks tier 100, then have the test server return only 50 ETH
// of depth at that tier — enough for size=1 but not for size=200 — and 1000
// ETH at very different prices at tier 500.
func TestClient_EffectivePrices_EscalatesAndPreservesSuccessfulQuotes(t *testing.T) {
	var requested []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		limit := r.URL.Query().Get("limit")
		requested = append(requested, limit)
		switch limit {
		case "100":
			// 50 ETH each side; price 2250 / 2249.
			_, _ = w.Write([]byte(`{"lastUpdateId":1,"bids":[["2249","50"]],"asks":[["2250","50"]]}`))
		case "500":
			// Plenty of depth but very different prices — used to prove
			// size=1 keeps its tier-100 answer (2250/2249).
			_, _ = w.Write([]byte(`{"lastUpdateId":1,"bids":[["2999","1000"]],"asks":[["3000","1000"]]}`))
		}
	}))
	defer srv.Close()

	// Density 10 ETH/level → for max size 200 the heuristic estimates 20
	// levels needed, so picks the smallest tier (100).
	sym := Symbol{Code: "ETHUSDC", EstLiquidityPerLevel: decimal.NewFromInt(10)}
	client := NewClient(srv.URL)
	quotes, err := client.EffectivePrices(context.Background(), sym, []decimal.Decimal{dec("1"), dec("200")})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Two fetches expected: tier 100 (heuristic pick) and tier 500 (escalation
	// for size=200 only — size=1 was already filled and is not re-queried).
	want := []string{"100", "500"}
	if len(requested) != len(want) {
		t.Fatalf("requested limits: got %v, want %v", requested, want)
	}
	if requested[0] != want[0] || requested[1] != want[1] {
		t.Errorf("requested limits: got %v, want %v", requested, want)
	}

	// Buy[0] / Sell[0] = size=1 — succeeded at tier 100; preserved from there.
	if !quotes.Buy[0].Price.Equal(dec("2250")) {
		t.Errorf("Buy 1: got %s, want 2250 (preserved from tier 100)", quotes.Buy[0].Price)
	}
	if !quotes.Sell[0].Price.Equal(dec("2249")) {
		t.Errorf("Sell 1: got %s, want 2249 (preserved from tier 100)", quotes.Sell[0].Price)
	}
	// Buy[1] / Sell[1] = size=200 — failed at tier 100, succeeded at tier 500.
	if !quotes.Buy[1].Price.Equal(dec("3000")) {
		t.Errorf("Buy 200: got %s, want 3000 (from tier 500 after escalation)", quotes.Buy[1].Price)
	}
	if !quotes.Sell[1].Price.Equal(dec("2999")) {
		t.Errorf("Sell 200: got %s, want 2999 (from tier 500 after escalation)", quotes.Sell[1].Price)
	}
}

func TestClient_EffectivePrices_EmptySizes(t *testing.T) {
	// No HTTP traffic should happen — there's no work to do — and both
	// returned slices should be empty (not nil-failure-prone).
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("server should not be hit when sizes is empty")
	}))
	defer srv.Close()

	client := NewClient(srv.URL)
	quotes, err := client.EffectivePrices(context.Background(), SymbolETHUSDC, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(quotes.Buy) != 0 || len(quotes.Sell) != 0 {
		t.Errorf("empty input: got Buy=%d Sell=%d, want 0/0", len(quotes.Buy), len(quotes.Sell))
	}
}

func TestClient_EffectivePrices_NonPositiveSize_RecordsErr(t *testing.T) {
	// walkOrderbook rejects non-positive sizes with a non-depth error, which
	// fillSide records on the Quote rather than propagating up. Verifies the
	// public API tolerates a bad-input edge without panicking or short-
	// circuiting the rest of the work.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"lastUpdateId":1,"bids":[["2249","10"]],"asks":[["2250","10"]]}`))
	}))
	defer srv.Close()

	client := NewClient(srv.URL)
	quotes, err := client.EffectivePrices(context.Background(), SymbolETHUSDC, []decimal.Decimal{dec("0")})
	if err != nil {
		t.Fatalf("unexpected top-level error: %v", err)
	}
	if quotes.Buy[0].Err == nil {
		t.Errorf("Buy: expected Err for size=0, got nil")
	}
	if errors.Is(quotes.Buy[0].Err, ErrInsufficientDepth) {
		t.Errorf("Buy: non-positive size should NOT be reported as insufficient depth, got %v", quotes.Buy[0].Err)
	}
	if quotes.Sell[0].Err == nil {
		t.Errorf("Sell: expected Err for size=0, got nil")
	}
}

func TestParseDepthResponse_RejectsMalformedJSON(t *testing.T) {
	// Top-level JSON corruption (not just per-level number parsing).
	cases := []string{
		``,
		`not json`,
		`{"bids":`,
		`{"bids":"not-an-array","asks":[]}`,
	}
	for _, body := range cases {
		if _, _, err := parseDepthResponse(strings.NewReader(body)); err == nil {
			t.Errorf("expected error for malformed JSON %q, got nil", body)
		}
	}
}

func TestParseDepthResponse_RejectsMalformedNumbers(t *testing.T) {
	// One case per field × side to guard against a regression that only
	// breaks one of the four parse paths through parseLevels.
	cases := []struct {
		name string
		body string
	}{
		{
			name: "bid price",
			body: `{"lastUpdateId":1,"bids":[["not-a-number","1"]],"asks":[]}`,
		},
		{
			name: "bid size",
			body: `{"lastUpdateId":1,"bids":[["2249.50","not-a-number"]],"asks":[]}`,
		},
		{
			name: "ask price",
			body: `{"lastUpdateId":1,"bids":[],"asks":[["not-a-number","1"]]}`,
		},
		{
			name: "ask size",
			body: `{"lastUpdateId":1,"bids":[],"asks":[["2250.10","not-a-number"]]}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := parseDepthResponse(strings.NewReader(tc.body)); err == nil {
				t.Fatalf("expected error for malformed %s, got nil", tc.name)
			}
		})
	}
}
