package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gagliardetto/solana-go"
	"golang.org/x/time/rate"

	"rugburn-worker/internal/config"
	"rugburn-worker/internal/providerbudget"
	"rugburn-worker/internal/scanner"
)

// GET /telegraph/wash?subject=<mint>  (or POST {"subject": "<mint>"})
//
// Serves the WASH_TRADING_DETECTION_GENERAL intent. Auth is an ordinary Rugburn
// developer API key (Authorization: Bearer <key> or X-Rugburn-API-Key), checked
// against developer_api_keys by sha256 hash, the same way the Next.js developer
// API does it. The route always answers with a verdict: it never hands back an
// empty 202 for "scan in progress" or "too fresh".

const (
	washScope           = "token:risk:read"
	washTotalCap        = 20 * time.Second
	washSwapReserve     = 7 * time.Second // time kept back for the swap fetch
	washFreshScanMaxAge = 15 * time.Minute
	washStaleScanMaxAge = 24 * time.Hour
	washStaleMaxAge     = 24 * time.Hour // in-memory answers kept for budget fallback
	washBudgetFeature   = "telegraph_wash"

	// Sample collection bounds (see scanner.CollectWashSwaps).
	washLookback         = 24 * time.Hour // swaps older than this are not read
	washTargetSwaps      = 200            // stop paging once this many swaps are in hand
	washPageMinRemaining = 2500 * time.Millisecond
	// Funder lookups only start with this much of the 20 s cap left; each is
	// one gTFA call and they run 4 at a time.
	washFunderMinRemaining = 4 * time.Second
	washFunderConcurrency  = 4
)

// washMaxPages caps Helius Enhanced Transactions calls per computed answer
// (TELEGRAPH_WASH_MAX_PAGES, default 3, clamped to 1..5). Each page is billed
// separately to the telegraph_wash bucket.
func washMaxPages() int {
	n := washEnvInt("TELEGRAPH_WASH_MAX_PAGES", 3)
	if n < 1 {
		n = 1
	}
	if n > 5 {
		n = 5
	}
	return n
}

// washMaxActivityLookups caps wallet-activity checks per computed answer
// (TELEGRAPH_WASH_ACTIVITY_LOOKUPS, default 3, 0 disables, max 5). Each is one
// Enhanced Transactions page billed to the telegraph_wash bucket, and they
// only run when the first measurement reaches washMultiMarketCheckFrom.
func washMaxActivityLookups() int {
	n := washEnvInt("TELEGRAPH_WASH_ACTIVITY_LOOKUPS", 3)
	if n > 5 {
		n = 5
	}
	return n
}

// washMultiMarketMinShare: a matched wallet is only checked when its matched
// volume is at least this share of the sample's priced volume.
const washMultiMarketMinShare = 0.05

// washMultiMarketCheckFrom: activity checks run only when the first
// measurement reaches this share. Below it, no exemption can change much.
const washMultiMarketCheckFrom = 15.0

// washMaxFunderLookups caps paid first-funder lookups per computed answer
// (TELEGRAPH_WASH_FUNDER_LOOKUPS, default 4, 0 disables, max 10). Each is one
// gTFA call (100 units) billed to the telegraph_wash bucket.
func washMaxFunderLookups() int {
	n := washEnvInt("TELEGRAPH_WASH_FUNDER_LOOKUPS", 4)
	if n > 10 {
		n = 10
	}
	return n
}

type washDevKey struct {
	ID        string   `json:"id"`
	UserID    *string  `json:"user_id"`
	Status    string   `json:"status"`
	Scopes    []string `json:"scopes"`
	ExpiresAt *string  `json:"expires_at"`
	RevokedAt *string  `json:"revoked_at"`
}

// washScanContext is the slice of a token scan the wash answer uses.
type washScanContext struct {
	Source       string    // fresh_scan | stored_scan | none
	PoolAddress  string    `json:"pool_address"`
	DexID        string    `json:"dex_id"`
	LiquidityUSD float64   `json:"liquidity_usd"`
	Volume24h    float64   `json:"volume_24h"`
	Buys24h      int       `json:"buy_count_24h"`
	Sells24h     int       `json:"sell_count_24h"`
	ScannedAt    time.Time `json:"scanned_at"`
}

// Seams for tests. Production values talk to Supabase, the scanner and Helius.
var (
	washLookupKey  = lookupDeveloperKeyByHash
	washLatestScan = latestStoredScanContext
	washRunScan    = func(cfg config.Config, mint string) (*scanner.TokenResult, error) {
		return scanner.ScanWithDedup(cfg, mint, time.Duration(cfg.MinRescanIntervalSeconds)*time.Second, scanner.ScanOptions{ScanSource: "telegraph", Chain: "solana"})
	}
	washScanInfo          = scanner.ScanInfo
	washFetchSample       = scanner.FetchWashSample
	washFunderFacts       = scanner.WashFunderFacts
	washClustersFromFacts = scanner.WashClustersFromFacts
	washResolveFunder     = func(ctx context.Context, cfg config.Config, wallet string) (scanner.FundingFact, error) {
		return scanner.ResolveWalletFunderFor(ctx, cfg, wallet, washBudgetFeature)
	}
	washPersistFacts = func(cfg config.Config, mint string, facts map[string]scanner.FundingFact, negatives []string) {
		if len(facts) > 0 && scanner.PersistFundingFacts != nil {
			scanner.PersistFundingFacts(cfg, "solana", mint, facts)
		}
		if len(negatives) > 0 && scanner.PersistFundingNegatives != nil {
			scanner.PersistFundingNegatives(cfg, "solana", negatives)
		}
	}
	washSOLUSD         = cachedSOLUSD
	washWalletActivity = scanner.FetchWashWalletActivity
	washBudgetAllowed  = func(cfg config.Config, units int) (bool, string) {
		st := providerbudget.GetStatus(cfg, "helius", washBudgetFeature, units)
		return st.Allowed, st.Reason
	}
	washRecordUsage = func(cfg config.Config, method string, units int, meta map[string]interface{}) {
		if err := providerbudget.RecordUsage(cfg, "helius", method, washBudgetFeature, units, false, meta); err != nil {
			log.Printf("[telegraph-wash] budget record failed: %v", err)
		}
	}
	washLogUsage                                           = logDeveloperAPIUsage
	washPersist    func(s *Server, r *scanner.TokenResult) = func(s *Server, r *scanner.TokenResult) { s.persistManualScanAsync(r) }
	washLoadAnswer                                         = loadPersistedWashAnswer
	washSaveAnswer                                         = savePersistedWashAnswer
	washPollPeriod                                         = 250 * time.Millisecond
	washNow                                                = time.Now
)

