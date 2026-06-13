package uniswapv3

import (
	"math/big"

	"github.com/shopspring/decimal"
)

// toRawAmount converts a human-readable decimal amount into the raw integer
// representation an ERC-20 contract uses, given the token's decimal precision.
// Example: toRawAmount(1.5, 18) == 1_500_000_000_000_000_000.
func toRawAmount(amount decimal.Decimal, decimals uint8) *big.Int {
	return amount.Shift(int32(decimals)).BigInt()
}

// fromRawAmount converts a raw ERC-20 amount back into a human-readable
// decimal, given the token's decimal precision.
// Example: fromRawAmount(1_500_000_000, 6) == 1500.
func fromRawAmount(raw *big.Int, decimals uint8) decimal.Decimal {
	return decimal.NewFromBigInt(raw, 0).Shift(-int32(decimals))
}
