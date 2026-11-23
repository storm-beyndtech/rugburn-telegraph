package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"rugburn-worker/internal/config"
	"rugburn-worker/internal/scanner"
)

// GET /telegraph/preflight?subject=<mint>  (or POST {"subject": "<mint>"})
//
// A token-risk preflight: HOLD | REVIEW | REJECT plus the reasons behind it.
// It reuses the wash route's auth (developer API key, scope token:risk:read),
// per-key rate limit, 20 s cap and telegraph_wash budget bucket, and it reuses
// the worker's own token scan (scanner.ScanWithDedup, the same path /scan and
// the v1 risk API persist into token_scans) plus the /telegraph/wash answer.
//
// Every rule is a named constant below. Each reason says whether it is an
// observed on-chain fact, a value derived from observed facts, or a
// hypothesis, and carries its evidence. Nothing here says who controls an
// address. An inconclusive wash answer never changes the verdict.

const (
	preflightEndpoint     = "/telegraph/preflight"
	preflightRulesVersion = "preflight-v1"

	// --- REJECT: hard on-chain controls that let one key take holders' money
	// or block exits. Allowlisted mints are exempt from the issuer-control
	// rules (authorities and Token-2022 issuer extensions) only.

	// A live mint authority can print unlimited supply into the pool.
	preflightRejectOnMintAuthority = true
	// A live freeze authority can freeze any holder's token account, which
	// blocks selling.
	preflightRejectOnFreezeAuthority = true
	// Token-2022 transfer fee at or above 10% of every transfer.
	preflightRejectTransferFeeBps = 1000
	// Sell simulation: hidden tax at or above 35% is the scanner's own
	// honeypot threshold (scanner/scan.go sets IsHoneypot from it).
	preflightRejectHiddenTaxPct = 35.0
	// LP: at least 95% of LP tokens unlocked AND one non-lock, non-burn holder
	// holding at least 80% of LP means one wallet can pull the liquidity.
	preflightRejectLPUnlockedPct     = 95.0
	preflightRejectLPSingleHolderPct = 80.0

	// --- REVIEW: soft signals worth a human look; none of them alone proves harm.

	// Any Token-2022 transfer fee below the reject line.
	preflightReviewTransferFeeBps = 1
	// Sell simulation hidden tax between 10% and the reject line.
	preflightReviewHiddenTaxPct = 10.0
	// LP mostly unlocked but spread across holders.
	preflightReviewLPUnlockedPct = 50.0
	// Holder concentration (pools, locks, programs and labelled infrastructure
	// are already excluded by the scanner's holder analysis).
	preflightReviewTop1HolderPct  = 20.0
	preflightReviewTop10HolderPct = 50.0
	// Pair younger than this.
	preflightReviewMinAgeHours = 24.0
	// Liquidity below this, in USD, across the pools the market data saw.
	preflightReviewMinLiquidityUSD = 10000.0
)

// preflightAllowlist: widely used stablecoin and wrapped-SOL mints whose
// issuer keeps mint/freeze authority (or Token-2022 issuer controls) as normal
// operation. Exempts them from the issuer-control REJECT rules only; every
// other rule still applies. Changes need a code review, not an env var.
var preflightAllowlist = map[string]string{
	"EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v": "USDC",
	"Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB": "USDT",
	"So11111111111111111111111111111111111111112":  "wSOL",
}

// Effects and categories used in reasons.
const (
	effectReject = "reject"
	effectReview = "review"
	effectNone   = "none"

	catObserved   = "observed"
	catDerived    = "derived"
	catHypothesis = "hypothesis"
)

// PreflightReason is one line of the answer.
type PreflightReason struct {
	Name     string                 `json:"name"`
	Category string                 `json:"category"`
	Effect   string                 `json:"effect"`
	Evidence map[string]interface{} `json:"evidence"`
	Note     string                 `json:"note,omitempty"`
}

