package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"rugburn-worker/internal/config"
	"rugburn-worker/internal/scanner"
)

const (
	pfUSDC  = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"
	pfUSDT  = "Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB"
	pfToken = "3GN46GsmAUQSpHXtY3oasJFYAbp9RBzVoR7X8ar1pump"
)

// cleanFacts: renounced, LP burned, a week old, liquid, holders spread.
func cleanFacts() preflightFacts {
	return preflightFacts{
		Source:       "fresh_scan",
		ScannedAt:    time.Now(),
		Authority:    &scanner.AuthorityResult{IsMintRenounced: true, IsFreezeRenounced: true},
		Tax:          &scanner.TaxResult{SimulationWorked: true, HiddenTaxPct: 0.4},
		LP:           &scanner.LPLockResult{Method: "burned", BurnedPercent: 100},
		Holders:      &scanner.HolderAnalysis{ScoredHolderRecords: 20, Top1HolderPct: 4, Top10HolderPct: 22},
		LiquidityUSD: 250000,
		AgeKnown:     true,
		PairAgeHours: 24 * 7,
		PoolAddress:  "Pool1",
	}
}

var (
	pfWashOrganic      = map[string]interface{}{"verdict": "organic", "wash_volume_pct": 3.1}
	pfWashLikely       = map[string]interface{}{"verdict": "likely_wash_trading", "wash_volume_pct": 25.2}
	pfWashInconclusive = map[string]interface{}{"verdict": "inconclusive", "wash_volume_pct": nil, "inconclusive_reason": "only 8 priced swaps"}
)

func reasonByName(rs []PreflightReason, name string) *PreflightReason {
	for i := range rs {
		if rs[i].Name == name {
			return &rs[i]
		}
	}
	return nil
}

func expectVerdict(t *testing.T, label, mint string, f preflightFacts, wash map[string]interface{}, want string, wantReason string, wantEffect string) []PreflightReason {
	t.Helper()
	v, rs := EvaluatePreflight(mint, f, wash)
	if v != want {
		t.Fatalf("%s: verdict %s want %s; reasons %+v", label, v, want, rs)
	}
	if wantReason != "" {
		r := reasonByName(rs, wantReason)
		if r == nil || r.Effect != wantEffect {
			t.Fatalf("%s: reason %s effect %v want %s; reasons %+v", label, wantReason, r, wantEffect, rs)
		}
		if r.Category != catObserved && r.Category != catDerived && r.Category != catHypothesis {
			t.Fatalf("%s: bad category %q", label, r.Category)
		}
		if r.Evidence["source"] == nil {
			t.Fatalf("%s: reason %s has no evidence source", label, wantReason)
		}
	}
	return rs
}

func TestPreflightCleanTokenHolds(t *testing.T) {
	expectVerdict(t, "organic", pfToken, cleanFacts(), pfWashOrganic, "HOLD", "wash_check_clear", effectNone)
}

func TestPreflightInconclusiveWashNeverMovesVerdict(t *testing.T) {
	expectVerdict(t, "clean+inconclusive", pfToken, cleanFacts(), pfWashInconclusive, "HOLD", "wash_check_inconclusive", effectNone)
	// Even with nothing else known, an inconclusive wash is not a reject.
	expectVerdict(t, "no scan+inconclusive", pfToken, preflightFacts{Source: "none"}, pfWashInconclusive, "REVIEW", "scan_unavailable", effectReview)
}

func TestPreflightWashLikelyIsReviewHypothesis(t *testing.T) {
	rs := expectVerdict(t, "likely", pfToken, cleanFacts(), pfWashLikely, "REVIEW", "likely_wash_trading", effectReview)
	if reasonByName(rs, "likely_wash_trading").Category != catHypothesis {
		t.Fatal("wash pattern must be labelled a hypothesis")
	}
	f := cleanFacts()
	expectVerdict(t, "wash_trading", pfToken, f, map[string]interface{}{"verdict": "wash_trading", "wash_volume_pct": 60.0}, "REVIEW", "likely_wash_trading", effectReview)
}

