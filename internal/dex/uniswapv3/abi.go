package uniswapv3

// quoterV2ABI is the minimal subset of QuoterV2's ABI this package uses —
// just the two simulated-swap functions. Full ABI:
// https://github.com/Uniswap/v3-periphery/blob/main/contracts/lens/QuoterV2.sol
const quoterV2ABI = `[
  {
    "inputs": [
      {
        "components": [
          {"name": "tokenIn",           "type": "address"},
          {"name": "tokenOut",          "type": "address"},
          {"name": "amountIn",          "type": "uint256"},
          {"name": "fee",               "type": "uint24"},
          {"name": "sqrtPriceLimitX96", "type": "uint160"}
        ],
        "name": "params",
        "type": "tuple"
      }
    ],
    "name": "quoteExactInputSingle",
    "outputs": [
      {"name": "amountOut",               "type": "uint256"},
      {"name": "sqrtPriceX96After",       "type": "uint160"},
      {"name": "initializedTicksCrossed", "type": "uint32"},
      {"name": "gasEstimate",             "type": "uint256"}
    ],
    "stateMutability": "nonpayable",
    "type": "function"
  },
  {
    "inputs": [
      {
        "components": [
          {"name": "tokenIn",           "type": "address"},
          {"name": "tokenOut",          "type": "address"},
          {"name": "amount",            "type": "uint256"},
          {"name": "fee",               "type": "uint24"},
          {"name": "sqrtPriceLimitX96", "type": "uint160"}
        ],
        "name": "params",
        "type": "tuple"
      }
    ],
    "name": "quoteExactOutputSingle",
    "outputs": [
      {"name": "amountIn",                "type": "uint256"},
      {"name": "sqrtPriceX96After",       "type": "uint160"},
      {"name": "initializedTicksCrossed", "type": "uint32"},
      {"name": "gasEstimate",             "type": "uint256"}
    ],
    "stateMutability": "nonpayable",
    "type": "function"
  }
]`