type washState struct {
	mu        sync.Mutex
	keys      map[string]washKeyEntry // by key hash
	limiters  map[string]*rate.Limiter
	results   map[string]washResultEntry
	scans     map[string]washScanContext
	inflight  map[string]chan struct{}
	day       string
	dayFetchs int
}

type washKeyEntry struct {
	key     *washDevKey
	expires time.Time
}

type washResultEntry struct {
	body       map[string]interface{}
	computedAt time.Time
}

// washFreshTTL is how long a computed answer is served as-is. Repeat checks
// inside it cost no Helius fetch. Default 30 min (TELEGRAPH_WASH_FRESH_TTL_MIN).
func washFreshTTL() time.Duration {
	return time.Duration(washEnvInt("TELEGRAPH_WASH_FRESH_TTL_MIN", 30)) * time.Minute
}

var washGlobal = &washState{}

func (st *washState) init() {
	if st.keys == nil {
		st.keys = map[string]washKeyEntry{}
		st.limiters = map[string]*rate.Limiter{}
		st.results = map[string]washResultEntry{}
		st.scans = map[string]washScanContext{}
		st.inflight = map[string]chan struct{}{}
	}
}

func washEnvFloat(key string, def float64) float64 {
	if v, err := strconv.ParseFloat(strings.TrimSpace(os.Getenv(key)), 64); err == nil && v > 0 {
		return v
	}
	return def
}

func washEnvInt(key string, def int) int {
	if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv(key))); err == nil && v >= 0 {
		return v
	}
	return def
}

func writeWashError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code, "message": msg})
}

func extractDeveloperKey(r *http.Request) string {
	if auth := strings.TrimSpace(r.Header.Get("Authorization")); len(auth) > 7 && strings.EqualFold(auth[:7], "bearer ") {
		return strings.TrimSpace(auth[7:])
	}
	return strings.TrimSpace(r.Header.Get("X-Rugburn-API-Key"))
}

// ValidateWashSubject accepts only a base58 32-byte Solana address.
func ValidateWashSubject(subject string) (string, error) {
	subject = strings.TrimSpace(subject)
	if subject == "" {
		return "", errors.New("subject is required: a Solana token mint address (base58)")
	}
	if strings.HasPrefix(strings.ToLower(subject), "0x") {
		return "", errors.New("subject looks like an EVM address; only Solana SPL / Token-2022 mint addresses (base58) are supported")
	}
	if _, err := solana.PublicKeyFromBase58(subject); err != nil {
		return "", errors.New("subject must be a Solana SPL / Token-2022 mint address (base58, 32 bytes); other chains are not supported")
	}
	return subject, nil
}

func (s *Server) authorizeWashKey(raw string) (*washDevKey, int, string) {
	if raw == "" {
		return nil, http.StatusUnauthorized, "missing API key: send Authorization: Bearer <key> or X-Rugburn-API-Key"
	}
	sum := sha256.Sum256([]byte(raw))
	hash := hex.EncodeToString(sum[:])
	now := washNow()

	st := washGlobal
	st.mu.Lock()
	st.init()
	entry, ok := st.keys[hash]
	st.mu.Unlock()

	var key *washDevKey
	if ok && now.Before(entry.expires) {
		key = entry.key
	} else {
		found, err := washLookupKey(s.config, hash)
		if err != nil {
			// Do not cache lookup failures; the next call retries.
			return nil, http.StatusServiceUnavailable, "API key check unavailable, retry shortly"
		}
		ttl := 60 * time.Second
		if found == nil {
			ttl = 30 * time.Second
		}
		st.mu.Lock()
		st.keys[hash] = washKeyEntry{key: found, expires: now.Add(ttl)}
		st.mu.Unlock()
		key = found
	}
	if key == nil {
		return nil, http.StatusUnauthorized, "invalid API key"
	}
	if key.Status != "active" || (key.RevokedAt != nil && *key.RevokedAt != "") {
		return nil, http.StatusForbidden, "API key is not active"
	}
	if key.ExpiresAt != nil && *key.ExpiresAt != "" {
		if t, err := time.Parse(time.RFC3339Nano, *key.ExpiresAt); err == nil && !t.After(now) {
			return nil, http.StatusForbidden, "API key has expired"
		}
	}
	scoped := false
	for _, sc := range key.Scopes {
		if sc == "*" || sc == washScope {
			scoped = true
			break
		}
	}
	if !scoped {
		return nil, http.StatusForbidden, "API key lacks scope " + washScope
	}
	return key, 0, ""
}

func (s *Server) washAllow(keyID string) bool {
	st := washGlobal
	st.mu.Lock()
	defer st.mu.Unlock()
	st.init()
	lim := st.limiters[keyID]
	if lim == nil {
		rps := washEnvFloat("TELEGRAPH_WASH_RPS_PER_KEY", 2)
		lim = rate.NewLimiter(rate.Limit(rps), int(rps+0.999))
		st.limiters[keyID] = lim
	}
	return lim.Allow()
}