func TestPreflightStablecoinAllowlist(t *testing.T) {
	// USDC/USDT keep live mint and freeze authorities; USDC's wash check came
	// back inconclusive and a competitor's rejected it. Ours must not.
	for _, mint := range []string{pfUSDC, pfUSDT} {
		f := cleanFacts()
		f.Authority = &scanner.AuthorityResult{MintAuthority: "IssuerMintAuth", FreezeAuthority: "IssuerFreezeAuth"}
		f.Holders = &scanner.HolderAnalysis{ScoredHolderRecords: 20, Top1HolderPct: 30, Top10HolderPct: 70}
		f.LP = nil
		rs := expectVerdict(t, mint, mint, f, pfWashInconclusive, "HOLD", "mint_authority_active", effectNone)
		if r := reasonByName(rs, "freeze_authority_active"); r == nil || r.Effect != effectNone {
			t.Fatalf("freeze authority on allowlisted mint must be reported with no effect: %+v", r)
		}
		if r := reasonByName(rs, "known_mint_allowlist"); r == nil || r.Category != catObserved || r.Note == "" {
			t.Fatalf("allowlist reason missing: %+v", r)
		}
	}
	// The allowlist covers issuer controls only: a hidden sell tax still rejects.
	f := cleanFacts()
	f.Tax = &scanner.TaxResult{SimulationWorked: true, HiddenTaxPct: 50}
	expectVerdict(t, "allowlisted but taxed", pfUSDC, f, pfWashOrganic, "REJECT", "sell_simulation_hidden_tax", effectReject)
}

func TestPreflightHardRejects(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*preflightFacts)
		reason string
	}{
		{"mint authority", func(f *preflightFacts) { f.Authority = &scanner.AuthorityResult{MintAuthority: "MintAuth1"} }, "mint_authority_active"},
		{"freeze authority", func(f *preflightFacts) { f.Authority = &scanner.AuthorityResult{FreezeAuthority: "FreezeAuth1"} }, "freeze_authority_active"},
		{"extreme transfer fee", func(f *preflightFacts) {
			f.Tax = &scanner.TaxResult{IsToken2022: true, HasTransferFee: true, TransferFeeBps: 1500}
		}, "token2022_transfer_fee_extreme"},
		{"non transferable", func(f *preflightFacts) { f.Tax = &scanner.TaxResult{IsToken2022: true, IsNonTransferable: true} }, "token2022_non_transferable"},
		{"permanent delegate", func(f *preflightFacts) {
			f.Tax = &scanner.TaxResult{IsToken2022: true, HasPermanentDelegate: true, PermanentDelegate: "Deleg"}
		}, "token2022_permanent_delegate"},
		{"default frozen", func(f *preflightFacts) { f.Tax = &scanner.TaxResult{IsToken2022: true, DefaultAccountFrozen: true} }, "token2022_default_frozen"},
		{"paused", func(f *preflightFacts) {
			f.Tax = &scanner.TaxResult{IsToken2022: true, IsPausable: true, IsPaused: true}
		}, "token2022_paused"},
		{"hidden tax", func(f *preflightFacts) { f.Tax = &scanner.TaxResult{SimulationWorked: true, HiddenTaxPct: 40} }, "sell_simulation_hidden_tax"},
		{"honeypot flag", func(f *preflightFacts) { f.IsHoneypot = true }, "scanner_honeypot_flag"},
		{"lp pullable", func(f *preflightFacts) {
			f.LP = &scanner.LPLockResult{Method: "unlocked", UnlockedPercent: 99, LPMint: "LP1", Details: []scanner.LPHolder{{Address: "LPAcct", Owner: "LPOwner", Percent: 90, Type: "unlocked"}, {Address: "Burn", Percent: 1, Type: "burned"}}}
		}, "lp_unlocked_single_holder"},
	}
	for _, c := range cases {
		f := cleanFacts()
		c.mutate(&f)
		rs := expectVerdict(t, c.name, pfToken, f, pfWashOrganic, "REJECT", c.reason, effectReject)
		if rs[0].Effect != effectReject {
			t.Fatalf("%s: rejects must be listed first", c.name)
		}
	}
	// Honeypot already explained by the sell simulation is not listed twice.
	f := cleanFacts()
	f.IsHoneypot = true
	f.Tax = &scanner.TaxResult{SimulationWorked: true, HiddenTaxPct: 60}
	_, rs := EvaluatePreflight(pfToken, f, pfWashOrganic)
	if reasonByName(rs, "scanner_honeypot_flag") != nil {
		t.Fatal("duplicate honeypot reason")
	}
}

