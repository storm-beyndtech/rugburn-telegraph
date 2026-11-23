package scanner

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"rugburn-worker/internal/config"
)

// Wash-share measurement for the WASH_TRADING_DETECTION intent.
//
// What it measures: the share of SOL swap volume, in one bounded window of the
// pool's recent swaps, that is matched by an opposite swap of similar size from
// the same wallet, or from another wallet that shares a recorded first funder,
// within a short time gap. A buy that is closed out by a near-equal sell minutes
// later, by the same purse, moves price and volume without changing who holds
// the token. That is the mechanical footprint of wash trading.
//
// What it does not measure: intent. A fast flipper who buys and sells the same
// size also matches. Clusters exist only where a funding edge is already in the
// graph, so missing edges make the cluster share a lower bound. Swaps quoted in
// anything other than SOL are not priced and are left out of both sides.

// WashSwap is one priced or unpriced swap of the subject token.
type WashSwap struct {
	Signature string
	Wallet    string
	Side      string // buy | sell
	SOL       float64
	// Quote is the leg the swap was priced from: "sol" (native SOL or wSOL),
	// "usd" (USDC/USDT converted at the sample's SOL/USD rate) or "" when the
	// swap could not be priced. Unpriced swaps are counted, never matched.
	Quote string
	Time  time.Time
	Slot  int64
}

// WashParams bounds the matcher. Defaults are deliberately strict.
type WashParams struct {
	MaxGap        time.Duration // longest time between the two legs of a round trip
	SizeTolerance float64       // |a-b| / max(a,b) allowed between legs
	MinSwaps      int           // priced swaps needed before a share is reported
	MinWallets    int           // distinct wallets needed before a share is reported
	// Unmatched wallets still count toward total volume but are never matched
	// (multi-market traders, see WashMultiMarketCandidates).
	Unmatched map[string]bool
}

func DefaultWashParams() WashParams {
	return WashParams{MaxGap: 10 * time.Minute, SizeTolerance: 0.25, MinSwaps: 20, MinWallets: 5}
}

// WashEntity is one wallet or funder cluster that produced matched round trips.
type WashEntity struct {
	Kind       string   `json:"kind"` // wallet | funder_cluster
	Key        string   `json:"key"`
	Wallets    []string `json:"wallets"`
	Pairs      int      `json:"matched_pairs"`
	MatchedSOL float64  `json:"matched_volume_sol"`
}

// WashMeasurement keeps observed counts apart from the derived share.
type WashMeasurement struct {
	// Observed
	SwapsObserved int       `json:"swaps_observed"`
	PricedSwaps   int       `json:"priced_swaps"`
	UnpricedSwaps int       `json:"unpriced_swaps"`
	Wallets       int       `json:"wallets"`
	WindowStart   time.Time `json:"window_start"`
	WindowEnd     time.Time `json:"window_end"`
	TotalVolume   float64   `json:"total_volume_sol"`
	FunderKnown   int       `json:"wallets_with_known_funder"`
	PricedSOL     int       `json:"sol_quoted_swaps"`
	PricedUSD     int       `json:"usd_quoted_swaps"`
	// Derived
	SelfPairs         int          `json:"self_round_trip_pairs"`
	SelfMatchedSOL    float64      `json:"self_round_trip_volume_sol"`
	ClusterPairs      int          `json:"cluster_round_trip_pairs"`
	ClusterMatchedSOL float64      `json:"cluster_round_trip_volume_sol"`
	Clusters          int          `json:"funder_clusters"`
	LargestCluster    int          `json:"largest_funder_cluster"`
	WashVolumePct     *float64     `json:"wash_volume_pct"`
	TopEntities       []WashEntity `json:"top_entities"`
	UnmatchedWallets  int          `json:"unmatched_wallets"`
	UnmatchedSOL      float64      `json:"unmatched_volume_sol"`
	Sufficient        bool         `json:"sample_sufficient"`
	InsufficientWhy   string       `json:"insufficient_reason,omitempty"`
}

// ExtractWashSwaps turns enhanced transactions into swaps of mint, pricing
// only SOL/wSOL legs (no SOL/USD rate). See ExtractWashSwapsPriced.
func ExtractWashSwaps(txs []enhancedTransaction, mint string) []WashSwap {
	return ExtractWashSwapsPriced(txs, mint, 0)
}

// ExtractWashSwapsPriced turns enhanced transactions into swaps of mint.
// Failed transactions and swaps whose actor is a pool/program account are
// dropped. Each swap is priced from the actor's own quote leg (washQuoteSOL);
// solUSD > 0 lets USDC/USDT legs be converted to SOL.
func ExtractWashSwapsPriced(txs []enhancedTransaction, mint string, solUSD float64) []WashSwap {
	out, _ := extractWashSwaps(txs, mint, solUSD)
	return out
}