// washTakeFetch enforces a hard in-process daily cap on paid computations, on
// top of the provider budget, so a missing env limit can never let this route
// eat the shared Helius budget. TELEGRAPH_WASH_MAX_FETCHES_PER_DAY counts
// computed ANSWERS, not Helius calls: one answer may spend up to
// washMaxPages() Enhanced Transactions pages plus washMaxFunderLookups() gTFA
// calls, each billed to the telegraph_wash bucket, which is the unit cap.
func washTakeFetch() bool {
	limit := washEnvInt("TELEGRAPH_WASH_MAX_FETCHES_PER_DAY", 40)
	day := washNow().UTC().Format("2006-01-02")
	st := washGlobal
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.day != day {
		st.day, st.dayFetchs = day, 0
	}
	if st.dayFetchs >= limit {
		return false
	}
	st.dayFetchs++
	return true
}

func (s *Server) handleTelegraphWash(w http.ResponseWriter, r *http.Request) {
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
		go washLogUsage(s.config, key, r, code, washNow().Sub(started), errCode)
	}
	if !s.washAllow(key.ID) {
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
	body := s.washAnswer(ctx, mint)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "private, max-age=60")
	_ = json.NewEncoder(w).Encode(body)
	finish(http.StatusOK, "")
}

// washAnswer returns a cached answer, or joins/creates the single computation
// for this mint. Concurrent callers for one mint share one Helius fetch.
func (s *Server) washAnswer(ctx context.Context, mint string) map[string]interface{} {
	st := washGlobal
	for {
		st.mu.Lock()
		st.init()
		if e, ok := st.results[mint]; ok && washNow().Sub(e.computedAt) < washFreshTTL() {
			st.mu.Unlock()
			return washMarkCached(e.body)
		}
		ch, busy := st.inflight[mint]
		if !busy {
			ch = make(chan struct{})
			st.inflight[mint] = ch
			st.mu.Unlock()
			break
		}
		st.mu.Unlock()
		select {
		case <-ch:
			continue
		case <-ctx.Done():
			return washInconclusive(mint, "another request for this subject is still computing; retry in a few seconds", nil)
		}
	}
	body, cacheable := s.computeWash(ctx, mint)
	computedAt := washNow()
	if cacheable {
		go washSaveAnswer(s.config, mint, body, computedAt)
	}
	st.mu.Lock()
	if cacheable {
		st.results[mint] = washResultEntry{body: body, computedAt: computedAt}
		if len(st.results) > 5000 {
			for k, e := range st.results {
				if computedAt.Sub(e.computedAt) > washStaleMaxAge {
					delete(st.results, k)
				}
			}
		}
	}
	close(st.inflight[mint])
	delete(st.inflight, mint)
	st.mu.Unlock()
	return body
}

func washMarkCached(body map[string]interface{}) map[string]interface{} {
	out := make(map[string]interface{}, len(body)+1)
	for k, v := range body {
		out[k] = v
	}
	out["cached"] = true
	return out
}

// washScanContextFor never blocks past the swap reserve. Order: a scan this
// route ran in the last 15 min, a stored scan under 15 min, wait on an
// in-flight scan, run a fresh one, then any stored scan under 24 h.
func (s *Server) washScanContextFor(ctx context.Context, mint string) washScanContext {
	now := washNow()
	st := washGlobal
	st.mu.Lock()
	if sc, ok := st.scans[mint]; ok && now.Sub(sc.ScannedAt) <= washFreshScanMaxAge {
		st.mu.Unlock()
		return sc
	}
	st.mu.Unlock()

	stored, _ := washLatestScan(s.config, mint)
	if stored != nil && now.Sub(stored.ScannedAt) <= washFreshScanMaxAge {
		return *stored
	}

	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = now.Add(washTotalCap)
	}
	scanCtx, cancel := context.WithDeadline(ctx, deadline.Add(-washSwapReserve))
	defer cancel()

	fallback := func() washScanContext {
		if latest, _ := washLatestScan(s.config, mint); latest != nil && washNow().Sub(latest.ScannedAt) <= washStaleScanMaxAge {
			return *latest
		}
		if stored != nil && washNow().Sub(stored.ScannedAt) <= washStaleScanMaxAge {
			return *stored
		}
		return washScanContext{Source: "none"}
	}

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
	type scanOut struct {
		res *scanner.TokenResult
		err error
	}
	done := make(chan scanOut, 1)
	go func() {
		res, err := washRunScan(s.config, mint)
		if err == nil && res != nil {
			washRecordUsage(s.config, "scan", s.config.HeliusFullScanEstimatedCost, map[string]interface{}{"address": mint, "scan_source": "telegraph", "rpc_calls_used": res.RPCCallsUsed})
			washPersist(s, res)
			sc := washScanContext{Source: "fresh_scan", PoolAddress: res.PoolAddress, DexID: res.DexID, LiquidityUSD: res.LiquidityUSD, Volume24h: res.Volume24h, Buys24h: res.Buys24h, Sells24h: res.Sells24h, ScannedAt: washNow()}
			if sc.PoolAddress == "" {
				if p, ok := res.RawOnchainData["pairAddress"].(string); ok {
					sc.PoolAddress = p
				}
			}
			st.mu.Lock()
			st.scans[mint] = sc
			st.mu.Unlock()
		}
		done <- scanOut{res, err}
	}()
	select {
	case out := <-done:
		if out.err == nil && out.res != nil {
			st.mu.Lock()
			sc := st.scans[mint]
			st.mu.Unlock()
			return sc
		}
		// ErrScanTooFresh / ErrScanInFlight / failure: use what is stored.
		return fallback()
	case <-scanCtx.Done():
		// The scan keeps running and persists itself; answer from storage now.
		return fallback()
	}
}

