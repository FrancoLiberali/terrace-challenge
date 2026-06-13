package binance

import (
	"errors"
	"testing"

	"github.com/shopspring/decimal"
)

func dec(s string) decimal.Decimal {
	return decimal.RequireFromString(s)
}

func TestWalkOrderbook(t *testing.T) {
	cases := []struct {
		name          string
		levels        []level
		size          string
		wantEffective string
		wantTotalCost string
		wantErr       error
	}{
		{
			name:          "exact match single level",
			levels:        []level{{price: dec("2250.10"), size: dec("10")}},
			size:          "10",
			wantEffective: "2250.10",
			wantTotalCost: "22501",
		},
		{
			name:          "partial consumption of first level",
			levels:        []level{{price: dec("2250.10"), size: dec("10")}},
			size:          "3",
			wantEffective: "2250.10",
			wantTotalCost: "6750.30",
		},
		{
			// 3.5 ETH at 2250.10 = 7875.35; 6.5 ETH at 2250.20 = 14626.30.
			// Total = 22501.65 over 10 ETH → 2250.165 per ETH.
			name: "crossing multiple levels",
			levels: []level{
				{price: dec("2250.10"), size: dec("3.5")},
				{price: dec("2250.20"), size: dec("7.0")},
			},
			size:          "10",
			wantEffective: "2250.165",
			wantTotalCost: "22501.65",
		},
		{
			name: "insufficient depth",
			levels: []level{
				{price: dec("2250.10"), size: dec("3")},
				{price: dec("2250.20"), size: dec("2")},
			},
			size:    "10",
			wantErr: ErrInsufficientDepth,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			eff, cost, err := walkOrderbook(tc.levels, dec(tc.size))
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err: got %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !eff.Equal(dec(tc.wantEffective)) {
				t.Errorf("effective price: got %s, want %s", eff, tc.wantEffective)
			}
			if !cost.Equal(dec(tc.wantTotalCost)) {
				t.Errorf("total cost: got %s, want %s", cost, tc.wantTotalCost)
			}
		})
	}
}

func TestWalkOrderbook_RejectsNonPositiveSize(t *testing.T) {
	levels := []level{{price: dec("2250"), size: dec("1")}}
	for _, size := range []string{"0", "-1"} {
		t.Run("size="+size, func(t *testing.T) {
			_, _, err := walkOrderbook(levels, dec(size))
			if err == nil {
				t.Fatal("expected an error, got nil")
			}
			if errors.Is(err, ErrInsufficientDepth) {
				t.Fatalf("non-positive size should not return ErrInsufficientDepth, got %v", err)
			}
		})
	}
}