// washIsRouteHop reports whether actor both received and sent mint in tx and
// ended within 1% of where it started: the token only passed through, as in
// an aggregator multi-hop or an atomic arbitrage loop (SOL -> LST -> USDC ->
// SOL). That is not a trade of mint by actor. Helius often still summarises
// such a transaction as a swap of mint, and on LST/SOL pools these loops are
// most of the traffic.
func washIsRouteHop(tx enhancedTransaction, mint, actor string) bool {
	in, out := 0.0, 0.0
	for _, t := range tx.TokenTransfers {
		if t.Mint != mint || t.FromUserAccount == t.ToUserAccount {
			continue
		}
		if t.ToUserAccount == actor {
			in += t.TokenAmount
		}
		if t.FromUserAccount == actor {
			out += t.TokenAmount
		}
	}
	if in <= 0 || out <= 0 {
		return false
	}
	return math.Abs(in-out) <= 0.01*math.Max(in, out)
}

// extractWashSwaps is ExtractWashSwapsPriced that also counts the
// transactions dropped as route hops.
func extractWashSwaps(txs []enhancedTransaction, mint string, solUSD float64) ([]WashSwap, int) {
	hops := 0
	out := make([]WashSwap, 0, len(txs))
	seen := map[string]struct{}{}
	for _, tx := range txs {
		if tx.Signature == "" || tx.TransactionError != nil {
			continue
		}
		if _, dup := seen[tx.Signature]; dup {
			continue
		}
		side, actor, _, ok := classifySwapDirection(tx, mint)
		if !ok || actor == "" || isKnownLiquidityProgram(actor) {
			continue
		}
		seen[tx.Signature] = struct{}{}
		if washIsRouteHop(tx, mint, actor) {
			hops++
			continue
		}
		sol, quote := washQuoteSOL(tx, mint, side, actor, solUSD)
		out = append(out, WashSwap{
			Signature: tx.Signature,
			Wallet:    actor,
			Side:      side,
			SOL:       sol,
			Quote:     quote,
			Time:      timeFromUnix(tx.Timestamp),
			Slot:      tx.Slot,
		})
	}
	return out, hops
}

// Quote mints the wash matcher can price. USD stables are converted to SOL at
// one rate per sample, so a buy in USDC and a sell in SOL still compare.
const (
	washMintWSOL = "So11111111111111111111111111111111111111112"
	washMintUSDC = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"
	washMintUSDT = "Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB"
)

// washQuoteSOL prices one swap from the actor's quote leg only, in this order:
//  1. the swap event's native SOL input/output,
//  2. the swap event's top-level wSOL / USDC / USDT inputs (buy) or outputs (sell),
//  3. wSOL / USDC / USDT token transfers out of (buy) or into (sell) the actor,
//  4. native SOL moved between the actor and the account on the other side of
//     the subject-token transfer (bonding curves, unparsed pool swaps).
//
// It never sums every native transfer in the transaction. That fallback, which
// the behaviour indexer uses for its coarse volume, prices a USDC-quoted swap
// at its priority fee, Jito tip and rent, a few thousandths of a SOL, and those
// fake sizes then "match" each other as round trips.
func washQuoteSOL(tx enhancedTransaction, mint, side, actor string, solUSD float64) (float64, string) {
	if ev := tx.Events.Swap; ev != nil {
		native := 0.0
		if side == "buy" && ev.NativeInput != nil {
			native += lamportsStringToSOL(ev.NativeInput.Amount)
		}
		if side == "sell" && ev.NativeOutput != nil {
			native += lamportsStringToSOL(ev.NativeOutput.Amount)
		}
		if native > 0 {
			return native, "sol"
		}
		legs := ev.TokenInputs
		if side == "sell" {
			legs = ev.TokenOutputs
		}
		if v, q := washSumQuoteLegs(legs, mint, solUSD); v > 0 {
			return v, q
		}
	}
	solSum, usdSum := 0.0, 0.0
	for _, tt := range tx.TokenTransfers {
		mine := (side == "buy" && strings.EqualFold(tt.FromUserAccount, actor)) || (side == "sell" && strings.EqualFold(tt.ToUserAccount, actor))
		if !mine || strings.EqualFold(tt.Mint, mint) {
			continue
		}
		switch tt.Mint {
		case washMintWSOL:
			solSum += math.Abs(tt.TokenAmount)
		case washMintUSDC, washMintUSDT:
			usdSum += math.Abs(tt.TokenAmount)
		}
	}
	if solSum > 0 {
		return solSum, "sol"
	}
	if usdSum > 0 && solUSD > 0 {
		return usdSum / solUSD, "usd"
	}
	counterparties := map[string]struct{}{}
	for _, tt := range tx.TokenTransfers {
		if !strings.EqualFold(tt.Mint, mint) {
			continue
		}
		if side == "buy" && strings.EqualFold(tt.ToUserAccount, actor) && tt.FromUserAccount != "" {
			counterparties[tt.FromUserAccount] = struct{}{}
		}
		if side == "sell" && strings.EqualFold(tt.FromUserAccount, actor) && tt.ToUserAccount != "" {
			counterparties[tt.ToUserAccount] = struct{}{}
		}
	}
	native := 0.0
	for _, nt := range tx.NativeTransfers {
		if side == "buy" && strings.EqualFold(nt.FromUserAccount, actor) {
			if _, ok := counterparties[nt.ToUserAccount]; ok {
				native += math.Abs(nt.Amount) / 1e9
			}
		}
		if side == "sell" && strings.EqualFold(nt.ToUserAccount, actor) {
			if _, ok := counterparties[nt.FromUserAccount]; ok {
				native += math.Abs(nt.Amount) / 1e9
			}
		}
	}
	if native > 0 {
		return native, "sol"
	}
	return 0, ""
}