// computeWash returns the response body and whether it may be cached.
func (s *Server) computeWash(ctx context.Context, mint string) (map[string]interface{}, bool) {
	// The SOL/USD rate (≤2 s, cached) is fetched while the scan context resolves.
	rateCh := make(chan float64, 1)
	solUSD := washSOLUSD
	go func() { rateCh <- solUSD(s.config) }()
	sc := s.washScanContextFor(ctx, mint)
	// Main pool first (sees every swap on it), then the mint (other pools,
	// aggregator routes). Deduplicated inside the collector.
	sources := []string{}
	if sc.PoolAddress != "" && sc.PoolAddress != mint {
		sources = append(sources, sc.PoolAddress)
	}
	sources = append(sources, mint)

	// Same per-call estimate the behavior indexer bills for one Enhanced
	// Transactions call; billed per page actually requested.
	pageCost := s.config.HeliusBehaviorIndexCostUnits
	if pageCost <= 0 {
		pageCost = 50
	}
	if allowed, reason := washBudgetAllowed(s.config, pageCost); !allowed {
		return s.washStaleOr(mint, "daily Helius budget for this route is spent ("+reason+")", &sc), false
	}
	if !washTakeFetch() {
		return s.washStaleOr(mint, "daily fetch cap for this route is reached", &sc), false
	}

	cost := washCost{PageUnits: pageCost}
	plan := scanner.WashFetchPlan{
		Sources:      sources,
		MaxPages:     washMaxPages(),
		Since:        washNow().Add(-washLookback),
		TargetSwaps:  washTargetSwaps,
		MinRemaining: washPageMinRemaining,
		SOLUSD:       <-rateCh,
		AllowPage: func(string) bool {
			ok, _ := washBudgetAllowed(s.config, pageCost)
			return ok
		},
		OnPage: func(src string, txs int, err error) {
			cost.Pages++
			washRecordUsage(s.config, "enhanced_transactions", pageCost, map[string]interface{}{"address": mint, "query_address": src, "ok": err == nil, "txs": txs})
		},
	}
	sample, err := washFetchSample(ctx, s.config, plan, mint)
	if err != nil {
		log.Printf("[telegraph-wash] swap fetch failed for %s: %v", mint, err)
		return s.washStaleOr(mint, "swap history is unavailable from Helius right now", &sc), false
	}
	swaps := sample.Swaps

	wallets := make([]string, 0, len(swaps))
	seen := map[string]struct{}{}
	for _, sw := range swaps {
		if _, ok := seen[sw.Wallet]; !ok {
			seen[sw.Wallet] = struct{}{}
			wallets = append(wallets, sw.Wallet)
		}
	}
	params := scanner.DefaultWashParams()
	facts := washFunderFacts(s.config, wallets)
	if facts == nil {
		facts = map[string]scanner.FundingFact{}
	}
	cost.FunderLookups, cost.FundersResolved = s.washLookupFunders(ctx, mint, swaps, facts, params)
	clusters := washClustersFromFacts(s.config, facts)
	m := scanner.MeasureWash(swaps, clusters, params)
	var multi []washMultiMarket
	if m.WashVolumePct != nil && *m.WashVolumePct >= washMultiMarketCheckFrom {
		multi = s.washCheckMultiMarket(ctx, mint, m, &cost)
		if len(multi) > 0 {
			params.Unmatched = map[string]bool{}
			for _, mm := range multi {
				params.Unmatched[mm.Wallet] = true
			}
			m = scanner.MeasureWash(swaps, clusters, params)
		}
	}
	resp := BuildWashResponse(mint, m, sc, sample, cost)
	if len(multi) > 0 {
		addMultiMarketSignal(resp, m, multi)
	}
	return resp, true
}

// washMultiMarket is one matched wallet found trading many other tokens.
type washMultiMarket struct {
	Wallet string `json:"wallet"`
	scanner.WashWalletActivity
}

// washCheckMultiMarket reads one page of recent activity for the largest
// single-wallet matched entities (scanner.WashMultiMarketCandidates) and
// returns those whose activity is mostly in other tokens
// (scanner.WashWalletActivity.MultiMarket). Bounded by the per-answer cap, the remaining time and the bucket.
func (s *Server) washCheckMultiMarket(ctx context.Context, mint string, m scanner.WashMeasurement, cost *washCost) []washMultiMarket {
	candidates := scanner.WashMultiMarketCandidates(m, washMaxActivityLookups(), washMultiMarketMinShare)
	if len(candidates) == 0 {
		return nil
	}
	if dl, ok := ctx.Deadline(); ok && time.Until(dl) < washFunderMinRemaining {
		return nil
	}
	if ok, _ := washBudgetAllowed(s.config, len(candidates)*cost.PageUnits); !ok {
		return nil
	}
	type res struct {
		wallet string
		a      scanner.WashWalletActivity
		err    error
	}
	out := make(chan res, len(candidates))
	for _, w := range candidates {
		w := w
		go func() {
			a, err := washWalletActivity(ctx, s.config, w, mint)
			out <- res{w, a, err}
		}()
	}
	found := []washMultiMarket{}
	for range candidates {
		r := <-out
		cost.ActivityLookups++
		washRecordUsage(s.config, "enhanced_transactions", cost.PageUnits, map[string]interface{}{"address": mint, "query_address": r.wallet, "ok": r.err == nil, "purpose": "wallet_activity"})
		if r.err == nil && r.a.MultiMarket() {
			found = append(found, washMultiMarket{Wallet: r.wallet, WashWalletActivity: r.a})
		}
	}
	sort.Slice(found, func(i, j int) bool { return found[i].Wallet < found[j].Wallet })
	return found
}

