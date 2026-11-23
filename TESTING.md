# Testing miner 6149

## Self-test

Use Telegraph's miner interface to call `rugburn-wash-trading` (registration ID 6149). Each non-cached call costs $0.01; avoid unnecessary repeats.

1. Try one known wash example and one organic example from the evaluation set.
2. Try a thin-coverage example such as USDC, where an inconclusive result is expected when fewer than 20 swaps are priced.
3. For each response, confirm the subject and `as_of` timestamp, inspect `verdict` and `wash_volume_pct`, and check `signals` for `swaps_observed`, `sol_priced_swaps`, `unpriced_swaps`, distinct wallets, window, pages and stop reason.
4. Check that a thin sample returns `inconclusive` instead of a confident wash percentage. Do not interpret the result as proof of wallet ownership or intent.

Do not put API keys, signatures, or other credentials in issues, screenshots, or shared test results.

## Tester feedback template

```text
Miner: 6149 (rugburn-wash-trading)
Token/mint:
Expected behavior (if known):
Verdict:
wash_volume_pct:
Priced swaps / observed swaps:
Distinct wallets:
Was the evidence useful or confusing?
Anything that looked wrong:
Approximate test time (WAT):
```

## Draft tester request

> Could a few people test my Telegraph miner? Miner 6149, `rugburn-wash-trading`, checks recent Solana swaps for same-wallet or shared-funder round trips and reports a verdict with sample evidence. Calls cost $0.01. Please try one token you expect to be wash-traded and one you expect to be organic; if possible, also try a thin-coverage token such as USDC. Share the token, verdict, priced swap count, and anything confusing or suspicious in the evidence. The source snapshot and evaluation notes will be linked here. Thin coverage may correctly return inconclusive, especially for USDC and LSTs.