func washSumQuoteLegs(legs []enhancedSwapToken, mint string, solUSD float64) (float64, string) {
	solSum, usdSum := 0.0, 0.0
	for _, l := range legs {
		if strings.EqualFold(l.Mint, mint) {
			continue
		}
		amt := math.Abs(l.TokenAmount)
		if amt == 0 {
			amt = rawTokenAmountUI(l.RawTokenAmount)
		}
		switch l.Mint {
		case washMintWSOL:
			solSum += amt
		case washMintUSDC, washMintUSDT:
			usdSum += amt
		}
	}
	if solSum > 0 {
		return solSum, "sol"
	}
	if usdSum > 0 && solUSD > 0 {
		return usdSum / solUSD, "usd"
	}
	return 0, ""
}

// MeasureWash matches round trips per entity. clusterOf maps a wallet to its
// funder-cluster key; wallets absent from it (or alone in their cluster) are
// their own entity.
func MeasureWash(swaps []WashSwap, clusterOf map[string]string, p WashParams) WashMeasurement {
	m := WashMeasurement{SwapsObserved: len(swaps), TopEntities: []WashEntity{}}
	wallets := map[string]struct{}{}
	priced := make([]WashSwap, 0, len(swaps))
	for _, s := range swaps {
		wallets[s.Wallet] = struct{}{}
		if s.SOL <= 0 || s.Time.IsZero() {
			m.UnpricedSwaps++
			continue
		}
		priced = append(priced, s)
		if s.Quote == "usd" {
			m.PricedUSD++
		} else {
			m.PricedSOL++
		}
		m.TotalVolume += s.SOL
		if m.WindowStart.IsZero() || s.Time.Before(m.WindowStart) {
			m.WindowStart = s.Time
		}
		if s.Time.After(m.WindowEnd) {
			m.WindowEnd = s.Time
		}
	}
	m.PricedSwaps = len(priced)
	m.Wallets = len(wallets)
	for w := range wallets {
		if clusterOf[w] != "" {
			m.FunderKnown++
		}
	}

	// A cluster needs two or more wallets seen in this window.
	members := map[string][]string{}
	for w := range wallets {
		if p.Unmatched[w] {
			continue
		}
		if k := clusterOf[w]; k != "" {
			members[k] = append(members[k], w)
		}
	}
	entityOf := func(w string) (string, string) {
		if k := clusterOf[w]; k != "" && len(members[k]) >= 2 {
			return "cluster:" + k, "funder_cluster"
		}
		return "wallet:" + w, "wallet"
	}
	for _, ws := range members {
		if len(ws) >= 2 {
			m.Clusters++
			if len(ws) > m.LargestCluster {
				m.LargestCluster = len(ws)
			}
		}
	}

	byEntity := map[string][]WashSwap{}
	kindOf := map[string]string{}
	unmatched := map[string]bool{}
	for _, s := range priced {
		if p.Unmatched[s.Wallet] {
			unmatched[s.Wallet] = true
			m.UnmatchedSOL += s.SOL
			continue
		}
		key, kind := entityOf(s.Wallet)
		byEntity[key] = append(byEntity[key], s)
		kindOf[key] = kind
	}

	entities := []WashEntity{}
	for key, list := range byEntity {
		sort.SliceStable(list, func(i, j int) bool {
			if list[i].Time.Equal(list[j].Time) {
				return list[i].Slot < list[j].Slot
			}
			return list[i].Time.Before(list[j].Time)
		})
		used := make([]bool, len(list))
		ent := WashEntity{Kind: kindOf[key], Key: strings.SplitN(key, ":", 2)[1]}
		walletSet := map[string]struct{}{}
		for i := range list {
			if used[i] {
				continue
			}
			for j := i + 1; j < len(list); j++ {
				if used[j] || list[j].Side == list[i].Side {
					continue
				}
				if list[j].Time.Sub(list[i].Time) > p.MaxGap {
					break
				}
				if !sizesMatch(list[i].SOL, list[j].SOL, p.SizeTolerance) {
					continue
				}
				used[i], used[j] = true, true
				vol := list[i].SOL + list[j].SOL
				ent.Pairs++
				ent.MatchedSOL += vol
				walletSet[list[i].Wallet] = struct{}{}
				walletSet[list[j].Wallet] = struct{}{}
				if list[i].Wallet == list[j].Wallet {
					m.SelfPairs++
					m.SelfMatchedSOL += vol
				} else {
					m.ClusterPairs++
					m.ClusterMatchedSOL += vol
				}
				break
			}
		}
		if ent.Pairs > 0 {
			for w := range walletSet {
				ent.Wallets = append(ent.Wallets, w)
			}
			sort.Strings(ent.Wallets)
			entities = append(entities, ent)
		}
	}
	sort.Slice(entities, func(i, j int) bool { return entities[i].MatchedSOL > entities[j].MatchedSOL })
	if len(entities) > 5 {
		entities = entities[:5]
	}
	for i := range entities {
		entities[i].MatchedSOL = round6(entities[i].MatchedSOL)
	}
	m.TopEntities = entities
	m.UnmatchedWallets = len(unmatched)
	m.UnmatchedSOL = round6(m.UnmatchedSOL)
	m.TotalVolume = round6(m.TotalVolume)
	m.SelfMatchedSOL = round6(m.SelfMatchedSOL)
	m.ClusterMatchedSOL = round6(m.ClusterMatchedSOL)

	switch {
	case m.PricedSwaps < p.MinSwaps:
		m.InsufficientWhy = fmt.Sprintf("only %d priced swaps (SOL, wSOL, USDC or USDT legs) in the window; %d needed", m.PricedSwaps, p.MinSwaps)
	case m.Wallets < p.MinWallets:
		m.InsufficientWhy = fmt.Sprintf("only %d distinct wallets in the window; %d needed", m.Wallets, p.MinWallets)
	case m.TotalVolume <= 0:
		m.InsufficientWhy = "no SOL volume in the window"
	default:
		m.Sufficient = true
		pct := round6(math.Min(100, (m.SelfMatchedSOL+m.ClusterMatchedSOL)/m.TotalVolume*100))
		m.WashVolumePct = &pct
	}
	return m
}