func addMultiMarketSignal(resp map[string]interface{}, m scanner.WashMeasurement, multi []washMultiMarket) {
	signals, _ := resp["signals"].([]map[string]interface{})
	resp["signals"] = append(signals, map[string]interface{}{
		"name":     "multi_market_wallets_unmatched",
		"category": "observed",
		"note":     fmt.Sprintf("These wallets moved %d or more other token mints in their newest 100 transactions, in at least a quarter of their trading transactions: the footprint of arbitrage or market-making bots trading many markets. Their volume stays in the total but is not matched as round trips.", scanner.WashMultiMarketMinMints),
		"evidence": map[string]interface{}{
			"wallets":              multi,
			"unmatched_volume_sol": m.UnmatchedSOL,
		},
	})
}

// washCost is what one computed answer spent, in budget units.
type washCost struct {
	PageUnits       int
	Pages           int
	FunderLookups   int
	FundersResolved int
	ActivityLookups int
}

func (c washCost) units() int {
	return (c.Pages+c.ActivityLookups)*c.PageUnits + c.FunderLookups*scanner.WashFunderLookupUnits
}

// washLookupFunders resolves first funders for the few wallets whose
// cross-wallet round trips could become shared-funder matches (see
// scanner.WashFunderCandidates), within the per-answer cap, the remaining
// time and the telegraph_wash bucket. New facts are merged into facts and
// written to the funding graph so the next answer gets them free.
func (s *Server) washLookupFunders(ctx context.Context, mint string, swaps []scanner.WashSwap, facts map[string]scanner.FundingFact, params scanner.WashParams) (attempted, resolved int) {
	limit := washMaxFunderLookups()
	if limit <= 0 {
		return 0, 0
	}
	if dl, ok := ctx.Deadline(); ok && time.Until(dl) < washFunderMinRemaining {
		return 0, 0
	}
	known := make(map[string]bool, len(facts))
	for w := range facts {
		known[w] = true
	}
	candidates := scanner.WashFunderCandidates(swaps, known, params, limit)
	if len(candidates) == 0 {
		return 0, 0
	}
	if ok, _ := washBudgetAllowed(s.config, len(candidates)*scanner.WashFunderLookupUnits); !ok {
		return 0, 0
	}
	type res struct {
		wallet string
		fact   scanner.FundingFact
		err    error
	}
	out := make(chan res, len(candidates))
	sem := make(chan struct{}, washFunderConcurrency)
	for _, w := range candidates {
		w := w
		go func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			f, err := washResolveFunder(ctx, s.config, w)
			out <- res{w, f, err}
		}()
	}
	newFacts := map[string]scanner.FundingFact{}
	negatives := []string{}
	failed := 0
	for range candidates {
		r := <-out
		attempted++
		if r.err != nil {
			failed++ // errors may embed provider URLs; never logged verbatim
			continue
		}
		resolved++
		facts[r.wallet] = r.fact
		if r.fact.Funder != "" {
			newFacts[r.wallet] = r.fact
		} else {
			negatives = append(negatives, r.wallet)
		}
	}
	if failed > 0 {
		log.Printf("[telegraph-wash] %d of %d funder lookups failed for %s", failed, attempted, mint)
	}
	if len(newFacts) > 0 || len(negatives) > 0 {
		go washPersistFacts(s.config, mint, newFacts, negatives)
	}
	return attempted, resolved
}

// cachedSOLUSD is the SOL/USD rate used to price USDC/USDT legs: DexScreener's
// deepest wSOL pair, cached 10 min, sanity-bounded. 0 means unavailable, and
// stable-quoted swaps then stay unpriced instead of being guessed.
var solUSDCache struct {
	sync.Mutex
	rate float64
	at   time.Time
}

func cachedSOLUSD(cfg config.Config) float64 {
	solUSDCache.Lock()
	if solUSDCache.rate > 0 && time.Since(solUSDCache.at) < 10*time.Minute {
		r := solUSDCache.rate
		solUSDCache.Unlock()
		return r
	}
	solUSDCache.Unlock()
	ch := make(chan float64, 1)
	go func() {
		d, err := scanner.FetchDexScreener(cfg, "So11111111111111111111111111111111111111112")
		if err != nil || d == nil {
			ch <- 0
			return
		}
		ch <- d.PriceUSD
	}()
	select {
	case r := <-ch:
		if r < 1 || r > 100000 {
			return 0
		}
		solUSDCache.Lock()
		solUSDCache.rate, solUSDCache.at = r, time.Now()
		solUSDCache.Unlock()
		return r
	case <-time.After(2 * time.Second):
		return 0
	}
}

// Verdict cutoffs on wash_volume_pct. Calibrated on 21 Nov 2026 against the
// 150 accepted Solana preference pairs for WASH_TRADING_DETECTION_GENERAL
// (Mongardini & Mei archive, Zenodo 17830944), using each token-day's matched
// round-trip share (2 x min(buy, sell) per maker over day volume) as a proxy
// for this metric. 40/15 put 55 of 63 "likely" pairs in wash_trading and 19
// of 34 "organic" pairs in likely_wash_trading (3-class agreement 51%);
// 70/35 agrees on 83% (81% in repeated 5-fold cross-validation). The proxy
// has no 10-minute or 25% size limit, so it reads at or above this matcher's
// own-wallet share; treat these as upper-end cutoffs.
const (
	washCutoffWash   = 70.0
	washCutoffLikely = 35.0
)

// WashVerdict maps the measured share to the intent enum.
func WashVerdict(m scanner.WashMeasurement) string {
	if !m.Sufficient || m.WashVolumePct == nil {
		return "inconclusive"
	}
	switch p := *m.WashVolumePct; {
	case p >= washCutoffWash:
		return "wash_trading"
	case p >= washCutoffLikely:
		return "likely_wash_trading"
	default:
		return "organic"
	}
}

func washInconclusive(mint, reason string, sc *washScanContext) map[string]interface{} {
	signals := []map[string]interface{}{}
	if sc != nil {
		signals = append(signals, washMarketSignals(*sc)...)
	}
	return map[string]interface{}{
		"subject":             mint,
		"chain":               "solana",
		"verdict":             "inconclusive",
		"wash_volume_pct":     nil,
		"signals":             signals,
		"summary":             "Inconclusive: " + reason + ".",
		"inconclusive_reason": reason,
		"as_of":               washNow().UTC().Format(time.RFC3339),
		"source":              "rugburn",
	}
}

