package uniswapv3

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/shopspring/decimal"
)

// newQuoterServer stands up a fake JSON-RPC server that recognizes
// QuoterV2's two function selectors and replies with the supplied
// pre-packed output bytes. selectorCounts (optional) is populated with
// per-method call counts so tests can assert how many eth_calls were
// issued for each side.
func newQuoterServer(t *testing.T, sellOut, buyOut []byte, selectorCounts map[string]int) *httptest.Server {
	t.Helper()
	parsed, err := abi.JSON(strings.NewReader(quoterV2ABI))
	if err != nil {
		t.Fatalf("parse ABI: %v", err)
	}
	sellID := parsed.Methods["quoteExactInputSingle"].ID
	buyID := parsed.Methods["quoteExactOutputSingle"].ID

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage   `json:"id"`
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode JSON-RPC request: %v", err)
			return
		}
		if req.Method != "eth_call" {
			t.Errorf("method: got %q, want eth_call", req.Method)
			return
		}
		var msg struct {
			To    string `json:"to"`
			Input string `json:"input"` // newer go-ethereum ethclient
			Data  string `json:"data"`  // legacy
		}
		if err := json.Unmarshal(req.Params[0], &msg); err != nil {
			t.Errorf("decode call msg: %v", err)
			return
		}
		hexData := msg.Input
		if hexData == "" {
			hexData = msg.Data
		}
		callData, err := hex.DecodeString(strings.TrimPrefix(hexData, "0x"))
		if err != nil || len(callData) < 4 {
			t.Errorf("decode call data: %v (%q)", err, hexData)
			return
		}
		var out []byte
		switch {
		case bytes.Equal(callData[:4], sellID):
			if selectorCounts != nil {
				selectorCounts["sell"]++
			}
			out = sellOut
		case bytes.Equal(callData[:4], buyID):
			if selectorCounts != nil {
				selectorCounts["buy"]++
			}
			out = buyOut
		default:
			t.Errorf("unknown function selector: 0x%x", callData[:4])
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      req.ID,
			"result":  "0x" + hex.EncodeToString(out),
		})
	}))
}

// packOutputs ABI-encodes the four return values QuoterV2 produces.
// Only the first value (amountOut for Sell / amountIn for Buy) is
// load-bearing for our price computation; the other three are filled
// with zeros to satisfy the ABI shape.
func packOutputs(t *testing.T, method string, primary *big.Int) []byte {
	t.Helper()
	parsed, err := abi.JSON(strings.NewReader(quoterV2ABI))
	if err != nil {
		t.Fatalf("parse ABI: %v", err)
	}
	out, err := parsed.Methods[method].Outputs.Pack(
		primary,
		new(big.Int), // sqrtPriceX96After
		uint32(0),    // initializedTicksCrossed
		new(big.Int), // gasEstimate
	)
	if err != nil {
		t.Fatalf("pack %s outputs: %v", method, err)
	}
	return out
}

func TestEffectivePrices_HappyPath(t *testing.T) {
	// 1 ETH Sell: pool returns amountOut = 1700 USDC → price = 1700/ETH.
	sellOut := packOutputs(t, "quoteExactInputSingle", big.NewInt(1_700_000_000))
	// 1 ETH Buy: pool says you'd need amountIn = 1710 USDC → price = 1710/ETH.
	// (Buy is always at least as expensive as Sell — the 0.3% fee + slippage.)
	buyOut := packOutputs(t, "quoteExactOutputSingle", big.NewInt(1_710_000_000))

	counts := make(map[string]int)
	srv := newQuoterServer(t, sellOut, buyOut, counts)
	defer srv.Close()

	client, err := NewClient(srv.URL)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer client.Close()

	quotes, err := client.EffectivePrices(context.Background(), PoolETHUSDC03, []decimal.Decimal{dec("1")})
	if err != nil {
		t.Fatalf("EffectivePrices: %v", err)
	}

	if got, want := counts["sell"], 1; got != want {
		t.Errorf("sell selector calls: got %d, want %d", got, want)
	}
	if got, want := counts["buy"], 1; got != want {
		t.Errorf("buy selector calls: got %d, want %d", got, want)
	}

	if len(quotes.Buy) != 1 || len(quotes.Sell) != 1 {
		t.Fatalf("expected 1 Buy and 1 Sell quote, got %d/%d", len(quotes.Buy), len(quotes.Sell))
	}

	checks := []struct {
		q         Quote
		wantSide  Side
		wantPrice string
		comment   string
	}{
		{quotes.Sell[0], Sell, "1700", "1 ETH SELL"},
		{quotes.Buy[0], Buy, "1710", "1 ETH BUY"},
	}
	for _, c := range checks {
		if c.q.Err != nil {
			t.Errorf("%s: unexpected Err: %v", c.comment, c.q.Err)
		}
		if c.q.Side != c.wantSide {
			t.Errorf("%s: side got %v, want %v", c.comment, c.q.Side, c.wantSide)
		}
		if !c.q.Size.Equal(dec("1")) {
			t.Errorf("%s: size got %s, want 1", c.comment, c.q.Size)
		}
		if !c.q.Price.Equal(dec(c.wantPrice)) {
			t.Errorf("%s: price got %s, want %s", c.comment, c.q.Price, c.wantPrice)
		}
	}
}