func sizesMatch(a, b, tol float64) bool {
	hi := math.Max(a, b)
	if hi <= 0 {
		return false
	}
	return math.Abs(a-b)/hi <= tol
}

func round6(v float64) float64 { return math.Round(v*1e6) / 1e6 }

func rawTokenAmountUI(r enhancedRawTokenAmount) float64 {
	v, err := strconv.ParseFloat(strings.TrimSpace(r.TokenAmount), 64)
	if err != nil || v <= 0 {
		return 0
	}
	return v / math.Pow10(r.Decimals)
}

// ---------------------------------------------------------------------------
// Sample collection: bounded paging over the main pool and the mint.
//
// One page is the newest ≤100 enhanced transactions of one address. A busy
// token's 100 newest pool transactions are often under half swaps (fee claims,
// account opens/closes, transfers), so a single page left the matcher with
// 35-50 swaps. CollectWashSwaps walks back with before-signature, bounded by
// pages, time, budget and a lookback, over sources in priority order: the
// scan's main pool first (sees every swap on that pool, including Raydium v4
// legs that never reference the mint account), then the mint itself (swaps on
// the token's other pools and aggregator routes) once the pool's history inside
// the lookback is used up or the pool is unknown. Swaps are deduplicated by
// signature.
//
// Each source is only complete back to its own oldest fetched transaction, so
// the merged sample is trimmed to the window every source fully covers: the
// newest "oldest" among sources that still had history left. Without the trim
// one dense source and one sparse source would cover different stretches of
// time and the volume share would mix them.
// ---------------------------------------------------------------------------

