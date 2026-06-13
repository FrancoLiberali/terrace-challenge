// Package uniswapv3 integrates with Uniswap V3's QuoterV2 contract on
// Ethereum mainnet, simulating swaps to compute slippage-aware effective
// per-unit prices for a list of trade sizes. It carries no resilience
// concerns (rate limiting, circuit breaking, retries): those are applied
// as wrappers around it.
package uniswapv3

import "github.com/ethereum/go-ethereum/common"

// Token identifies an ERC-20 token by its mainnet address and the decimal
// precision the contract uses for raw integer amounts.
type Token struct {
	Address  common.Address
	Decimals uint8
}

// Protocol-baked decimal precisions for the tokens this package targets.
// Each value is fixed by the token's deployed contract.
const (
	decimalsWETH uint8 = 18
	decimalsUSDC uint8 = 6
)

// Mainnet token constants used by this package.
var (
	WETH = Token{
		Address:  common.HexToAddress("0xC02aaA39b223FE8D0A0e5C4F27eAD9083C756Cc2"),
		Decimals: decimalsWETH,
	}
	USDC = Token{
		Address:  common.HexToAddress("0xA0b86991c6218b36c1d19D4a2e9Eb0cE3606eB48"),
		Decimals: decimalsUSDC,
	}
)

// Pool identifies a Uniswap V3 pool by its two tokens and fee tier (in
// millionths — 3000 = 0.3%). Base is the asset the configured trade sizes
// are denominated in; Quote is the per-unit pricing token. QuoterV2
// derives the pool address from (Base, Quote, Fee), so we don't carry the
// pool address explicitly.
type Pool struct {
	Base  Token
	Quote Token
	Fee   uint32
}

// Uniswap V3 fee tiers in millionths of a unit (the contract's encoding).
// 3000 = 3000/1_000_000 = 0.3%.
const feeTier03Percent uint32 = 3000

// PoolETHUSDC03 targets the 0.3% fee-tier ETH-USDC pool — the one the
// challenge implies and that limitations.md §8 documents as a deliberate
// single-pool simplification.
var PoolETHUSDC03 = Pool{
	Base:  WETH,
	Quote: USDC,
	Fee:   feeTier03Percent,
}
