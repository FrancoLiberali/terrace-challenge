// Package uniswapv3 integrates with Uniswap V3's QuoterV2 contract on
// Ethereum mainnet, simulating swaps to compute slippage-aware effective
// per-unit prices for a list of trade sizes. It carries no resilience
// concerns (rate limiting, circuit breaking, retries): those are applied
// as wrappers around it.
package uniswapv3

import (
	"bytes"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

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

// UniswapV3FactoryMainnet is the canonical V3 factory deployment on
// Ethereum mainnet. PoolAddress takes a factory address so the same
// derivation works on any chain where the V3 protocol is deployed.
var UniswapV3FactoryMainnet = common.HexToAddress("0x1F98431c8aD98523631AE4a59f267346ea31F984")

// poolInitCodeHash is the CREATE2 init-code hash for Uniswap V3 pool
// contracts — a protocol-level constant, stable across V3 deployments.
var poolInitCodeHash = common.HexToHash("0xe34f199b19b2b4f47f68442619d555527d244f78a3297ea89325f843f87b8b54")

// evmWord is the EVM's native word size in bytes. CREATE2 inputs are
// each left-padded to one EVM word inside the salt's preimage.
const evmWord = 32

// PoolAddress computes the deterministic V3 pool address for the
// (factory, base, quote, fee) tuple via the standard CREATE2 formula.
// V3 sorts tokens lexicographically inside the salt, so the returned
// address is the same regardless of base/quote ordering.
//
// No RPC call required — V3 pool addresses are a pure function of the
// factory + sorted token pair + fee tier.
func PoolAddress(factory common.Address, base, quote Token, fee uint32) common.Address {
	token0, token1 := base.Address, quote.Address
	if bytes.Compare(token0.Bytes(), token1.Bytes()) > 0 {
		token0, token1 = token1, token0
	}
	saltHash := crypto.Keccak256Hash(
		common.LeftPadBytes(token0.Bytes(), evmWord),
		common.LeftPadBytes(token1.Bytes(), evmWord),
		common.LeftPadBytes(big.NewInt(int64(fee)).Bytes(), evmWord),
	)
	var salt [32]byte
	copy(salt[:], saltHash[:])
	return crypto.CreateAddress2(factory, salt, poolInitCodeHash[:])
}