// WashPageSize is Helius's maximum enhanced transactions per call.
const WashPageSize = 100

// WashFetchPlan bounds one sample collection.
type WashFetchPlan struct {
	Sources      []string      // addresses to read, in priority order (deduplicated)
	MaxPages     int           // total Helius calls across all sources
	Since        time.Time     // lookback floor; older transactions end a source
	TargetSwaps  int           // stop early once this many swaps are collected
	MinRemaining time.Duration // do not start a page with less time than this left
	SOLUSD       float64       // >0 lets USDC/USDT legs be priced
	// AllowPage is asked before every page (budget check). nil allows.
	AllowPage func(source string) bool
	// OnPage is told about every page attempt, for usage accounting.
	OnPage func(source string, txs int, err error)
}

// WashSourceCoverage reports what was read from one address.
type WashSourceCoverage struct {
	Address   string    `json:"address"`
	Pages     int       `json:"pages"`
	Txs       int       `json:"transactions"`
	Swaps     int       `json:"swaps_extracted"`
	Oldest    time.Time `json:"oldest_tx_time"`
	Exhausted bool      `json:"history_exhausted"`
	Error     string    `json:"error,omitempty"`
}

// WashSample is the merged, trimmed swap sample plus how it was obtained.
type WashSample struct {
	Swaps        []WashSwap           `json:"-"`
	Sources      []WashSourceCoverage `json:"sources"`
	Pages        int                  `json:"pages_fetched"`
	FailedPages  int                  `json:"pages_failed"`
	Cutoff       time.Time            `json:"common_window_start"`
	TrimmedSwaps int                  `json:"swaps_trimmed_outside_common_window"`
	RouteHops    int                  `json:"route_hops_skipped"`
	StopReason   string               `json:"stop_reason"`
	SOLUSD       float64              `json:"sol_usd_rate"`
}

// WashPageFetcher returns the newest ≤WashPageSize transactions of address
// strictly older than before (empty = newest), not older than since.
type WashPageFetcher func(ctx context.Context, address, before string, since time.Time) ([]enhancedTransaction, error)

type washSourceState struct {
	cov    WashSourceCoverage
	before string
	done   bool
}

// CollectWashSwaps runs the paging plan with an injected fetcher. It returns
// an error only when no page at all could be read.
func CollectWashSwaps(ctx context.Context, plan WashFetchPlan, mint string, fetch WashPageFetcher) (WashSample, error) {
	sample := WashSample{SOLUSD: plan.SOLUSD}
	states := []*washSourceState{}
	seenSrc := map[string]bool{}
	for _, a := range plan.Sources {
		a = strings.TrimSpace(a)
		if a == "" || seenSrc[a] {
			continue
		}
		seenSrc[a] = true
		states = append(states, &washSourceState{cov: WashSourceCoverage{Address: a}})
	}
	maxPages := plan.MaxPages
	if maxPages <= 0 {
		maxPages = 1
	}
	all := []WashSwap{}
	seenSig := map[string]struct{}{}
	var firstErr error

	for {
		if sample.Pages+sample.FailedPages >= maxPages {
			sample.StopReason = "page_limit"
			break
		}
		if dl, ok := ctx.Deadline(); ok && plan.MinRemaining > 0 && time.Until(dl) < plan.MinRemaining {
			sample.StopReason = "time_limit"
			break
		}
		if ctx.Err() != nil {
			sample.StopReason = "time_limit"
			break
		}
		next := pickWashSource(states)
		if next == nil {
			sample.StopReason = "history_exhausted"
			break
		}
		if plan.TargetSwaps > 0 && len(all) >= plan.TargetSwaps && next.cov.Pages > 0 {
			sample.StopReason = "target_reached"
			break
		}
		if plan.AllowPage != nil && !plan.AllowPage(next.cov.Address) {
			sample.StopReason = "budget"
			break
		}
		txs, err := fetch(ctx, next.cov.Address, next.before, plan.Since)
		if plan.OnPage != nil {
			plan.OnPage(next.cov.Address, len(txs), err)
		}
		if err != nil {
			sample.FailedPages++
			if firstErr == nil {
				firstErr = err
			}
			next.cov.Error = err.Error()
			// Stop reading this source. Its coverage ends where it ended; if it
			// never read a page it contributes nothing and does not trim.
			next.done = true
			continue
		}
		sample.Pages++
		next.cov.Pages++
		next.cov.Txs += len(txs)
		for _, tx := range txs {
			t := timeFromUnix(tx.Timestamp)
			if !t.IsZero() && (next.cov.Oldest.IsZero() || t.Before(next.cov.Oldest)) {
				next.cov.Oldest = t
			}
		}
		swaps, hops := extractWashSwaps(txs, mint, plan.SOLUSD)
		sample.RouteHops += hops
		for _, sw := range swaps {
			if _, dup := seenSig[sw.Signature]; dup {
				continue
			}
			seenSig[sw.Signature] = struct{}{}
			next.cov.Swaps++
			all = append(all, sw)
		}
		if len(txs) < WashPageSize || (!plan.Since.IsZero() && !next.cov.Oldest.IsZero() && !next.cov.Oldest.After(plan.Since)) {
			next.cov.Exhausted = true
			next.done = true
		} else {
			next.before = txs[len(txs)-1].Signature
		}
	}

	if sample.Pages == 0 && firstErr != nil {
		for _, st := range states {
			sample.Sources = append(sample.Sources, st.cov)
		}
		return sample, firstErr
	}

	// Common window: every source that read at least one page and still had
	// history left (cut by pages, time, budget or an error) is complete only
	// back to its oldest transaction.
	for _, st := range states {
		if st.cov.Pages > 0 && !st.cov.Exhausted && st.cov.Oldest.After(sample.Cutoff) {
			sample.Cutoff = st.cov.Oldest
		}
		sample.Sources = append(sample.Sources, st.cov)
	}
	if !sample.Cutoff.IsZero() {
		kept := all[:0]
		for _, sw := range all {
			if sw.Time.Before(sample.Cutoff) {
				sample.TrimmedSwaps++
				continue
			}
			kept = append(kept, sw)
		}
		all = kept
	}
	sample.Swaps = all
	return sample, nil
}