func washMarketSignals(sc washScanContext) []map[string]interface{} {
	if sc.Source == "none" || sc.ScannedAt.IsZero() {
		return nil
	}
	ev := map[string]interface{}{
		"volume_24h_usd": sc.Volume24h,
		"liquidity_usd":  sc.LiquidityUSD,
		"buys_24h":       sc.Buys24h,
		"sells_24h":      sc.Sells24h,
		"dex_id":         sc.DexID,
		"scan_source":    sc.Source,
		"scanned_at":     sc.ScannedAt.UTC().Format(time.RFC3339),
	}
	if sc.LiquidityUSD > 0 {
		ev["volume_to_liquidity"] = roundTo(sc.Volume24h/sc.LiquidityUSD, 4)
	}
	return []map[string]interface{}{{
		"name":     "market_activity_24h",
		"category": "observed",
		"note":     "Context from the token scan's market data. Not used in wash_volume_pct.",
		"evidence": ev,
	}}
}

func roundTo(v float64, places int) float64 {
	p := 1.0
	for i := 0; i < places; i++ {
		p *= 10
	}
	if v >= 0 {
		return float64(int64(v*p+0.5)) / p
	}
	return float64(int64(v*p-0.5)) / p
}

// BuildWashResponse renders the WASH intent shape. Observed facts, derived
// values and hypotheses are labelled separately in signals; nothing here says
// who controls a wallet.
func BuildWashResponse(mint string, m scanner.WashMeasurement, sc washScanContext, sample scanner.WashSample, cost washCost) map[string]interface{} {
	verdict := WashVerdict(m)
	window := map[string]interface{}{}
	if !m.WindowStart.IsZero() {
		window["start"] = m.WindowStart.UTC().Format(time.RFC3339)
		window["end"] = m.WindowEnd.UTC().Format(time.RFC3339)
	}
	signals := []map[string]interface{}{
		{
			"name":     "swap_sample",
			"category": "observed",
			"evidence": map[string]interface{}{
				"swaps_observed":            m.SwapsObserved,
				"sol_priced_swaps":          m.PricedSwaps,
				"unpriced_swaps":            m.UnpricedSwaps,
				"distinct_wallets":          m.Wallets,
				"total_volume_sol":          m.TotalVolume,
				"window":                    window,
				"sol_quoted_swaps":          m.PricedSOL,
				"usd_quoted_swaps":          m.PricedUSD,
				"sol_usd_rate":              nilIfZero(sample.SOLUSD),
				"query_address":             washFirstSource(sample.Sources),
				"sources":                   washSourcesEvidence(sample.Sources),
				"pages_fetched":             sample.Pages,
				"pages_failed":              sample.FailedPages,
				"paging_stop_reason":        sample.StopReason,
				"swaps_trimmed":             sample.TrimmedSwaps,
				"route_hops_skipped":        sample.RouteHops,
				"wallets_with_known_funder": m.FunderKnown,
				"funder_lookups":            cost.FunderLookups,
				"funders_resolved":          cost.FundersResolved,
			},
		},
	}
	if m.Sufficient {
		signals = append(signals,
			map[string]interface{}{
				"name":     "self_round_trips",
				"category": "derived",
				"evidence": map[string]interface{}{
					"matched_pairs":      m.SelfPairs,
					"matched_volume_sol": m.SelfMatchedSOL,
					"rule":               "same wallet, opposite side, sizes within 25%, legs within 10 min",
				},
			},
			map[string]interface{}{
				"name":     "shared_funder_round_trips",
				"category": "derived",
				"evidence": map[string]interface{}{
					"matched_pairs":          m.ClusterPairs,
					"matched_volume_sol":     m.ClusterMatchedSOL,
					"funder_clusters":        m.Clusters,
					"largest_funder_cluster": m.LargestCluster,
					"rule":                   "wallets sharing a recorded first-funding tx or funder (exchange/infra funders excluded), opposite side, sizes within 25%, legs within 10 min",
				},
			},
		)
		if len(m.TopEntities) > 0 {
			signals = append(signals, map[string]interface{}{
				"name":     "top_matched_entities",
				"category": "derived",
				"evidence": map[string]interface{}{"entities": m.TopEntities},
			})
		}
		if m.Wallets > 0 && float64(m.FunderKnown)/float64(m.Wallets) < 0.3 {
			signals = append(signals, map[string]interface{}{
				"name":     "low_funding_coverage",
				"category": "derived",
				"note":     "Fewer than 30% of wallets have a recorded funder, so shared-funder matching is a lower bound.",
				"evidence": map[string]interface{}{"wallets_with_known_funder": m.FunderKnown, "distinct_wallets": m.Wallets},
			})
		}
		if verdict == "wash_trading" || verdict == "likely_wash_trading" {
			signals = append(signals, map[string]interface{}{
				"name":     "round_trip_pattern",
				"category": "hypothesis",
				"note":     "Matched volume is consistent with volume being cycled between commonly funded or identical wallets. Fast legitimate flipping produces the same footprint; this does not establish who controls the wallets or why.",
			})
		}
	}
	signals = append(signals, washMarketSignals(sc)...)

	var summary string
	body := map[string]interface{}{
		"subject":         mint,
		"chain":           "solana",
		"verdict":         verdict,
		"wash_volume_pct": nil,
		"signals":         signals,
		"as_of":           washNow().UTC().Format(time.RFC3339),
		"source":          "rugburn",
		"method":          fmt.Sprintf("Greedy chronological matching of opposite-side swaps of similar size per wallet and per shared-funder cluster. Sample: up to %d pages of ≤100 recent transactions (last 24 h) read from the main pool and the mint, deduplicated, trimmed to the window every source fully covers. Swaps are priced from the trader's own SOL/wSOL leg, or USDC/USDT converted at one SOL/USD rate; swaps without such a leg are unpriced and left out. wash_volume_pct = matched round-trip volume (both legs) ÷ total priced swap volume × 100.", washMaxPages()),
		"cost": map[string]interface{}{
			"helius_pages":   cost.Pages,
			"funder_lookups": cost.FunderLookups,
			"activity_pages": cost.ActivityLookups,
			"budget_units":   cost.units(),
			"budget_bucket":  washBudgetFeature,
			"note":           "Spent when this answer was computed (as_of). A cached answer costs nothing.",
		},
	}
	if m.Sufficient && m.WashVolumePct != nil {
		pct := *m.WashVolumePct
		body["wash_volume_pct"] = pct
		summary = fmt.Sprintf("%s: %.2f%% of %.4f SOL across %d priced swaps by %d wallets was matched self or shared-funder round trips (%d self pairs, %d cluster pairs).",
			washVerdictLabel(verdict), pct, m.TotalVolume, m.PricedSwaps, m.Wallets, m.SelfPairs, m.ClusterPairs)
	} else {
		why := m.InsufficientWhy
		if sample.RouteHops > 0 && sample.RouteHops >= m.PricedSwaps {
			why += fmt.Sprintf("; %d other swap transactions only passed the token through (aggregator routes or arbitrage loops) and are not trades of it", sample.RouteHops)
		}
		body["inconclusive_reason"] = why
		summary = "Inconclusive: " + why + "."
	}
	body["summary"] = summary
	return body
}

