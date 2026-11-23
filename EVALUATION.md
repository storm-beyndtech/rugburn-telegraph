# Evaluation

## Calibration set

The reported evaluation used 150 Solana preference pairs from the pool, with all pairs eligible and none rejected. Pair verdicts were attested to the [Mongardini & Mei meme-coin manipulation dataset on Zenodo](https://zenodo.org/records/17830944); each pair was reviewed by hand.

The prior 40% / 15% cutoffs agreed with pair verdicts 51% of the time. The calibrated live cutoffs (70% / 35%) agreed 83% of the time (81% under repeated cross-validation), with reported flagged precision of 0.97, recall of 0.96, and no organic cases called wash among 34 organic cases.

## Hold-out

On the last 30 archive days (227 token-days), reported agreement increased from 43% to 76%.

## Caveats

- The pair verdicts and calibration data come from the same archive, so the 83% agreement is partly circular.
- Calibration used daily per-wallet totals as a stand-in for the live 10-minute matcher.
- The live endpoint uses recent on-chain transactions, can have thin or unpriced samples, and returns `inconclusive` when coverage is insufficient.
- LSTs (including JitoSOL and mSOL) and USDC are known limits: their traffic is often routing/arbitrage or against pairs Rugburn does not price yet. In the observed USDC re-run, only 11 of 44 swaps were priced in a 7-second window, below the 20-swap minimum.

An inconclusive result is a coverage outcome, not a clean bill of health.