// pickWashSource reads sources strictly in priority order: the next source is
// only opened once the previous one is finished (history ended inside the
// lookback, or it failed). Measured on a pump token whose main pool was busy,
// 100 mint-feed transactions held 15 swaps against ~32 per 100 pool
// transactions, and a round-robin split forced the common-window trim to throw
// away a third of the pool's swaps, leaving a smaller sample than the pool
// alone. So the pool is primary and the mint feed fills in when the pool is
// thin, moved, or unknown.
func pickWashSource(states []*washSourceState) *washSourceState {
	for _, st := range states {
		if !st.done {
			return st
		}
	}
	return nil
}

// FetchWashSample collects a sample from Helius with the worker's keys.
func FetchWashSample(ctx context.Context, cfg config.Config, plan WashFetchPlan, mint string) (WashSample, error) {
	keys := orderedHeliusKeys(cfg)
	if len(keys) == 0 {
		return WashSample{}, fmt.Errorf("helius key not configured")
	}
	fetch := func(ctx context.Context, address, before string, since time.Time) ([]enhancedTransaction, error) {
		var lastErr error
		for _, key := range keys {
			txs, err := fetchWashPage(ctx, key, address, before, since)
			if err == nil {
				return txs, nil
			}
			lastErr = err
			if ctx.Err() != nil {
				break
			}
		}
		return nil, lastErr
	}
	return CollectWashSwaps(ctx, plan, mint, fetch)
}

// fetchWashPage is one Enhanced Transactions call, newest first, through the
// shared Helius limiter. Errors never carry the request URL (it holds the key).
func fetchWashPage(ctx context.Context, apiKey, address, before string, since time.Time) ([]enhancedTransaction, error) {
	q := url.Values{}
	q.Set("api-key", apiKey)
	q.Set("limit", strconv.Itoa(WashPageSize))
	q.Set("commitment", "finalized")
	q.Set("token-accounts", "all")
	q.Set("sort-order", "desc")
	if before != "" {
		q.Set("before-signature", before)
	}
	if !since.IsZero() {
		q.Set("gte-time", strconv.FormatInt(since.Unix(), 10))
	}
	endpoint := fmt.Sprintf("%s/addresses/%s/transactions?%s", heliusEnhancedBaseURL, url.PathEscape(address), q.Encode())
	resp, err := doHeliusAPI(ctx, func(ctx context.Context) (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err == nil {
			req.Header.Set("Accept", "application/json")
		}
		return req, err
	})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var txs []enhancedTransaction
	if err := json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(&txs); err != nil {
		return nil, fmt.Errorf("helius enhanced transactions decode: %w", err)
	}
	return txs, nil
}

// ---------------------------------------------------------------------------
// Funders.
// ---------------------------------------------------------------------------

