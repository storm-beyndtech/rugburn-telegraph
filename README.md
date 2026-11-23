# Rugburn Telegraph wash-trading miner

This is the focused showcase repository for Rugburn's Telegraph miner, `rugburn-wash-trading` (registration ID 6149). The miner is registered and serves `WASH_TRADING_DETECTION_GENERAL` on Solana through Telegraph.

The source snapshot was copied from Rugburn `main` at commit `1b5807924f0c33355bb252c437ae75db5282627f`:

- `source/rugburn-worker/internal/api/telegraph_wash.go` — authenticated Telegraph endpoint, scan reuse, sample orchestration, response and cost reporting.
- `source/rugburn-worker/internal/scanner/wash.go` — swap extraction, paging, pricing, first-funder enrichment and round-trip matching.
- `source/rugburn-worker/internal/api/telegraph_preflight.go` — the related HOLD / REVIEW / REJECT preflight endpoint.
- `tests/` — corresponding worker tests copied from the same commit.

These files are an inspectable snapshot of code integrated into Rugburn's Go worker. This draft is not a standalone server and its tests require the full worker module and its other internal packages. The live miner is tested through Telegraph; no Rugburn service credentials are needed by testers.

## What the miner measures

For each priced swap in a bounded sample, the matcher looks for an opposite-side swap by the same wallet or a wallet with a recorded shared first funder. The legs must be within 25% of one another and occur within 10 minutes. `wash_volume_pct` is matched round-trip volume divided by total priced swap volume.

Verdicts use calibrated cutoffs: `wash_trading` at 70% or above, `likely_wash_trading` at 35% or above, and `organic` below 35%. A verdict is `inconclusive` unless the sample has at least 20 priced swaps from at least 5 wallets. The matcher reports its coverage and leaves unpriced swaps out of the percentage.

The measure describes a transaction pattern. It does not establish intent or identify who controls a wallet. Fast legitimate trading can produce similar round trips; missing first-funder data makes shared-funder matching a lower bound.

## Calibration and limits

See [EVALUATION.md](EVALUATION.md) for the reported pair-pool and hold-out results, their caveats, and the known limits for LSTs and USDC. The cited source is the Mongardini & Mei [meme-coin manipulation dataset on Zenodo](https://zenodo.org/records/17830944).

## Try the live miner

Use Telegraph's miner interface and select miner 6149, `rugburn-wash-trading`. Calls cost $0.01. Cached answers are free. [TESTING.md](TESTING.md) has a short self-test and a tester feedback template.

## Burn Workstation

Burn Workstation is a separate investigation app built as a fork of OpenCode. The local checkout has not yet been connected to Telegraph or miner 6149, so this showcase does not describe Burn as a current miner client. See [APP-INTEGRATION-DECISION.md](APP-INTEGRATION-DECISION.md) for the recommended boundary and next step.

## Publication status

The source snapshot and documentation are released under the MIT License; see [LICENSE](LICENSE). The existing Rugburn repository remains private. This focused repository does not contain Burn Workstation code.