// preflightFacts is the slice of a token scan the rules read.
type preflightFacts struct {
	Source        string // fresh_scan | memory | stored_scan | none
	ScannedAt     time.Time
	ScanCostUnits int

	Authority        *scanner.AuthorityResult
	Tax              *scanner.TaxResult
	LP               *scanner.LPLockResult
	Holders          *scanner.HolderAnalysis
	IsHoneypot       bool
	HoneypotAssessed bool
	LiquidityUSD     float64
	PairAgeHours     float64
	AgeKnown         bool
	PoolAddress      string
	DexID            string
	Volume24h        float64
	Buys24h          int
	Sells24h         int
}

func (f preflightFacts) scanContext() washScanContext {
	return washScanContext{Source: f.Source, PoolAddress: f.PoolAddress, DexID: f.DexID, LiquidityUSD: f.LiquidityUSD, Volume24h: f.Volume24h, Buys24h: f.Buys24h, Sells24h: f.Sells24h, ScannedAt: f.ScannedAt}
}

// Seams for tests.
var (
	preflightLatestFacts = latestStoredPreflightFacts
	preflightWash        = func(s *Server, ctx context.Context, mint string) map[string]interface{} {
		return s.washAnswer(ctx, mint)
	}
	preflightLogUsage = func(cfg config.Config, key *washDevKey, r *http.Request, status int, latency time.Duration, errCode string) {
		logDeveloperAPIUsageFor(cfg, preflightEndpoint, key, r, status, latency, errCode)
	}
)

var preflightCache = struct {
	sync.Mutex
	facts map[string]preflightFacts
}{facts: map[string]preflightFacts{}}