// WashFunderFacts reads recorded first-funding facts for wallets (database
// only, no paid lookups). An entry with an empty Funder is an authoritative
// negative: resolved before, no usable funder.
func WashFunderFacts(cfg config.Config, wallets []string) map[string]FundingFact {
	if LookupFundingFacts == nil || len(wallets) == 0 {
		return map[string]FundingFact{}
	}
	if len(wallets) > 400 {
		wallets = wallets[:400]
	}
	facts := LookupFundingFacts(cfg, "solana", wallets)
	if facts == nil {
		return map[string]FundingFact{}
	}
	return facts
}

// WashClustersFromFacts maps each wallet to a funder-cluster key. The key is
// the funding transaction when known (operators fan out through single-use
// intermediaries inside one transaction), else the funder. Funders the label
// registry marks as exchange, bridge or other infrastructure are ignored: an
// exchange withdrawal wallet funding many people is not a shared purse.
func WashClustersFromFacts(cfg config.Config, facts map[string]FundingFact) map[string]string {
	out := map[string]string{}
	funders := make([]string, 0, len(facts))
	for _, f := range facts {
		if f.Funder != "" {
			funders = append(funders, f.Funder)
		}
	}
	infra := fetchLabeledInfraOwners(cfg, funders)
	for w, f := range facts {
		if f.Funder == "" {
			continue
		}
		if _, skip := infra[f.Funder]; skip {
			continue
		}
		key := strings.TrimSpace(f.Signature)
		if key == "" {
			key = f.Funder
		}
		out[w] = key
	}
	return out
}

// WashFunderClusters is WashClustersFromFacts over recorded facts only.
func WashFunderClusters(cfg config.Config, wallets []string) map[string]string {
	return WashClustersFromFacts(cfg, WashFunderFacts(cfg, wallets))
}

// WashFunderCandidates picks which wallets are worth a paid funder lookup.
// Only a cross-wallet round trip can turn into a shared-funder match, and only
// when BOTH wallets have a funder, so candidates come in pairs: two different
// wallets with opposite-side swaps of matching size inside MaxGap, at least one
// of them without a recorded fact. Pairs are ranked by the volume they would
// match and wallets are taken until limit, never splitting a pair (half a pair
// cannot match). Wallets that only trade with themselves already count as self
// round trips and need no lookup.
func WashFunderCandidates(swaps []WashSwap, known map[string]bool, p WashParams, limit int) []string {
	if limit <= 0 {
		return nil
	}
	priced := make([]WashSwap, 0, len(swaps))
	for _, s := range swaps {
		if s.SOL > 0 && !s.Time.IsZero() && s.Wallet != "" {
			priced = append(priced, s)
		}
	}
	sort.SliceStable(priced, func(i, j int) bool { return priced[i].Time.Before(priced[j].Time) })
	type pair struct {
		a, b string
		vol  float64
	}
	byKey := map[string]*pair{}
	for i := range priced {
		for j := i + 1; j < len(priced); j++ {
			if priced[j].Time.Sub(priced[i].Time) > p.MaxGap {
				break
			}
			a, b := priced[i], priced[j]
			if a.Wallet == b.Wallet || a.Side == b.Side || !sizesMatch(a.SOL, b.SOL, p.SizeTolerance) {
				continue
			}
			if known[a.Wallet] && known[b.Wallet] {
				continue
			}
			x, y := a.Wallet, b.Wallet
			if y < x {
				x, y = y, x
			}
			k := x + "|" + y
			if byKey[k] == nil {
				byKey[k] = &pair{a: x, b: y}
			}
			byKey[k].vol += a.SOL + b.SOL
		}
	}
	pairs := make([]*pair, 0, len(byKey))
	for _, pr := range byKey {
		pairs = append(pairs, pr)
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].vol != pairs[j].vol {
			return pairs[i].vol > pairs[j].vol
		}
		return pairs[i].a+pairs[i].b < pairs[j].a+pairs[j].b
	})
	out := []string{}
	picked := map[string]bool{}
	for _, pr := range pairs {
		need := []string{}
		for _, w := range []string{pr.a, pr.b} {
			if !known[w] && !picked[w] {
				need = append(need, w)
			}
		}
		if len(out)+len(need) > limit {
			continue
		}
		for _, w := range need {
			picked[w] = true
			out = append(out, w)
		}
		if len(out) >= limit {
			break
		}
	}
	return out
}

// ResolveWalletFunderFor is ResolveWalletFunder billed to a caller's budget
// bucket: one gTFA call (gtfaCreditsPerCall units), denied rather than spent
// when that bucket is exhausted.
func ResolveWalletFunderFor(ctx context.Context, cfg config.Config, wallet, budgetPurpose string) (FundingFact, error) {
	return resolveFundingFactViaGTFAFor(ctx, cfg, wallet, budgetPurpose)
}