func TestPreflightSoftReviews(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*preflightFacts)
		reason string
	}{
		{"small transfer fee", func(f *preflightFacts) {
			f.Tax = &scanner.TaxResult{IsToken2022: true, HasTransferFee: true, TransferFeeBps: 300}
		}, "token2022_transfer_fee"},
		{"transfer hook", func(f *preflightFacts) {
			f.Tax = &scanner.TaxResult{IsToken2022: true, HasTransferHook: true, TransferHookProgram: "Hook"}
		}, "token2022_transfer_hook"},
		{"pausable", func(f *preflightFacts) { f.Tax = &scanner.TaxResult{IsToken2022: true, IsPausable: true} }, "token2022_pausable"},
		{"some hidden tax", func(f *preflightFacts) { f.Tax = &scanner.TaxResult{SimulationWorked: true, HiddenTaxPct: 15} }, "sell_simulation_tax"},
		{"lp unlocked but spread", func(f *preflightFacts) {
			f.LP = &scanner.LPLockResult{Method: "unlocked", UnlockedPercent: 99, Details: []scanner.LPHolder{{Percent: 40, Type: "unlocked"}, {Percent: 59, Type: "unlocked"}}}
		}, "lp_mostly_unlocked"},
		{"top holder", func(f *preflightFacts) { f.Holders.Top1HolderPct = 25 }, "concentrated_holders"},
		{"top 10", func(f *preflightFacts) { f.Holders.Top10HolderPct = 55 }, "concentrated_holders"},
		{"new", func(f *preflightFacts) { f.PairAgeHours = 3 }, "very_new_token"},
		{"low liquidity", func(f *preflightFacts) { f.LiquidityUSD = 3720 }, "low_liquidity"},
		{"authority unknown", func(f *preflightFacts) { f.Authority = nil }, "authority_not_assessed"},
	}
	for _, c := range cases {
		f := cleanFacts()
		c.mutate(&f)
		expectVerdict(t, c.name, pfToken, f, pfWashOrganic, "REVIEW", c.reason, effectReview)
	}
	// Pump.fun bonding-curve LP is not a removable LP position.
	f := cleanFacts()
	f.LP = &scanner.LPLockResult{Method: "unassessed", IsPumpFun: true, UnlockedPercent: 100}
	expectVerdict(t, "pump curve", pfToken, f, pfWashOrganic, "HOLD", "", "")
}

func TestPreflightResponseShape(t *testing.T) {
	f := cleanFacts()
	f.Authority = &scanner.AuthorityResult{MintAuthority: "MintAuth1"}
	f.ScanCostUnits = 12
	body := BuildPreflightResponse(pfToken, f, map[string]interface{}{"verdict": "organic", "wash_volume_pct": 2.0, "cost": map[string]interface{}{"budget_units": 150}, "as_of": "2026-11-16T10:00:00Z"})
	raw, _ := json.Marshal(body)
	var out map[string]interface{}
	_ = json.Unmarshal(raw, &out)
	for _, k := range []string{"subject", "chain", "source", "as_of", "verdict", "reasons", "checks", "summary", "rules_version", "cost_units", "method"} {
		if out[k] == nil {
			t.Fatalf("missing %s in %s", k, raw)
		}
	}
	if out["verdict"] != "REJECT" || out["source"] != "rugburn" || out["chain"] != "solana" || out["cost_units"].(float64) != 162 {
		t.Fatalf("body: %s", raw)
	}
	checks := out["checks"].(map[string]interface{})
	if checks["token_scan"].(map[string]interface{})["cost_units"].(float64) != 12 || checks["wash"].(map[string]interface{})["cost_units"].(float64) != 150 {
		t.Fatalf("checks: %v", checks)
	}
	// Cached wash answers cost nothing this request.
	body = BuildPreflightResponse(pfToken, f, map[string]interface{}{"verdict": "organic", "cached": true, "cost": map[string]interface{}{"budget_units": 150}})
	if body["checks"].(map[string]interface{})["wash"].(map[string]interface{})["cost_units"] != 0 {
		t.Fatal("cached wash should cost 0")
	}
	// No identity language anywhere in the rules' text.
	lower := strings.ToLower(string(raw))
	for _, w := range []string{"scammer", "operated by", "belongs to", "owned by"} {
		if strings.Contains(lower, w) {
			t.Fatalf("identity claim %q in %s", w, raw)
		}
	}
}

// --- handler ---

func installPreflightStubs(t *testing.T, st *washStubs) {
	t.Helper()
	installWashStubs(t, st)
	oldLatest, oldWash, oldLog := preflightLatestFacts, preflightWash, preflightLogUsage
	preflightLatestFacts = func(config.Config, string) (*preflightFacts, error) { return nil, nil }
	preflightLogUsage = func(config.Config, *washDevKey, *http.Request, int, time.Duration, string) {}
	preflightCache.Lock()
	preflightCache.facts = map[string]preflightFacts{}
	preflightCache.Unlock()
	t.Cleanup(func() {
		preflightLatestFacts, preflightWash, preflightLogUsage = oldLatest, oldWash, oldLog
		preflightCache.Lock()
		preflightCache.facts = map[string]preflightFacts{}
		preflightCache.Unlock()
	})
}

