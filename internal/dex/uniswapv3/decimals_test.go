package uniswapv3

import (
	"math/big"
	"testing"

	"github.com/shopspring/decimal"
)

func dec(s string) decimal.Decimal { return decimal.RequireFromString(s) }

func TestToRawAmount(t *testing.T) {
	cases := []struct {
		name     string
		amount   string
		decimals uint8
		want     string // big.Int as decimal string
	}{
		{"1 WETH (18 decimals)", "1", 18, "1000000000000000000"},
		{"1.5 WETH (18 decimals)", "1.5", 18, "1500000000000000000"},
		{"1700 USDC (6 decimals)", "1700", 6, "1700000000"},
		{"0.000001 USDC (6 decimals)", "0.000001", 6, "1"},
		{"zero", "0", 18, "0"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := toRawAmount(dec(c.amount), c.decimals)
			want, _ := new(big.Int).SetString(c.want, 10)
			if got.Cmp(want) != 0 {
				t.Errorf("toRawAmount: got %s, want %s", got, want)
			}
		})
	}
}

func TestFromRawAmount(t *testing.T) {
	cases := []struct {
		name     string
		raw      string
		decimals uint8
		want     string
	}{
		{"1 WETH raw → 1", "1000000000000000000", 18, "1"},
		{"1.5 WETH raw → 1.5", "1500000000000000000", 18, "1.5"},
		{"1700 USDC raw → 1700", "1700000000", 6, "1700"},
		{"1 wei USDC → 0.000001", "1", 6, "0.000001"},
		{"zero", "0", 18, "0"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			raw, _ := new(big.Int).SetString(c.raw, 10)
			got := fromRawAmount(raw, c.decimals)
			if !got.Equal(dec(c.want)) {
				t.Errorf("fromRawAmount: got %s, want %s", got, c.want)
			}
		})
	}
}

// TestRoundTrip checks toRawAmount + fromRawAmount cancel for the
// precisions this package supports (USDC: 6 decimals, WETH: 18).
func TestRoundTrip(t *testing.T) {
	for _, in := range []string{"1", "10", "100", "1234.5678", "0.000001"} {
		for _, decimals := range []uint8{6, 18} {
			amount := dec(in)
			got := fromRawAmount(toRawAmount(amount, decimals), decimals)
			if !got.Equal(amount) {
				t.Errorf("round trip (%s, decimals=%d): got %s, want %s", in, decimals, got, amount)
			}
		}
	}
}