func (s *Server) handleTelegraphPreflight(w http.ResponseWriter, r *http.Request) {
	started := washNow()
	var subject string
	switch r.Method {
	case http.MethodGet:
		subject = r.URL.Query().Get("subject")
	case http.MethodPost:
		var body struct {
			Subject string `json:"subject"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(&body); err != nil {
			writeWashError(w, http.StatusBadRequest, "invalid_body", "body must be JSON: {\"subject\": \"<solana mint>\"}")
			return
		}
		subject = body.Subject
	default:
		writeWashError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use GET ?subject= or POST {\"subject\"}")
		return
	}
	key, status, msg := s.authorizeWashKey(extractDeveloperKey(r))
	if key == nil {
		writeWashError(w, status, "unauthorized", msg)
		return
	}
	finish := func(code int, errCode string) {
		go preflightLogUsage(s.config, key, r, code, washNow().Sub(started), errCode)
	}
	// Own limiter per key, same rate as /telegraph/wash.
	if !s.washAllow("preflight:" + key.ID) {
		w.Header().Set("Retry-After", "1")
		writeWashError(w, http.StatusTooManyRequests, "rate_limited", "per-key rate limit exceeded")
		finish(http.StatusTooManyRequests, "rate_limited")
		return
	}
	mint, err := ValidateWashSubject(subject)
	if err != nil {
		writeWashError(w, http.StatusBadRequest, "unsupported_subject", err.Error())
		finish(http.StatusBadRequest, "unsupported_subject")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), washTotalCap)
	defer cancel()
	body := s.preflightAnswer(ctx, mint)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "private, max-age=60")
	_ = json.NewEncoder(w).Encode(body)
	finish(http.StatusOK, "")
}

func (s *Server) preflightAnswer(ctx context.Context, mint string) map[string]interface{} {
	facts := s.preflightScanFacts(ctx, mint)
	wash := preflightWash(s, ctx, mint)
	if wash == nil {
		wash = map[string]interface{}{"verdict": "inconclusive", "inconclusive_reason": "wash check returned nothing"}
	}
	return BuildPreflightResponse(mint, facts, wash)
}

// preflightScanFacts mirrors washScanContextFor: a scan this route ran in the
// last 15 min, a stored scan under 15 min, wait on an in-flight scan, run a
// fresh one (budget permitting, billed to telegraph_wash), then any stored
// scan under 24 h. It never blocks past the wash reserve, and a fresh scan's
// pool context is handed to the wash check so it does not scan again.
func (s *Server) preflightScanFacts(ctx context.Context, mint string) preflightFacts {
	now := washNow()
	preflightCache.Lock()
	if f, ok := preflightCache.facts[mint]; ok && now.Sub(f.ScannedAt) <= washFreshScanMaxAge {
		preflightCache.Unlock()
		f.Source, f.ScanCostUnits = "memory", 0
		return f
	}
	preflightCache.Unlock()

	stored, _ := preflightLatestFacts(s.config, mint)
	if stored != nil && now.Sub(stored.ScannedAt) <= washFreshScanMaxAge {
		return *stored
	}
	fallback := func() preflightFacts {
		if latest, _ := preflightLatestFacts(s.config, mint); latest != nil && washNow().Sub(latest.ScannedAt) <= washStaleScanMaxAge {
			return *latest
		}
		if stored != nil && washNow().Sub(stored.ScannedAt) <= washStaleScanMaxAge {
			return *stored
		}
		return preflightFacts{Source: "none"}
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = now.Add(washTotalCap)
	}
	scanCtx, cancel := context.WithDeadline(ctx, deadline.Add(-washSwapReserve))
	defer cancel()

	if _, inFlight, _ := washScanInfo(mint); inFlight {
		ticker := time.NewTicker(washPollPeriod)
		defer ticker.Stop()
		for {
			select {
			case <-scanCtx.Done():
				return fallback()
			case <-ticker.C:
				if _, still, _ := washScanInfo(mint); !still {
					return fallback()
				}
			}
		}
	}
	if allowed, _ := washBudgetAllowed(s.config, s.config.HeliusFullScanEstimatedCost); !allowed {
		return fallback()
	}
	done := make(chan *scanner.TokenResult, 1)
	go func() {
		res, err := washRunScan(s.config, mint)
		if err != nil || res == nil {
			done <- nil
			return
		}
		washRecordUsage(s.config, "scan", s.config.HeliusFullScanEstimatedCost, map[string]interface{}{"address": mint, "scan_source": "telegraph_preflight", "rpc_calls_used": res.RPCCallsUsed})
		washPersist(s, res)
		f := preflightFactsFromResult(res, washNow())
		f.ScanCostUnits = s.config.HeliusFullScanEstimatedCost
		preflightCache.Lock()
		preflightCache.facts[mint] = f
		if len(preflightCache.facts) > 5000 {
			for k, v := range preflightCache.facts {
				if washNow().Sub(v.ScannedAt) > washFreshScanMaxAge {
					delete(preflightCache.facts, k)
				}
			}
		}
		preflightCache.Unlock()
		st := washGlobal
		st.mu.Lock()
		st.init()
		st.scans[mint] = f.scanContext()
		st.mu.Unlock()
		done <- res
	}()
	select {
	case res := <-done:
		if res != nil {
			preflightCache.Lock()
			f := preflightCache.facts[mint]
			preflightCache.Unlock()
			return f
		}
		return fallback()
	case <-scanCtx.Done():
		return fallback()
	}
}

func preflightFactsFromResult(res *scanner.TokenResult, at time.Time) preflightFacts {
	f := preflightFacts{
		Source:       "fresh_scan",
		ScannedAt:    at,
		Authority:    res.AuthorityStatus,
		Tax:          res.TaxAnalysis,
		LP:           res.LPLockStatus,
		Holders:      res.HolderAnalysis,
		IsHoneypot:   res.IsHoneypot,
		LiquidityUSD: res.LiquidityUSD,
		PoolAddress:  res.PoolAddress,
		DexID:        res.DexID,
		Volume24h:    res.Volume24h,
		Buys24h:      res.Buys24h,
		Sells24h:     res.Sells24h,
	}
	if f.PoolAddress == "" {
		if p, ok := res.RawOnchainData["pairAddress"].(string); ok {
			f.PoolAddress = p
		}
	}
	readRawIntel(&f, res.RawOnchainData["launch_intelligence"], res.RawOnchainData["security_intelligence"])
	return f
}

func readRawIntel(f *preflightFacts, launch, security interface{}) {
	if m, ok := launch.(map[string]interface{}); ok {
		if known, _ := m["age_known"].(bool); known {
			if h, ok := m["pair_age_hours"].(float64); ok {
				f.AgeKnown, f.PairAgeHours = true, h
			}
		}
	}
	if m, ok := security.(map[string]interface{}); ok {
		f.HoneypotAssessed, _ = m["honeypot_assessed"].(bool)
	}
}

// latestStoredPreflightFacts reads the newest token_scans row for mint.
func latestStoredPreflightFacts(cfg config.Config, mint string) (*preflightFacts, error) {
	var rows []struct {
		Authority    *scanner.AuthorityResult `json:"authority_status"`
		Tax          *scanner.TaxResult       `json:"tax_analysis"`
		LP           *scanner.LPLockResult    `json:"lp_lock_status"`
		Holders      *scanner.HolderAnalysis  `json:"holder_analysis"`
		IsHoneypot   *bool                    `json:"is_honeypot"`
		LiquidityUSD *float64                 `json:"liquidity_usd"`
		PoolAddress  *string                  `json:"pool_address"`
		DexID        *string                  `json:"dex_id"`
		Volume24h    *float64                 `json:"volume_24h"`
		Buys24h      *int                     `json:"buy_count_24h"`
		Sells24h     *int                     `json:"sell_count_24h"`
		ScannedAt    string                   `json:"scanned_at"`
		Launch       interface{}              `json:"launch"`
		Security     interface{}              `json:"security"`
	}
	q := url.Values{}
	q.Set("select", "authority_status,tax_analysis,lp_lock_status,holder_analysis,is_honeypot,liquidity_usd,pool_address,dex_id,volume_24h,buy_count_24h,sell_count_24h,scanned_at,launch:raw_onchain_data->launch_intelligence,security:raw_onchain_data->security_intelligence")
	q.Set("address", "eq."+mint)
	q.Set("order", "scanned_at.desc")
	q.Set("limit", "1")
	if err := washSupabaseGet(cfg, "/rest/v1/token_scans?"+q.Encode(), &rows); err != nil || len(rows) == 0 {
		return nil, err
	}
	row := rows[0]
	t, err := time.Parse(time.RFC3339Nano, row.ScannedAt)
	if err != nil {
		return nil, err
	}
	f := &preflightFacts{Source: "stored_scan", ScannedAt: t, Authority: row.Authority, Tax: row.Tax, LP: row.LP, Holders: row.Holders}
	if row.IsHoneypot != nil {
		f.IsHoneypot = *row.IsHoneypot
	}
	if row.LiquidityUSD != nil {
		f.LiquidityUSD = *row.LiquidityUSD
	}
	if row.PoolAddress != nil {
		f.PoolAddress = *row.PoolAddress
	}
	if row.DexID != nil {
		f.DexID = *row.DexID
	}
	if row.Volume24h != nil {
		f.Volume24h = *row.Volume24h
	}
	if row.Buys24h != nil {
		f.Buys24h = *row.Buys24h
	}
	if row.Sells24h != nil {
		f.Sells24h = *row.Sells24h
	}
	readRawIntel(f, row.Launch, row.Security)
	return f, nil
}

// EvaluatePreflight applies the rules. Pure: same facts, same answer.
func EvaluatePreflight(mint string, f preflightFacts, wash map[string]interface{}) (string, []PreflightReason) {
	reasons := []PreflightReason{}
	add := func(r PreflightReason) {
		if r.Evidence == nil {
			r.Evidence = map[string]interface{}{}
		}
		reasons = append(reasons, r)
	}
	allowName, allowlisted := preflightAllowlist[mint]
	if allowlisted {
		add(PreflightReason{
			Name: "known_mint_allowlist", Category: catObserved, Effect: effectNone,
			Evidence: map[string]interface{}{"mint": mint, "list_entry": allowName, "source": "rugburn preflight allowlist (code constant)"},
			Note:     "This mint is on a fixed allowlist of widely used stablecoin and wrapped-SOL mints, where an active mint or freeze authority and Token-2022 issuer controls are normal. Those rules are reported but do not reject it. This says nothing about who holds the authority keys.",
		})
	}
	issuerEffect := func(def string) string {
		if allowlisted {
			return effectNone
		}
		return def
	}
	scanSrc := "token_scan (" + f.Source + ")"

	// --- Scan coverage.
	if f.Source == "none" {
		add(PreflightReason{
			Name: "scan_unavailable", Category: catObserved, Effect: effectReview,
			Evidence: map[string]interface{}{"source": "token_scan"},
			Note:     "No token scan could be run or found within 24 h, so on-chain controls are unchecked. Not enough evidence to clear the token.",
		})
	}

	// --- Authorities.
	if f.Source != "none" {
		if f.Authority == nil {
			add(PreflightReason{
				Name: "authority_not_assessed", Category: catObserved, Effect: effectReview,
				Evidence: map[string]interface{}{"source": scanSrc},
				Note:     "The scan has no mint/freeze authority reading for this token.",
			})
		} else {
			a := f.Authority
			if a.MintAuthority != "" && preflightRejectOnMintAuthority {
				add(PreflightReason{
					Name: "mint_authority_active", Category: catObserved, Effect: issuerEffect(effectReject),
					Evidence: map[string]interface{}{"mint_authority": a.MintAuthority, "source": scanSrc + ".authority_status"},
					Note:     "The mint authority is set, so new supply can still be minted.",
				})
			}
			if a.FreezeAuthority != "" && preflightRejectOnFreezeAuthority {
				add(PreflightReason{
					Name: "freeze_authority_active", Category: catObserved, Effect: issuerEffect(effectReject),
					Evidence: map[string]interface{}{"freeze_authority": a.FreezeAuthority, "source": scanSrc + ".authority_status"},
					Note:     "The freeze authority is set, so holder token accounts can be frozen, which blocks selling.",
				})
			}
			if a.MintAuthority == "" && a.FreezeAuthority == "" {
				add(PreflightReason{
					Name: "authorities_renounced", Category: catObserved, Effect: effectNone,
					Evidence: map[string]interface{}{"mint_authority": nil, "freeze_authority": nil, "source": scanSrc + ".authority_status"},
				})
			}
		}
	}

	// --- Token-2022 extensions and sell simulation.
	if t := f.Tax; t != nil {
		src := scanSrc + ".tax_analysis"
		if t.HasTransferFee && t.TransferFeeBps >= preflightRejectTransferFeeBps {
			add(PreflightReason{Name: "token2022_transfer_fee_extreme", Category: catObserved, Effect: effectReject,
				Evidence: map[string]interface{}{"transfer_fee_bps": t.TransferFeeBps, "transfer_fee_max": t.TransferFeeMax, "reject_at_bps": preflightRejectTransferFeeBps, "source": src}})
		} else if t.HasTransferFee && t.TransferFeeBps >= preflightReviewTransferFeeBps {
			add(PreflightReason{Name: "token2022_transfer_fee", Category: catObserved, Effect: effectReview,
				Evidence: map[string]interface{}{"transfer_fee_bps": t.TransferFeeBps, "transfer_fee_max": t.TransferFeeMax, "source": src}})
		}
		if t.IsNonTransferable {
			add(PreflightReason{Name: "token2022_non_transferable", Category: catObserved, Effect: effectReject,
				Evidence: map[string]interface{}{"non_transferable": true, "source": src}, Note: "Holders cannot transfer, so cannot sell."})
		}
		if t.DefaultAccountFrozen {
			add(PreflightReason{Name: "token2022_default_frozen", Category: catObserved, Effect: issuerEffect(effectReject),
				Evidence: map[string]interface{}{"default_account_state": "frozen", "source": src}, Note: "New token accounts start frozen until the freeze authority thaws them."})
		}
		if t.HasPermanentDelegate {
			add(PreflightReason{Name: "token2022_permanent_delegate", Category: catObserved, Effect: issuerEffect(effectReject),
				Evidence: map[string]interface{}{"permanent_delegate": t.PermanentDelegate, "source": src}, Note: "The permanent delegate can transfer or burn tokens from any holder's account."})
		}
		if t.IsPausable && t.IsPaused {
			add(PreflightReason{Name: "token2022_paused", Category: catObserved, Effect: issuerEffect(effectReject),
				Evidence: map[string]interface{}{"paused": true, "source": src}, Note: "Transfers are paused right now."})
		} else if t.IsPausable {
			add(PreflightReason{Name: "token2022_pausable", Category: catObserved, Effect: issuerEffect(effectReview),
				Evidence: map[string]interface{}{"pausable": true, "paused": false, "source": src}})
		}
		if t.HasTransferHook {
			add(PreflightReason{Name: "token2022_transfer_hook", Category: catObserved, Effect: issuerEffect(effectReview),
				Evidence: map[string]interface{}{"transfer_hook_program": t.TransferHookProgram, "source": src}, Note: "A program runs on every transfer and can refuse it."})
		}
		if t.SimulationWorked && t.HiddenTaxPct >= preflightRejectHiddenTaxPct {
			add(PreflightReason{Name: "sell_simulation_hidden_tax", Category: catDerived, Effect: effectReject,
				Evidence: map[string]interface{}{"hidden_tax_pct": t.HiddenTaxPct, "estimated_sell_tax_pct": t.EstimatedSellTaxPct, "round_trip_loss_pct": t.RoundTripLossPct, "reject_at_pct": preflightRejectHiddenTaxPct, "confidence": t.Confidence, "source": src},
				Note:     "A simulated buy-then-sell lost this much beyond known DEX fees and price impact."})
		} else if t.SimulationWorked && t.HiddenTaxPct >= preflightReviewHiddenTaxPct {
			add(PreflightReason{Name: "sell_simulation_tax", Category: catDerived, Effect: effectReview,
				Evidence: map[string]interface{}{"hidden_tax_pct": t.HiddenTaxPct, "estimated_sell_tax_pct": t.EstimatedSellTaxPct, "confidence": t.Confidence, "source": src}})
		}
	}
	// The scanner's honeypot flag can also come from other checks; reject on
	// it only when the sell-simulation rule above has not already explained it.
	if f.IsHoneypot && !hasReason(reasons, "sell_simulation_hidden_tax") {
		add(PreflightReason{Name: "scanner_honeypot_flag", Category: catDerived, Effect: effectReject,
			Evidence: map[string]interface{}{"is_honeypot": true, "honeypot_assessed": f.HoneypotAssessed, "source": scanSrc + ".is_honeypot"},
			Note:     "The scanner marked this token unsafe to sell (sell simulation or its brand-impersonation guard)."})
	}

	// --- Liquidity lock.
	if lp := f.LP; lp != nil && !lp.IsPumpFun {
		src := scanSrc + ".lp_lock_status"
		top := topUnlockedLPHolder(lp)
		switch {
		case lp.Method == "unlocked" && lp.UnlockedPercent >= preflightRejectLPUnlockedPct && top != nil && top.Percent >= preflightRejectLPSingleHolderPct:
			add(PreflightReason{Name: "lp_unlocked_single_holder", Category: catDerived, Effect: effectReject,
				Evidence: map[string]interface{}{"lp_mint": lp.LPMint, "unlocked_pct": round2(lp.UnlockedPercent), "holder_token_account": top.Address, "holder_owner": top.Owner, "holder_lp_pct": round2(top.Percent), "source": src},
				Note:     "Almost all LP is unlocked and one account holds most of it, so that account can remove the liquidity."})
		case (lp.Method == "unlocked" || lp.Method == "partially_locked") && lp.UnlockedPercent >= preflightReviewLPUnlockedPct:
			add(PreflightReason{Name: "lp_mostly_unlocked", Category: catObserved, Effect: effectReview,
				Evidence: map[string]interface{}{"lp_mint": lp.LPMint, "unlocked_pct": round2(lp.UnlockedPercent), "locked_pct": round2(lp.LockedPercent), "burned_pct": round2(lp.BurnedPercent), "method": lp.Method, "source": src}})
		case lp.Method == "locked" || lp.Method == "burned":
			add(PreflightReason{Name: "lp_locked_or_burned", Category: catObserved, Effect: effectNone,
				Evidence: map[string]interface{}{"locked_pct": round2(lp.LockedPercent), "burned_pct": round2(lp.BurnedPercent), "method": lp.Method, "source": src}})
		}
	}

	// --- Holders.
	if h := f.Holders; h != nil && h.ScoredHolderRecords > 0 && !allowlisted {
		if h.Top1HolderPct >= preflightReviewTop1HolderPct || h.Top10HolderPct >= preflightReviewTop10HolderPct {
			ev := map[string]interface{}{"top_1_holder_pct": round2(h.Top1HolderPct), "top_10_holder_pct": round2(h.Top10HolderPct), "review_at_top1_pct": preflightReviewTop1HolderPct, "review_at_top10_pct": preflightReviewTop10HolderPct, "holder_records": h.ScoredHolderRecords, "source": scanSrc + ".holder_analysis"}
			for _, th := range h.TopHolders {
				if th.Type == "retail" {
					ev["largest_holder_owner"] = th.Owner
					ev["largest_holder_pct"] = round2(th.Percent)
					break
				}
			}
			add(PreflightReason{Name: "concentrated_holders", Category: catDerived, Effect: effectReview, Evidence: ev,
				Note: "Pools, locks, programs and labelled exchanges are excluded before these shares are computed."})
		}
	}

	// --- Market.
	if f.Source != "none" && !allowlisted {
		if f.AgeKnown && f.PairAgeHours < preflightReviewMinAgeHours {
			add(PreflightReason{Name: "very_new_token", Category: catObserved, Effect: effectReview,
				Evidence: map[string]interface{}{"pair_age_hours": round2(f.PairAgeHours), "review_under_hours": preflightReviewMinAgeHours, "source": scanSrc + ".launch_intelligence"}})
		}
		if f.LiquidityUSD < preflightReviewMinLiquidityUSD {
			add(PreflightReason{Name: "low_liquidity", Category: catObserved, Effect: effectReview,
				Evidence: map[string]interface{}{"liquidity_usd": round2(f.LiquidityUSD), "review_under_usd": preflightReviewMinLiquidityUSD, "pool_address": f.PoolAddress, "dex_id": f.DexID, "source": scanSrc + ".liquidity_usd"},
				Note:     "Zero means the market data provider reported no liquidity."})
		}
	}

	// --- Wash trading (never rejects; inconclusive never moves the verdict).
	add(washReason(wash))

	verdict := "HOLD"
	for _, r := range reasons {
		if r.Effect == effectReject {
			verdict = "REJECT"
			break
		}
		if r.Effect == effectReview {
			verdict = "REVIEW"
		}
	}
	// Rejects first, then reviews, then context.
	rank := map[string]int{effectReject: 0, effectReview: 1, effectNone: 2}
	sort.SliceStable(reasons, func(i, j int) bool { return rank[reasons[i].Effect] < rank[reasons[j].Effect] })
	return verdict, reasons
}

func washReason(wash map[string]interface{}) PreflightReason {
	verdict, _ := wash["verdict"].(string)
	ev := map[string]interface{}{"wash_verdict": verdict, "wash_volume_pct": wash["wash_volume_pct"], "source": "telegraph_wash"}
	if v, ok := wash["as_of"]; ok {
		ev["as_of"] = v
	}
	if v, ok := wash["stale"]; ok {
		ev["stale"] = v
	}
	switch verdict {
	case "wash_trading", "likely_wash_trading":
		return PreflightReason{Name: "likely_wash_trading", Category: catHypothesis, Effect: effectReview, Evidence: ev,
			Note: "A measured share of recent volume was matched round trips by the same wallet or wallets sharing a recorded funder. Fast legitimate flipping leaves the same footprint; this does not say who controls the wallets."}
	case "organic":
		return PreflightReason{Name: "wash_check_clear", Category: catDerived, Effect: effectNone, Evidence: ev}
	default:
		if r, ok := wash["inconclusive_reason"]; ok {
			ev["inconclusive_reason"] = r
		}
		return PreflightReason{Name: "wash_check_inconclusive", Category: catObserved, Effect: effectNone, Evidence: ev,
			Note: "Not enough priced swaps to measure. This does not count for or against the token."}
	}
}

func topUnlockedLPHolder(lp *scanner.LPLockResult) *scanner.LPHolder {
	var top *scanner.LPHolder
	for i := range lp.Details {
		d := &lp.Details[i]
		if d.Type != "unlocked" {
			continue
		}
		if top == nil || d.Percent > top.Percent {
			top = d
		}
	}
	return top
}

func hasReason(rs []PreflightReason, name string) bool {
	for _, r := range rs {
		if r.Name == name {
			return true
		}
	}
	return false
}

func round2(v float64) float64 { return roundTo(v, 2) }

// BuildPreflightResponse renders the answer. Never empty.
func BuildPreflightResponse(mint string, f preflightFacts, wash map[string]interface{}) map[string]interface{} {
	verdict, reasons := EvaluatePreflight(mint, f, wash)
	rejects, reviews := []string{}, []string{}
	for _, r := range reasons {
		switch r.Effect {
		case effectReject:
			rejects = append(rejects, r.Name)
		case effectReview:
			reviews = append(reviews, r.Name)
		}
	}
	var summary string
	switch verdict {
	case "REJECT":
		summary = "REJECT: " + strings.Join(rejects, ", ") + "."
	case "REVIEW":
		summary = "REVIEW: " + strings.Join(reviews, ", ") + "."
	default:
		summary = "HOLD: no reject or review rule fired."
	}

	scanCheck := map[string]interface{}{"source": f.Source, "cached": f.Source == "memory" || f.Source == "stored_scan", "cost_units": f.ScanCostUnits}
	if !f.ScannedAt.IsZero() {
		scanCheck["scanned_at"] = f.ScannedAt.UTC().Format(time.RFC3339)
		scanCheck["age_seconds"] = int64(washNow().Sub(f.ScannedAt).Seconds())
	}
	washCached, _ := wash["cached"].(bool)
	washStale, _ := wash["stale"].(bool)
	washCostUnits := 0
	if c, ok := wash["cost"].(map[string]interface{}); ok && !washCached && !washStale {
		switch u := c["budget_units"].(type) {
		case int:
			washCostUnits = u
		case float64:
			washCostUnits = int(u)
		}
	}
	washCheck := map[string]interface{}{"verdict": wash["verdict"], "wash_volume_pct": wash["wash_volume_pct"], "cached": washCached, "stale": washStale, "cost_units": washCostUnits, "as_of": wash["as_of"]}

	return map[string]interface{}{
		"subject":       mint,
		"chain":         "solana",
		"source":        "rugburn",
		"as_of":         washNow().UTC().Format(time.RFC3339),
		"verdict":       verdict,
		"summary":       summary,
		"reasons":       reasons,
		"rules_version": preflightRulesVersion,
		"checks": map[string]interface{}{
			"token_scan": scanCheck,
			"wash":       washCheck,
		},
		"cost_units": f.ScanCostUnits + washCostUnits,
		"method":     fmt.Sprintf("REJECT if any hard on-chain rule fires (live mint or freeze authority outside the stablecoin/wSOL allowlist, Token-2022 transfer fee ≥%d bps, non-transferable, default-frozen, permanent delegate or paused, sell-simulation hidden tax ≥%.0f%%, or ≥%.0f%% of LP unlocked with one holder ≥%.0f%%). Else REVIEW if any soft rule fires (wash_trading or likely_wash_trading, top holder ≥%.0f%% or top 10 ≥%.0f%%, pair under %.0f h, liquidity under $%.0f, smaller transfer fee or hidden tax, transfer hook, pausable, LP ≥%.0f%% unlocked, no scan). Else HOLD. An inconclusive wash check never changes the verdict.", preflightRejectTransferFeeBps, preflightRejectHiddenTaxPct, preflightRejectLPUnlockedPct, preflightRejectLPSingleHolderPct, preflightReviewTop1HolderPct, preflightReviewTop10HolderPct, preflightReviewMinAgeHours, preflightReviewMinLiquidityUSD, preflightReviewLPUnlockedPct),
	}
}