func doPreflight(t *testing.T, srv *Server, method, subject string, auth bool) (*httptest.ResponseRecorder, map[string]interface{}) {
	t.Helper()
	var req *http.Request
	if method == http.MethodPost {
		req = httptest.NewRequest(method, "/telegraph/preflight", strings.NewReader(`{"subject":"`+subject+`"}`))
	} else {
		req = httptest.NewRequest(method, "/telegraph/preflight?subject="+subject, nil)
	}
	if auth {
		req.Header.Set("X-Rugburn-API-Key", washTestKey)
	}
	rec := httptest.NewRecorder()
	srv.handleTelegraphPreflight(rec, req)
	var body map[string]interface{}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return rec, body
}

func TestTelegraphPreflightEndToEnd(t *testing.T) {
	swaps, clusters := washClusterSwaps()
	st := &washStubs{swaps: swaps, clusters: clusters}
	installPreflightStubs(t, st)
	washRunScan = func(cfg config.Config, mint string) (*scanner.TokenResult, error) {
		atomic.AddInt32(&st.scanCalls, 1)
		return &scanner.TokenResult{
			Address: mint, PoolAddress: "PoolFresh", LiquidityUSD: 50000,
			AuthorityStatus: &scanner.AuthorityResult{MintAuthority: "MintAuth1"},
			RawOnchainData:  map[string]interface{}{"launch_intelligence": map[string]interface{}{"age_known": true, "pair_age_hours": 400.0}},
		}, nil
	}
	rec, body := doPreflight(t, &Server{}, http.MethodGet, pfToken, true)
	if rec.Code != 200 || body["verdict"] != "REJECT" {
		t.Fatalf("code=%d body=%v", rec.Code, body)
	}
	// One scan serves both the rules and the wash check, and the wash check
	// read the scan's pool first.
	if st.scanCalls != 1 || st.fetchCalls != 1 || len(st.lastPlan.Sources) != 2 || st.lastPlan.Sources[0] != "PoolFresh" || st.lastPlan.Sources[1] != pfToken {
		t.Fatalf("scans=%d fetches=%d sources=%v", st.scanCalls, st.fetchCalls, st.lastPlan.Sources)
	}
	checks := body["checks"].(map[string]interface{})
	if checks["token_scan"].(map[string]interface{})["source"] != "fresh_scan" || checks["wash"].(map[string]interface{})["verdict"] != "wash_trading" {
		t.Fatalf("checks=%v", checks)
	}
	// Second call within 15 min: scan from memory, wash cached, nothing paid.
	_, body = doPreflight(t, &Server{}, http.MethodPost, pfToken, true)
	checks = body["checks"].(map[string]interface{})
	if st.scanCalls != 1 || st.fetchCalls != 1 || checks["token_scan"].(map[string]interface{})["source"] != "memory" || body["cost_units"].(float64) != 0 {
		t.Fatalf("repeat: scans=%d fetches=%d body=%v", st.scanCalls, st.fetchCalls, body)
	}
}

func TestTelegraphPreflightNoScanStillAnswers(t *testing.T) {
	st := &washStubs{scanErr: scanner.ErrScanTooFresh}
	installPreflightStubs(t, st)
	rec, body := doPreflight(t, &Server{}, http.MethodGet, pfToken, true)
	if rec.Code != 200 || body["verdict"] != "REVIEW" || rec.Body.Len() == 0 {
		t.Fatalf("code=%d body=%v", rec.Code, body)
	}
}

func TestTelegraphPreflightAuthAndSubject(t *testing.T) {
	installPreflightStubs(t, &washStubs{})
	rec, body := doPreflight(t, &Server{}, http.MethodGet, pfToken, false)
	if rec.Code != http.StatusUnauthorized || body["error"] != "unauthorized" {
		t.Fatalf("no key: %d %v", rec.Code, body)
	}
	rec, body = doPreflight(t, &Server{}, http.MethodGet, "0xdeadbeef", true)
	if rec.Code != http.StatusBadRequest || body["error"] != "unsupported_subject" {
		t.Fatalf("evm: %d %v", rec.Code, body)
	}
	washLookupKey = func(config.Config, string) (*washDevKey, error) {
		return &washDevKey{ID: "k2", Status: "active", Scopes: []string{"other:scope"}}, nil
	}
	washGlobal = &washState{}
	rec, _ = doPreflight(t, &Server{}, http.MethodGet, pfToken, true)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("wrong scope: %d", rec.Code)
	}
}