func TestEffectivePrices_MultipleSizes_IssuesOneCallPerSizePerSide(t *testing.T) {
	// Constant pool: every Sell returns 1700 USDC/ETH, every Buy 1710 USDC/ETH.
	// (Tests the dispatch + count, not the slippage math — that's the pool's job.)
	sellOut := packOutputs(t, "quoteExactInputSingle", big.NewInt(1_700_000_000))
	buyOut := packOutputs(t, "quoteExactOutputSingle", big.NewInt(1_710_000_000))

	counts := make(map[string]int)
	srv := newQuoterServer(t, sellOut, buyOut, counts)
	defer srv.Close()

	client, err := NewClient(srv.URL)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer client.Close()

	sizes := []decimal.Decimal{dec("1"), dec("10"), dec("100")}
	quotes, err := client.EffectivePrices(context.Background(), PoolETHUSDC03, sizes)
	if err != nil {
		t.Fatalf("EffectivePrices: %v", err)
	}

	// Architecturally important: one eth_call per (size, side), no batching
	// (yet), no caching across sizes — see plan.md decision 4.
	if got, want := counts["sell"], len(sizes); got != want {
		t.Errorf("sell calls: got %d, want %d", got, want)
	}
	if got, want := counts["buy"], len(sizes); got != want {
		t.Errorf("buy calls: got %d, want %d", got, want)
	}

	// Sell amountOut scales linearly with size for a constant pool, so the
	// per-unit price stays at 1700; Buy stays at 1710.
	// Wait — packOutputs returns the same fixed amountOut regardless of input.
	// So size=10 returns amountOut=1700 USDC total, giving price=170 USDC/ETH.
	// We assert that explicitly, which doubles as a check that the size
	// argument flowed through to the price math.
	if !quotes.Sell[0].Price.Equal(dec("1700")) {
		t.Errorf("Sell[0] (size 1): got %s, want 1700", quotes.Sell[0].Price)
	}
	if !quotes.Sell[1].Price.Equal(dec("170")) {
		t.Errorf("Sell[1] (size 10): got %s, want 170", quotes.Sell[1].Price)
	}
	if !quotes.Sell[2].Price.Equal(dec("17")) {
		t.Errorf("Sell[2] (size 100): got %s, want 17", quotes.Sell[2].Price)
	}
}

func TestEffectivePrices_PerRowFailureWhenRPCErrors(t *testing.T) {
	// JSON-RPC error from the upstream node — verify it lands as Quote.Err
	// without taking down the whole call.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID json.RawMessage `json:"id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      req.ID,
			"error": map[string]any{
				"code":    -32000,
				"message": "execution reverted",
			},
		})
	}))
	defer srv.Close()

	client, err := NewClient(srv.URL)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer client.Close()

	quotes, err := client.EffectivePrices(context.Background(), PoolETHUSDC03, []decimal.Decimal{dec("1")})
	if err != nil {
		t.Fatalf("top-level error: %v", err)
	}
	if quotes.Sell[0].Err == nil {
		t.Errorf("expected Sell[0].Err to be set, got nil")
	}
	if quotes.Buy[0].Err == nil {
		t.Errorf("expected Buy[0].Err to be set, got nil")
	}
}
