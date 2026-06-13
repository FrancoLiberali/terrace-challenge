package binance

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/shopspring/decimal"
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
	snap, err := client.EffectivePrices(context.Background(), SymbolETHUSDC, []decimal.Decimal{dec("1"), dec("10")})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Snapshot must carry the raw top-of-book from the initial fetch.
	if !snap.BestBid.Equal(dec("2249.50")) {
		t.Errorf("BestBid: got %s, want 2249.50", snap.BestBid)
	}
	if !snap.BestAsk.Equal(dec("2250.10")) {
		t.Errorf("BestAsk: got %s, want 2250.10", snap.BestAsk)
	}

	if got, want := len(snap.Quotes), 4; got != want {
		t.Fatalf("quotes: got %d, want %d", got, want)
	}

	// Layout: [Buy 1, Sell 1, Buy 10, Sell 10]
	checks := []struct {
		idx       int
		wantSize  string
		wantSide  Side
		wantPrice string
		comment   string
	}{
		// 1 ETH BUY eats the lowest ask: 1 ETH at 2250.10 → 2250.10
		{0, "1", Buy, "2250.10", "1 ETH BUY"},
		// 1 ETH SELL eats the highest bid: 1 ETH at 2249.50 → 2249.50
		{1, "1", Sell, "2249.50", "1 ETH SELL"},
		// 10 ETH BUY: 3.5 at 2250.10 + 6.5 at 2250.20 = 22501.65 → /10 = 2250.165
		{2, "10", Buy, "2250.165", "10 ETH BUY"},
		// 10 ETH SELL: 8.2 at 2249.50 (18445.90) + 1.8 at 2249.40 (4048.92) = 22494.82 → /10 = 2249.482
		{3, "10", Sell, "2249.482", "10 ETH SELL"},
	}
	for _, c := range checks {
		q := snap.Quotes[c.idx]
		if !q.Size.Equal(dec(c.wantSize)) {
			t.Errorf("%s: size got %s, want %s", c.comment, q.Size, c.wantSize)
		}
		if q.Side != c.wantSide {
			t.Errorf("%s: side got %v, want %v", c.comment, q.Side, c.wantSide)
		}
		if q.Err != nil {
			t.Errorf("%s: unexpected Err: %v", c.comment, q.Err)
		}
		if !q.Price.Equal(dec(c.wantPrice)) {
			t.Errorf("%s: price got %s, want %s", c.comment, q.Price, c.wantPrice)
		}
	}
}

func TestClient_EffectivePrices_PerRowInsufficientDepth(t *testing.T) {
	// Asks only have 2 ETH total — a 10 ETH BUY can't be filled at any tier
	// (the server returns the same body regardless of limit). Bids have 12
	// ETH — a 10 ETH SELL succeeds.
	const body = `{
		"lastUpdateId": 1,
		"bids": [["2249.50", "12.0"]],
		"asks": [["2250.10", "2.0"]]
	}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	client := NewClient(srv.URL)
	snap, err := client.EffectivePrices(context.Background(), SymbolETHUSDC, []decimal.Decimal{dec("10")})
	if err != nil {
		t.Fatalf("unexpected top-level error: %v", err)
	}
	if len(snap.Quotes) != 2 {
		t.Fatalf("expected 2 quotes, got %d", len(snap.Quotes))
	}
	if !errors.Is(snap.Quotes[0].Err, ErrInsufficientDepth) {
		t.Errorf("expected BUY quote to have ErrInsufficientDepth, got %v", snap.Quotes[0].Err)
	}
	if snap.Quotes[1].Err != nil {
		t.Errorf("expected SELL quote to succeed, got %v", snap.Quotes[1].Err)
	}
	if !snap.Quotes[1].Price.Equal(dec("2249.50")) {
		t.Errorf("SELL price: got %s, want 2249.50", snap.Quotes[1].Price)
	}
}

func TestClient_EffectivePrices_NonOKStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
// three behaviours at once:
//   - the initial heuristic tier is escalated to a deeper tier when the
//     orderbook turns out thinner than the per-pair density assumed;
//   - quotes that succeeded at the initial tier are NOT recomputed against
//     the deeper-tier data (their per-unit price stays as the first answer);
//   - the Snapshot's BestBid/BestAsk reflect the initial fetch, not later tiers.
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
	snap, err := client.EffectivePrices(context.Background(), sym, []decimal.Decimal{dec("1"), dec("200")})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Two fetches expected: tier 100 (heuristic pick) and tier 500 (escalation
	// for size=200 only — size=1 was already filled and is not re-queried).
	want := []string{"100", "500"}
	if len(requested) != len(want) || requested[0] != want[0] || requested[1] != want[1] {
		t.Errorf("requested limits: got %v, want %v", requested, want)
	}

	// BestBid/BestAsk come from the FIRST fetch, not the later tier.
	if !snap.BestBid.Equal(dec("2249")) {
		t.Errorf("BestBid: got %s, want 2249 (from initial tier)", snap.BestBid)
	}
	if !snap.BestAsk.Equal(dec("2250")) {
		t.Errorf("BestAsk: got %s, want 2250 (from initial tier)", snap.BestAsk)
	}

	// quotes[0] = Buy 1 — succeeded at tier 100; should still report 2250.
	if !snap.Quotes[0].Price.Equal(dec("2250")) {
		t.Errorf("Buy 1: got %s, want 2250 (preserved from tier 100)", snap.Quotes[0].Price)
	}
	// quotes[1] = Sell 1 — same.
	if !snap.Quotes[1].Price.Equal(dec("2249")) {
		t.Errorf("Sell 1: got %s, want 2249 (preserved from tier 100)", snap.Quotes[1].Price)
	}
	// quotes[2] = Buy 200 — failed at tier 100, succeeded at tier 500.
	if !snap.Quotes[2].Price.Equal(dec("3000")) {
		t.Errorf("Buy 200: got %s, want 3000 (from tier 500 after escalation)", snap.Quotes[2].Price)
	}
	// quotes[3] = Sell 200 — same.
	if !snap.Quotes[3].Price.Equal(dec("2999")) {
		t.Errorf("Sell 200: got %s, want 2999 (from tier 500 after escalation)", snap.Quotes[3].Price)
	}
}

func TestParseDepthResponse_RejectsMalformedNumbers(t *testing.T) {
	const body = `{"lastUpdateId": 1, "bids": [["not-a-number", "1"]], "asks": []}`
	if _, _, err := parseDepthResponse(strings.NewReader(body)); err == nil {
		t.Fatal("expected error for malformed price, got nil")
	}
}