// WashFunderLookupUnits is the budget estimate for one funder lookup.
const WashFunderLookupUnits = gtfaCreditsPerCall

// ---------------------------------------------------------------------------
// Multi-market traders.
//
// On a deep, busy pool (USDC/SOL, measured 21 Nov 2026) a 3-page sample covers
// about two minutes, and its matched volume came from two high-frequency
// wallets: one quoting fixed clips through Jupiter, one hitting Raydium CLMM,
// Orca, Meteora and PancakeSwap pools directly, each touching 30+ token mints
// in twenty minutes at roughly 175k-265k transactions a day. They buy and sell
// similar sizes within seconds, so they match as round trips, but they are
// arbitrage and market-making flow across many markets, not volume cycled in
// one token. A wallet that touched WashMultiMarketMinMints or more other mints
// in its newest ≤100 transactions, with at least a quarter of its trading
// transactions moving one of them, keeps its volume in the total and is never
// matched. Volume bots that cycle one token do not trade dozens of others.
// ---------------------------------------------------------------------------

// WashMultiMarketMinMints is how many distinct other token mints (not the
// subject, SOL/wSOL, USDC or USDT) a wallet's newest ≤100 transactions must
// touch for it to count as a multi-market trader.
const WashMultiMarketMinMints = 5

// WashMultiMarketCandidates picks the wallets worth checking: the largest
// single-wallet matched entities, each holding at least minShare (0-1) of
// total priced volume, at most limit.
func WashMultiMarketCandidates(m WashMeasurement, limit int, minShare float64) []string {
	out := []string{}
	if limit <= 0 || m.TotalVolume <= 0 {
		return out
	}
	for _, e := range m.TopEntities { // already sorted by matched volume
		if e.Kind != "wallet" || e.MatchedSOL/m.TotalVolume < minShare {
			continue
		}
		out = append(out, e.Key)
		if len(out) >= limit {
			break
		}
	}
	return out
}

// WashWalletActivity summarises one page of a wallet's newest transactions.
type WashWalletActivity struct {
	OtherMints int `json:"other_mints"`         // distinct non-subject, non-quote mints moved
	TradingTxs int `json:"trading_txs"`         // transactions moving any token to or from the wallet
	OtherTxs   int `json:"txs_with_other_mint"` // of those, ones moving a non-subject, non-quote mint
}

// MultiMarket: WashMultiMarketMinMints or more other mints, and at least a
// quarter of the wallet's trading transactions move one of them. Five small
// side trades next to a stream of subject round trips do not qualify. The
// cross-venue bot behind the USDC case measured 31% to 56% on five reads
// within half an hour (13-15 other mints each time); a one-token volume bot
// sits near zero.
func (a WashWalletActivity) MultiMarket() bool {
	return a.OtherMints >= WashMultiMarketMinMints && a.TradingTxs > 0 && 4*a.OtherTxs >= a.TradingTxs
}

// WashWalletActivityOf computes WashWalletActivity for wallet over txs.
// Quote mints (wSOL, USDC, USDT) and the subject are not "other" mints.
func WashWalletActivityOf(txs []enhancedTransaction, wallet, subject string) WashWalletActivity {
	skip := map[string]bool{subject: true, washMintWSOL: true, washMintUSDC: true, washMintUSDT: true, "": true}
	seen := map[string]bool{}
	a := WashWalletActivity{}
	for _, tx := range txs {
		trading, other := false, false
		for _, t := range tx.TokenTransfers {
			if t.FromUserAccount != wallet && t.ToUserAccount != wallet {
				continue
			}
			trading = true
			if !skip[t.Mint] {
				other = true
				seen[t.Mint] = true
			}
		}
		if trading {
			a.TradingTxs++
		}
		if other {
			a.OtherTxs++
		}
	}
	a.OtherMints = len(seen)
	return a
}

// FetchWashWalletActivity reads one page (≤100) of wallet's newest enhanced
// transactions and returns WashWalletActivityOf over it.
func FetchWashWalletActivity(ctx context.Context, cfg config.Config, wallet, subject string) (WashWalletActivity, error) {
	keys := orderedHeliusKeys(cfg)
	if len(keys) == 0 {
		return WashWalletActivity{}, fmt.Errorf("helius key not configured")
	}
	var lastErr error
	for _, key := range keys {
		txs, err := fetchWashPage(ctx, key, wallet, "", time.Time{})
		if err == nil {
			return WashWalletActivityOf(txs, wallet, subject), nil
		}
		lastErr = err
		if ctx.Err() != nil {
			break
		}
	}
	return WashWalletActivity{}, lastErr
}

