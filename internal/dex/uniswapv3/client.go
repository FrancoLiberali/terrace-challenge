package uniswapv3

import (
	"context"
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/shopspring/decimal"
)

// QuoterV2Address is the deployed QuoterV2 contract on Ethereum mainnet.
var QuoterV2Address = common.HexToAddress("0x61fFE014bA17989E743c5F6cB21bF9697530B21e")

// Client wraps an Ethereum RPC client to issue QuoterV2 simulated-swap
// calls. It is intentionally thin: no rate limiting, no retries, no
// circuit breaking — those concerns belong to wrapper layers above the
// adapter.
type Client struct {
	eth    *ethclient.Client
	abi    abi.ABI
	quoter common.Address
}

// NewClient dials the given Ethereum JSON-RPC endpoint and parses the
// QuoterV2 ABI once for reuse across calls.
func NewClient(rpcURL string) (*Client, error) {
	eth, err := ethclient.Dial(rpcURL)
	if err != nil {
		return nil, fmt.Errorf("dial RPC: %w", err)
	}
	parsed, err := abi.JSON(strings.NewReader(quoterV2ABI))
	if err != nil {
		eth.Close()
		return nil, fmt.Errorf("parse QuoterV2 ABI: %w", err)
	}
	return &Client{eth: eth, abi: parsed, quoter: QuoterV2Address}, nil
}

// Close releases the underlying RPC connection.
func (c *Client) Close() { c.eth.Close() }

// EffectivePrices returns the slippage-aware effective per-unit price for
// each (size, side) combination against the given pool's current state.
//
// Buy[i] and Sell[i] in the returned Quotes both refer to sizes[i]:
//   - Buy[i] simulates spending Quote token to receive exactly sizes[i] of
//     Base, via QuoterV2.quoteExactOutputSingle. Price = amountIn / size.
//   - Sell[i] simulates sending exactly sizes[i] of Base and receiving
//     Quote, via QuoterV2.quoteExactInputSingle. Price = amountOut / size.
//
// Per-row failures (e.g., the pool reverting because it cannot service the
// requested size) are recorded in Quote.Err; the top-level error is
// returned only if the input cannot be processed at all.
func (c *Client) EffectivePrices(ctx context.Context, pool Pool, sizes []decimal.Decimal) (Quotes, error) {
	out := Quotes{
		Buy:  make([]Quote, len(sizes)),
		Sell: make([]Quote, len(sizes)),
	}
	for i, size := range sizes {
		out.Buy[i] = c.quoteBuy(ctx, pool, size)
		out.Sell[i] = c.quoteSell(ctx, pool, size)
	}
	return out, nil
}

// exactInputParams matches QuoterV2.quoteExactInputSingle's tuple input.
type exactInputParams struct {
	TokenIn           common.Address `abi:"tokenIn"`
	TokenOut          common.Address `abi:"tokenOut"`
	AmountIn          *big.Int       `abi:"amountIn"`
	Fee               *big.Int       `abi:"fee"`
	SqrtPriceLimitX96 *big.Int       `abi:"sqrtPriceLimitX96"`
}

// exactOutputParams matches QuoterV2.quoteExactOutputSingle's tuple input.
type exactOutputParams struct {
	TokenIn           common.Address `abi:"tokenIn"`
	TokenOut          common.Address `abi:"tokenOut"`
	Amount            *big.Int       `abi:"amount"`
	Fee               *big.Int       `abi:"fee"`
	SqrtPriceLimitX96 *big.Int       `abi:"sqrtPriceLimitX96"`
}

// quoteSell simulates sending `size` Base units and computes the
// effective per-unit Quote price from the resulting amountOut.
func (c *Client) quoteSell(ctx context.Context, pool Pool, size decimal.Decimal) Quote {
	return c.quote(ctx, Sell, size, pool.Quote.Decimals, "quoteExactInputSingle", exactInputParams{
		TokenIn:           pool.Base.Address,
		TokenOut:          pool.Quote.Address,
		AmountIn:          toRawAmount(size, pool.Base.Decimals),
		Fee:               big.NewInt(int64(pool.Fee)),
		SqrtPriceLimitX96: new(big.Int),
	})
}

// quoteBuy simulates the Quote token cost of receiving exactly `size`
// Base units and computes the effective per-unit Quote price from the
// resulting amountIn.
func (c *Client) quoteBuy(ctx context.Context, pool Pool, size decimal.Decimal) Quote {
	return c.quote(ctx, Buy, size, pool.Quote.Decimals, "quoteExactOutputSingle", exactOutputParams{
		TokenIn:           pool.Quote.Address,
		TokenOut:          pool.Base.Address,
		Amount:            toRawAmount(size, pool.Base.Decimals),
		Fee:               big.NewInt(int64(pool.Fee)),
		SqrtPriceLimitX96: new(big.Int),
	})
}

// quote shares the call → unpack → price-math path between Buy and Sell.
// QuoterV2's two functions return the load-bearing value (amountOut for
// exactInput, amountIn for exactOutput) in the same first output slot, so
// both directions reduce to "first big.Int divided by size, denominated in
// quote decimals."
func (c *Client) quote(ctx context.Context, side Side, size decimal.Decimal, quoteDecimals uint8, method string, params any) Quote {
	raw, err := c.call(ctx, method, params)
	if err != nil {
		return Quote{Size: size, Side: side, Err: err}
	}
	primary, ok := raw[0].(*big.Int)
	if !ok {
		return Quote{Size: size, Side: side, Err: fmt.Errorf("unexpected primary output type %T", raw[0])}
	}
	price := fromRawAmount(primary, quoteDecimals).Div(size)
	return Quote{Size: size, Side: side, Price: price}
}

// call packs the given method's params, fires an eth_call to QuoterV2 at
// latest state, and returns the unpacked outputs.
func (c *Client) call(ctx context.Context, method string, params any) ([]any, error) {
	data, err := c.abi.Pack(method, params)
	if err != nil {
		return nil, fmt.Errorf("pack %s: %w", method, err)
	}
	raw, err := c.eth.CallContract(ctx, ethereum.CallMsg{To: &c.quoter, Data: data}, nil)
	if err != nil {
		return nil, fmt.Errorf("eth_call %s: %w", method, err)
	}
	out, err := c.abi.Unpack(method, raw)
	if err != nil {
		return nil, fmt.Errorf("unpack %s: %w", method, err)
	}
	return out, nil
}
