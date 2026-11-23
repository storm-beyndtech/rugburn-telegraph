Showcase, Rugburn, Mission 14 (Web3 Native & DeFi), Evaluation + Provider tracks, Sepolia testnet.

An agent about to buy a Solana token can't tell real volume from cycled volume. Rugburn sells that answer on Telegraph as WASH_TRADING_DETECTION_GENERAL, and we used our own evaluation work to make it sharper.

**Evaluation:** 150 Solana preference pairs in the pool, all eligible (0 rejected), attested to the Mongardini & Mei archive ([Zenodo 17830944](https://zenodo.org/records/17830944)), each reviewed by hand.

**Calibration:** We fit our miner's cutoffs against those pairs. Old cutoffs (40/15) agreed with the pair verdicts 51% of the time and pushed most "likely wash" cases to full wash. After calibrating against the pool, the live cutoffs (70/35) agree 83% (81% cross-validated), flagged precision 0.97, recall 0.96, and 0 of 34 organic cases called wash. On a hold-out of the last 30 archive days (227 token-days) agreement rose from 43% to 76%. Caveat: the pair verdicts come from the same archive, so the 83% is partly circular, and calibration used daily per-wallet totals as a stand-in for the live 10-minute matcher.

**Miner:** registrationId 6149, active, $0.01 per call. Each answer reads up to 300 recent transactions from the pool and the mint, traces first funders, and returns a verdict and wash_volume_pct with evidence split into observed, derived and hypothesis, a lower-bound note when funding coverage is thin, and its own compute cost. Answers cache for 30 minutes; if the daily data budget runs out it returns the last answer marked stale rather than failing. It never claims who controls a wallet, only how the volume moved.

**Known limits:** For liquid staking tokens (JitoSOL, mSOL) and USDC it returns inconclusive, not a verdict. Most of their pool traffic is routing and arbitrage that leaves the token unchanged, or pairs we don't price yet, so a sample holds too few real trades. An honest "inconclusive" beats a wrong flag; deeper sampling is next.

**Next:** The token-risk preflight that buys this check alongside audit and on-chain verification and returns HOLD / REVIEW / REJECT with receipts.

**Source:** [Rugburn Telegraph repo](https://github.com/storm-beyndtech/rugburn-telegraph)

