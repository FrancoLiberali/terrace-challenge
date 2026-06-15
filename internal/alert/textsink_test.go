package alert

import (
	"bytes"
	"io"
	"log/slog"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/shopspring/decimal"

	"github.com/FrancoLiberali/terrace-challenge/internal/arbitrage"
	"github.com/FrancoLiberali/terrace-challenge/internal/chain"
	"github.com/FrancoLiberali/terrace-challenge/internal/pathfinder"
)

// testPoolAddress is the canonical ETH-USDC 0.3% pool address. Used by
// pretty-output tests to verify the DEX leg's "Pool:" sub-bullet
// matches what arbd would actually compute at startup.
var testPoolAddress = common.HexToAddress("0x8ad599c3A0ff1De082011EFDDc58f1908eb6e6D8")

// sampleOpportunity is a deterministic opportunity used as fixture
// across tests. Values are picked so format assertions can grep for
// known substrings.
func sampleOpportunity() arbitrage.Opportunity {
	return arbitrage.Opportunity{
		CandidatePath: pathfinder.CandidatePath{
			Block: chain.BlockEvent{
				Number:    12345,
				Timestamp: time.Unix(1_700_000_000, 0).UTC(),
				BaseFee:   big.NewInt(5_000_000_000), // 5 gwei
			},
			Size:        decimal.NewFromInt(10),
			BuyVenue:    "binance",
			SellVenue:   "uniswap",
			BuyPrice:    decimal.RequireFromString("1680.00"),
			SellPrice:   decimal.RequireFromString("1690.00"),
			GasEstimate: 150_000,
		},
		SpreadPerUnit: decimal.RequireFromString("10.00"),
		GrossProfit:   decimal.RequireFromString("100.00"),
		GasCostUSDC:   decimal.RequireFromString("1.26"),
		NetProfit:     decimal.RequireFromString("98.74"),
		NetProfitPct:  decimal.RequireFromString("0.5878"),
		CapitalUSDC:   decimal.RequireFromString("16800.00"),
	}
}

func TestTextSink_EmitsStructuredEvent(t *testing.T) {
	var logBuf bytes.Buffer
	sink := &TextSink{
		Logger: slog.New(slog.NewTextHandler(&logBuf, nil)),
		Out:    io.Discard,
		Pretty: false,
	}
	sink.Emit(sampleOpportunity())

	got := logBuf.String()
	for _, want := range []string{
		`msg="arbitrage opportunity detected"`,
		"block=12345",
		"buy_venue=binance",
		"sell_venue=uniswap",
		"size_eth=10",
		"net_profit_usdc=98.74",
		"capital_usdc=16800.00",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("slog output missing %q\nfull output: %s", want, got)
		}
	}
}

func TestTextSink_PrettyOutputContainsExpectedSections(t *testing.T) {
	var logBuf, outBuf bytes.Buffer
	sink := &TextSink{
		Logger:             slog.New(slog.NewTextHandler(&logBuf, nil)),
		Out:                &outBuf,
		Pretty:             true,
		UniswapVenue:       "uniswap",
		UniswapPoolAddress: testPoolAddress,
	}
	sink.Emit(sampleOpportunity())

	got := outBuf.String()
	for _, want := range []string{
		"ARBITRAGE OPPORTUNITY DETECTED",
		"Block Number: 12345",
		"Direction:    binance → uniswap",
		"Trade Size:        10 ETH",
		"Net Profit:        $98.74",
		"Capital Required:  $16800.00 USDC",
		"Execution Steps:",
		"baseFee=5.000 gwei",
		// Sell leg is on Uniswap in the fixture; spec format applies.
		"Execute Uniswap V3 swap: 10 ETH → USDC",
		"Pool: " + testPoolAddress.Hex(),
		"Expected output: ~$16900.00 USDC",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("pretty output missing %q\nfull output:\n%s", want, got)
		}
	}
}

func TestTextSink_PrettyOffSuppressesOnStdoutChannel(t *testing.T) {
	var outBuf bytes.Buffer
	sink := &TextSink{
		Logger: slog.New(slog.DiscardHandler),
		Out:    &outBuf,
		Pretty: false,
	}
	sink.Emit(sampleOpportunity())

	if outBuf.Len() != 0 {
		t.Errorf("pretty=false should write nothing to Out, got: %q", outBuf.String())
	}
}