func nilIfZero(v float64) interface{} {
	if v == 0 {
		return nil
	}
	return v
}

func washFirstSource(src []scanner.WashSourceCoverage) string {
	if len(src) == 0 {
		return ""
	}
	return src[0].Address
}

func washSourcesEvidence(src []scanner.WashSourceCoverage) []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(src))
	for _, c := range src {
		e := map[string]interface{}{
			"address":           c.Address,
			"pages":             c.Pages,
			"transactions":      c.Txs,
			"swaps_extracted":   c.Swaps,
			"history_exhausted": c.Exhausted,
		}
		if !c.Oldest.IsZero() {
			e["oldest_tx_time"] = c.Oldest.UTC().Format(time.RFC3339)
		}
		if c.Error != "" {
			e["error"] = "fetch failed"
		}
		out = append(out, e)
	}
	return out
}

func washVerdictLabel(v string) string {
	switch v {
	case "wash_trading":
		return "Wash trading"
	case "likely_wash_trading":
		return "Likely wash trading"
	case "organic":
		return "Organic"
	}
	return "Inconclusive"
}

func washSupabaseGet(cfg config.Config, path string, out interface{}) error {
	if cfg.SupabaseURL == "" || cfg.SupabaseServiceRole == "" {
		return errors.New("supabase not configured")
	}
	req, err := http.NewRequest(http.MethodGet, strings.TrimRight(cfg.SupabaseURL, "/")+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("apikey", cfg.SupabaseServiceRole)
	req.Header.Set("Authorization", "Bearer "+cfg.SupabaseServiceRole)
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("supabase status %d", resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out)
}

func lookupDeveloperKeyByHash(cfg config.Config, hash string) (*washDevKey, error) {
	var rows []washDevKey
	q := url.Values{}
	q.Set("select", "id,user_id,status,scopes,expires_at,revoked_at")
	q.Set("key_hash", "eq."+hash)
	q.Set("limit", "1")
	if err := washSupabaseGet(cfg, "/rest/v1/developer_api_keys?"+q.Encode(), &rows); err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return &rows[0], nil
}

func latestStoredScanContext(cfg config.Config, mint string) (*washScanContext, error) {
	var rows []struct {
		PoolAddress  *string  `json:"pool_address"`
		DexID        *string  `json:"dex_id"`
		LiquidityUSD *float64 `json:"liquidity_usd"`
		Volume24h    *float64 `json:"volume_24h"`
		Buys24h      *int     `json:"buy_count_24h"`
		Sells24h     *int     `json:"sell_count_24h"`
		ScannedAt    string   `json:"scanned_at"`
	}
	q := url.Values{}
	q.Set("select", "pool_address,dex_id,liquidity_usd,volume_24h,buy_count_24h,sell_count_24h,scanned_at")
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
	sc := &washScanContext{Source: "stored_scan", ScannedAt: t}
	if row.PoolAddress != nil {
		sc.PoolAddress = *row.PoolAddress
	}
	if row.DexID != nil {
		sc.DexID = *row.DexID
	}
	if row.LiquidityUSD != nil {
		sc.LiquidityUSD = *row.LiquidityUSD
	}
	if row.Volume24h != nil {
		sc.Volume24h = *row.Volume24h
	}
	if row.Buys24h != nil {
		sc.Buys24h = *row.Buys24h
	}
	if row.Sells24h != nil {
		sc.Sells24h = *row.Sells24h
	}
	return sc, nil
}

// logDeveloperAPIUsage mirrors the Next.js developer API's api_usage_events
// row so this route shows up in the same usage views. Best effort.
func logDeveloperAPIUsage(cfg config.Config, key *washDevKey, r *http.Request, status int, latency time.Duration, errCode string) {
	logDeveloperAPIUsageFor(cfg, "/telegraph/wash", key, r, status, latency, errCode)
}

func logDeveloperAPIUsageFor(cfg config.Config, endpoint string, key *washDevKey, r *http.Request, status int, latency time.Duration, errCode string) {
	if cfg.SupabaseURL == "" || cfg.SupabaseServiceRole == "" || key == nil {
		return
	}
	row := map[string]interface{}{
		"api_key_id":  key.ID,
		"user_id":     key.UserID,
		"request_id":  fmt.Sprintf("tgw-%d", time.Now().UnixNano()),
		"endpoint":    endpoint,
		"method":      r.Method,
		"scope":       washScope,
		"status_code": status,
		"cost_units":  1,
		"latency_ms":  latency.Milliseconds(),
	}
	if ua := r.UserAgent(); ua != "" {
		if len(ua) > 500 {
			ua = ua[:500]
		}
		row["user_agent"] = ua
	}
	if errCode != "" {
		row["error_code"] = errCode
	}
	payload, _ := json.Marshal(row)
	req, err := http.NewRequest(http.MethodPost, strings.TrimRight(cfg.SupabaseURL, "/")+"/rest/v1/api_usage_events", bytes.NewReader(payload))
	if err != nil {
		return
	}
	req.Header.Set("apikey", cfg.SupabaseServiceRole)
	req.Header.Set("Authorization", "Bearer "+cfg.SupabaseServiceRole)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Prefer", "return=minimal")
	resp, err := (&http.Client{Timeout: 3 * time.Second}).Do(req)
	if err != nil {
		log.Printf("[telegraph-wash] usage insert failed: %v", err)
		return
	}
	resp.Body.Close()
}

// washStaleOr serves the last computed answer for mint when a new one cannot
// be computed (budget, cap or Helius failure): the in-memory copy if it is
// under 24 h old, else the persisted one, whatever its age. The original as_of
// is kept, the body is marked stale and a signal says why. Only when no prior
// answer exists does the caller get an inconclusive verdict.
func (s *Server) washStaleOr(mint, reason string, sc *washScanContext) map[string]interface{} {
	now := washNow()
	st := washGlobal
	st.mu.Lock()
	e, ok := st.results[mint]
	st.mu.Unlock()
	if ok && now.Sub(e.computedAt) <= washStaleMaxAge {
		return washStaleBody(e.body, e.computedAt, reason, "memory")
	}
	body, at, err := washLoadAnswer(s.config, mint)
	if err != nil {
		log.Printf("[telegraph-wash] persisted answer read failed for %s: %v", mint, err)
	}
	if body != nil {
		return washStaleBody(body, at, reason, "persisted")
	}
	return washInconclusive(mint, "no new answer could be computed ("+reason+") and none is stored", sc)
}

func washStaleBody(body map[string]interface{}, computedAt time.Time, reason, from string) map[string]interface{} {
	out := make(map[string]interface{}, len(body)+3)
	for k, v := range body {
		out[k] = v
	}
	delete(out, "cached")
	if _, ok := out["as_of"]; !ok {
		out["as_of"] = computedAt.UTC().Format(time.RFC3339)
	}
	out["stale"] = true
	out["served_at"] = washNow().UTC().Format(time.RFC3339)
	signals := []interface{}{}
	switch v := body["signals"].(type) {
	case []interface{}:
		signals = append(signals, v...)
	case []map[string]interface{}:
		for _, m := range v {
			signals = append(signals, m)
		}
	}
	signals = append(signals, map[string]interface{}{
		"name":     "cached_answer",
		"category": "observed",
		"note":     "Previously computed answer, served because a new one could not be computed: " + reason + ". Figures describe the window as of as_of, not now.",
		"evidence": map[string]interface{}{
			"computed_at": computedAt.UTC().Format(time.RFC3339),
			"age_seconds": int64(washNow().Sub(computedAt).Seconds()),
			"reason":      reason,
			"store":       from,
		},
	})
	out["signals"] = signals
	return out
}

// telegraph_wash_answers (migration 046) keeps the last answer per mint.
func loadPersistedWashAnswer(cfg config.Config, mint string) (map[string]interface{}, time.Time, error) {
	var rows []struct {
		Body       map[string]interface{} `json:"body"`
		ComputedAt string                 `json:"computed_at"`
	}
	q := url.Values{}
	q.Set("select", "body,computed_at")
	q.Set("mint", "eq."+mint)
	q.Set("limit", "1")
	if err := washSupabaseGet(cfg, "/rest/v1/telegraph_wash_answers?"+q.Encode(), &rows); err != nil {
		return nil, time.Time{}, err
	}
	if len(rows) == 0 || rows[0].Body == nil {
		return nil, time.Time{}, nil
	}
	at, err := time.Parse(time.RFC3339Nano, rows[0].ComputedAt)
	if err != nil {
		return nil, time.Time{}, err
	}
	return rows[0].Body, at, nil
}

func savePersistedWashAnswer(cfg config.Config, mint string, body map[string]interface{}, computedAt time.Time) {
	if cfg.SupabaseURL == "" || cfg.SupabaseServiceRole == "" {
		return
	}
	verdict, _ := body["verdict"].(string)
	row := map[string]interface{}{
		"mint":            mint,
		"verdict":         verdict,
		"wash_volume_pct": body["wash_volume_pct"],
		"body":            body,
		"computed_at":     computedAt.UTC().Format(time.RFC3339Nano),
		"updated_at":      time.Now().UTC().Format(time.RFC3339Nano),
	}
	payload, err := json.Marshal(row)
	if err != nil {
		return
	}
	req, err := http.NewRequest(http.MethodPost, strings.TrimRight(cfg.SupabaseURL, "/")+"/rest/v1/telegraph_wash_answers?on_conflict=mint", bytes.NewReader(payload))
	if err != nil {
		return
	}
	req.Header.Set("apikey", cfg.SupabaseServiceRole)
	req.Header.Set("Authorization", "Bearer "+cfg.SupabaseServiceRole)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Prefer", "resolution=merge-duplicates,return=minimal")
	resp, err := (&http.Client{Timeout: 3 * time.Second}).Do(req)
	if err != nil {
		log.Printf("[telegraph-wash] persist answer failed for %s: %v", mint, err)
		return
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		log.Printf("[telegraph-wash] persist answer status %d for %s", resp.StatusCode, mint)
	}
}

