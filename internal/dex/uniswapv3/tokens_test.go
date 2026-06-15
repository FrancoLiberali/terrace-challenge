package uniswapv3

import (
	"strings"
	"testing"
)

// TestPoolAddress verifies the CREATE2 derivation against the
// canonical mainnet pool addresses Uniswap publishes for the three
// ETH-USDC fee tiers. If any of these change, either Uniswap has
// redeployed (unlikely) or we've broken the derivation.
func TestPoolAddress(t *testing.T) {
	cases := []struct {
		name string
		fee  uint32
		want string
	}{
		{"ETH-USDC 0.05%", 500, "0x88e6A0c2dDD26FEEb64F039a2c41296FcB3f5640"},
		{"ETH-USDC 0.3%", 3000, "0x8ad599c3A0ff1De082011EFDDc58f1908eb6e6D8"},
		{"ETH-USDC 1%", 10000, "0x7BeA39867e4169DBe237d55C8242a8f2fcDcc387"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := PoolAddress(UniswapV3FactoryMainnet, WETH, USDC, c.fee).Hex()
			if !strings.EqualFold(got, c.want) {
				t.Errorf("fee=%d: got %s, want %s", c.fee, got, c.want)
			}
		})
	}
}

// TestPoolAddress_BaseQuoteOrderingIndependent confirms the helper is
// symmetric in base/quote — V3 sorts tokens inside the salt, so
// PoolAddress(f, A, B, fee) == PoolAddress(f, B, A, fee).
func TestPoolAddress_BaseQuoteOrderingIndependent(t *testing.T) {
	a := PoolAddress(UniswapV3FactoryMainnet, WETH, USDC, 3000)
	b := PoolAddress(UniswapV3FactoryMainnet, USDC, WETH, 3000)
	if a != b {
		t.Errorf("base/quote ordering must not matter: WETH-USDC=%s, USDC-WETH=%s", a, b)
	}
}
