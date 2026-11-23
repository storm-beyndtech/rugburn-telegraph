package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"rugburn-worker/internal/config"
	"rugburn-worker/internal/scanner"
)

const washTestMint = "So11111111111111111111111111111111111111112"
const washTestKey = "rb_live_test_key"

type washStubs struct {
	scanCalls  int32
	fetchCalls int32
	stored     *washScanContext
	scanErr    error
	swaps      []scanner.WashSwap
	clusters   map[string]string
	inFlight   bool
	fetchErr   error
	// funder lookups
	funderLookups int32
	resolvable    map[string]string
	lastPlan      scanner.WashFetchPlan
	persisted     map[string]interface{}
	persistAt     time.Time
	saves         int32
	// wallet activity checks
	activity       map[string]scanner.WashWalletActivity
	activityChecks int32
}

func installWashStubs(t *testing.T, st *washStubs) {
	t.Helper()
	washGlobal = &washState{}
	old := []interface{}{washLookupKey, washLatestScan, washRunScan, washScanInfo, washFetchSample, washFunderFacts, washBudgetAllowed, washRecordUsage, washLogUsage, washPersist, washPollPeriod, washLoadAnswer, washSaveAnswer, washNow}
	oldClusters, oldResolve, oldPersistFacts, oldSOLUSD, oldWalletActivity := washClustersFromFacts, washResolveFunder, washPersistFacts, washSOLUSD, washWalletActivity
	washWalletActivity = func(ctx context.Context, cfg config.Config, wallet, mint string) (scanner.WashWalletActivity, error) {
		atomic.AddInt32(&st.activityChecks, 1)
		return st.activity[wallet], nil
	}
	washClustersFromFacts = func(cfg config.Config, facts map[string]scanner.FundingFact) map[string]string {
		out := map[string]string{}
		for w, f := range facts {
			if f.Funder != "" {
				out[w] = f.Funder
			}
		}
		return out
	}
	washResolveFunder = func(ctx context.Context, cfg config.Config, wallet string) (scanner.FundingFact, error) {
		atomic.AddInt32(&st.funderLookups, 1)
		if f, ok := st.resolvable[wallet]; ok {
			return scanner.FundingFact{Funder: f, Signature: f}, nil
		}
		return scanner.FundingFact{}, nil
	}
	washPersistFacts = func(config.Config, string, map[string]scanner.FundingFact, []string) {}
	washSOLUSD = func(config.Config) float64 { return 0 }
	washLoadAnswer = func(config.Config, string) (map[string]interface{}, time.Time, error) {
		return st.persisted, st.persistAt, nil
	}
	washSaveAnswer = func(config.Config, string, map[string]interface{}, time.Time) { atomic.AddInt32(&st.saves, 1) }
	washLookupKey = func(cfg config.Config, hash string) (*washDevKey, error) {
		if len(hash) != 64 {
			t.Errorf("expected sha256 hex, got %d chars", len(hash))
		}
		return &washDevKey{ID: "key-1", Status: "active", Scopes: []string{"token:risk:read"}}, nil
	}
	washLatestScan = func(cfg config.Config, mint string) (*washScanContext, error) { return st.stored, nil }
	washRunScan = func(cfg config.Config, mint string) (*scanner.TokenResult, error) {
		atomic.AddInt32(&st.scanCalls, 1)
		if st.scanErr != nil {
			return nil, st.scanErr
		}
		return &scanner.TokenResult{Address: mint, PoolAddress: "PoolFresh", LiquidityUSD: 1000}, nil
	}
	washScanInfo = func(string) (time.Time, bool, time.Time) { return time.Time{}, st.inFlight, time.Time{} }
	washFetchSample = func(ctx context.Context, cfg config.Config, plan scanner.WashFetchPlan, mint string) (scanner.WashSample, error) {
		atomic.AddInt32(&st.fetchCalls, 1)
		st.lastPlan = plan
		if st.fetchErr != nil {
			return scanner.WashSample{}, st.fetchErr
		}
		if plan.OnPage != nil {
			plan.OnPage(plan.Sources[0], len(st.swaps), nil)
		}
		return scanner.WashSample{Swaps: st.swaps, Pages: 1}, nil
	}
	washFunderFacts = func(cfg config.Config, wallets []string) map[string]scanner.FundingFact {
		out := map[string]scanner.FundingFact{}
		for w, k := range st.clusters {
			out[w] = scanner.FundingFact{Funder: k, Signature: k}
		}
		return out
	}
	washBudgetAllowed = func(config.Config, int) (bool, string) { return true, "allowed" }
	washRecordUsage = func(config.Config, string, int, map[string]interface{}) {}
	washLogUsage = func(config.Config, *washDevKey, *http.Request, int, time.Duration, string) {}
	washPersist = func(*Server, *scanner.TokenResult) {}
	washPollPeriod = 5 * time.Millisecond
	t.Cleanup(func() {
		washLookupKey = old[0].(func(config.Config, string) (*washDevKey, error))
		washLatestScan = old[1].(func(config.Config, string) (*washScanContext, error))
		washRunScan = old[2].(func(config.Config, string) (*scanner.TokenResult, error))
		washScanInfo = old[3].(func(string) (time.Time, bool, time.Time))
		washFetchSample = old[4].(func(context.Context, config.Config, scanner.WashFetchPlan, string) (scanner.WashSample, error))
		washFunderFacts = old[5].(func(config.Config, []string) map[string]scanner.FundingFact)
		washClustersFromFacts, washResolveFunder, washPersistFacts, washSOLUSD, washWalletActivity = oldClusters, oldResolve, oldPersistFacts, oldSOLUSD, oldWalletActivity
		washBudgetAllowed = old[6].(func(config.Config, int) (bool, string))
		washRecordUsage = old[7].(func(config.Config, string, int, map[string]interface{}))
		washLogUsage = old[8].(func(config.Config, *washDevKey, *http.Request, int, time.Duration, string))
		washPersist = old[9].(func(*Server, *scanner.TokenResult))
		washPollPeriod = old[10].(time.Duration)
		washLoadAnswer = old[11].(func(config.Config, string) (map[string]interface{}, time.Time, error))
		washSaveAnswer = old[12].(func(config.Config, string, map[string]interface{}, time.Time))
		washNow = old[13].(func() time.Time)
		washGlobal = &washState{}
	})
}

func washSwapAt(i int, w, side string, sol, min float64) scanner.WashSwap {
	return scanner.WashSwap{Signature: fmt.Sprintf("s%d", i), Wallet: w, Side: side, SOL: sol, Time: time.Unix(1762000000, 0).Add(time.Duration(min * float64(time.Minute))), Slot: int64(i)}
}

func washClusterSwaps() ([]scanner.WashSwap, map[string]string) {
	s := []scanner.WashSwap{}
	i := 0
	for k := 0; k < 8; k++ {
		b := float64(k) * 6
		s = append(s, washSwapAt(i, "A", "buy", 1, b), washSwapAt(i+1, "B", "sell", 0.97, b+1), washSwapAt(i+2, "C", "buy", 1.02, b+2), washSwapAt(i+3, "A", "sell", 0.99, b+3))
		i += 4
	}
	for k := 0; k < 6; k++ {
		s = append(s, washSwapAt(100+k, fmt.Sprintf("O%d", k), "buy", 0.3+float64(k)*0.41, float64(k)*9))
	}
	return s, map[string]string{"A": "tx1", "B": "tx1", "C": "tx1"}
}

func washCleanSwaps() []scanner.WashSwap {
	s := []scanner.WashSwap{}
	for k := 0; k < 30; k++ {
		s = append(s, washSwapAt(k, fmt.Sprintf("W%d", k), "buy", 0.2+float64(k)*0.13, float64(k)*3))
	}
	return s
}

func doWash(t *testing.T, srv *Server, method, subject string, auth bool) (*httptest.ResponseRecorder, map[string]interface{}) {
	t.Helper()
	var req *http.Request
	if method == http.MethodPost {
		req = httptest.NewRequest(method, "/telegraph/wash", strings.NewReader(`{"subject":"`+subject+`"}`))
	} else {
		req = httptest.NewRequest(method, "/telegraph/wash?subject="+subject, nil)
	}
	if auth {
		req.Header.Set("Authorization", "Bearer "+washTestKey)
	}
	rec := httptest.NewRecorder()
	srv.handleTelegraphWash(rec, req)
	var body map[string]interface{}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return rec, body
}

func TestTelegraphWashCluster(t *testing.T) {
	swaps, clusters := washClusterSwaps()
	st := &washStubs{swaps: swaps, clusters: clusters, stored: &washScanContext{Source: "stored_scan", PoolAddress: "Pool1", ScannedAt: time.Now().Add(-2 * time.Minute)}}
	installWashStubs(t, st)
	rec, body := doWash(t, &Server{}, http.MethodGet, washTestMint, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if body["verdict"] != "wash_trading" {
		t.Fatalf("verdict %v; body %s", body["verdict"], rec.Body.String())
	}
	if pct, _ := body["wash_volume_pct"].(float64); pct < 70 || pct > 100 {
		t.Fatalf("pct %v", body["wash_volume_pct"])
	}
	if body["subject"] != washTestMint || body["summary"] == "" {
		t.Fatalf("missing fields: %v", body)
	}
	cats := map[string]bool{}
	for _, raw := range body["signals"].([]interface{}) {
		cats[raw.(map[string]interface{})["category"].(string)] = true
	}
	if !cats["observed"] || !cats["derived"] || !cats["hypothesis"] {
		t.Fatalf("signal categories %v", cats)
	}
	if st.scanCalls != 0 {
		t.Fatalf("fresh stored scan should skip rescanning")
	}
	// Second call is served from the result cache, no new fetch.
	_, body2 := doWash(t, &Server{}, http.MethodPost, washTestMint, true)
	if body2["cached"] != true || st.fetchCalls != 1 {
		t.Fatalf("expected cached answer, fetches=%d body=%v", st.fetchCalls, body2["cached"])
	}
}

func TestTelegraphWashClean(t *testing.T) {
	st := &washStubs{swaps: washCleanSwaps(), stored: &washScanContext{Source: "stored_scan", ScannedAt: time.Now()}}
	installWashStubs(t, st)
	rec, body := doWash(t, &Server{}, http.MethodGet, washTestMint, true)
	if rec.Code != 200 || body["verdict"] != "organic" || body["wash_volume_pct"].(float64) != 0 {
		t.Fatalf("got %d %s", rec.Code, rec.Body.String())
	}
}

func TestTelegraphWashInsufficient(t *testing.T) {
	st := &washStubs{swaps: washCleanSwaps()[:4], stored: &washScanContext{Source: "stored_scan", ScannedAt: time.Now()}}
	installWashStubs(t, st)
	rec, body := doWash(t, &Server{}, http.MethodGet, washTestMint, true)
	if rec.Code != 200 || body["verdict"] != "inconclusive" || body["wash_volume_pct"] != nil || body["inconclusive_reason"] == "" {
		t.Fatalf("got %d %s", rec.Code, rec.Body.String())
	}
}

func TestTelegraphWashRejectsEVM(t *testing.T) {
	installWashStubs(t, &washStubs{})
	for _, subj := range []string{"0x6982508145454Ce325dDbE47a25d4ec3d2311933", "not-a-mint", ""} {
		washGlobal = &washState{}
		rec, body := doWash(t, &Server{}, http.MethodGet, subj, true)
		if rec.Code != http.StatusBadRequest || !strings.Contains(fmt.Sprint(body["message"]), "Solana") {
			t.Fatalf("%q: got %d %s", subj, rec.Code, rec.Body.String())
		}
	}
}

func TestTelegraphWashTooFreshServesStored(t *testing.T) {
	// Stored scan is older than 15 min, the scanner says "too fresh" (another
	// replica scanned it): answer from storage, never an empty 202.
	st := &washStubs{swaps: washCleanSwaps(), scanErr: scanner.ErrScanTooFresh, stored: &washScanContext{Source: "stored_scan", PoolAddress: "PoolOld", ScannedAt: time.Now().Add(-40 * time.Minute)}}
	installWashStubs(t, st)
	rec, body := doWash(t, &Server{}, http.MethodGet, washTestMint, true)
	if rec.Code != 200 || body["verdict"] == nil || body["verdict"] == "" {
		t.Fatalf("got %d %s", rec.Code, rec.Body.String())
	}
	if st.scanCalls != 1 || st.fetchCalls != 1 {
		t.Fatalf("scan=%d fetch=%d", st.scanCalls, st.fetchCalls)
	}
}

func TestTelegraphWashInFlightWaitsThenAnswers(t *testing.T) {
	st := &washStubs{swaps: washCleanSwaps(), inFlight: true}
	installWashStubs(t, st)
	var flight atomic.Bool
	flight.Store(true)
	washScanInfo = func(string) (time.Time, bool, time.Time) { return time.Time{}, flight.Load(), time.Time{} }
	go func() { time.Sleep(30 * time.Millisecond); flight.Store(false) }()
	rec, body := doWash(t, &Server{}, http.MethodGet, washTestMint, true)
	if rec.Code != 200 || body["verdict"] != "organic" || st.scanCalls != 0 {
		t.Fatalf("got %d scan=%d %s", rec.Code, st.scanCalls, rec.Body.String())
	}
}

func TestTelegraphWashAuthAndRateLimit(t *testing.T) {
	st := &washStubs{swaps: washCleanSwaps(), stored: &washScanContext{Source: "stored_scan", ScannedAt: time.Now()}}
	installWashStubs(t, st)
	if rec, _ := doWash(t, &Server{}, http.MethodGet, washTestMint, false); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no key: %d", rec.Code)
	}
	washLookupKey = func(config.Config, string) (*washDevKey, error) {
		return &washDevKey{ID: "k2", Status: "active", Scopes: []string{"other"}}, nil
	}
	washGlobal = &washState{}
	if rec, _ := doWash(t, &Server{}, http.MethodGet, washTestMint, true); rec.Code != http.StatusForbidden {
		t.Fatalf("missing scope: %d", rec.Code)
	}
	exp := time.Now().Add(-time.Hour).Format(time.RFC3339)
	washLookupKey = func(config.Config, string) (*washDevKey, error) {
		return &washDevKey{ID: "k3", Status: "active", Scopes: []string{"*"}, ExpiresAt: &exp}, nil
	}
	washGlobal = &washState{}
	if rec, _ := doWash(t, &Server{}, http.MethodGet, washTestMint, true); rec.Code != http.StatusForbidden {
		t.Fatalf("expired: %d", rec.Code)
	}
	washLookupKey = func(config.Config, string) (*washDevKey, error) {
		return &washDevKey{ID: "k4", Status: "active", Scopes: []string{"*"}}, nil
	}
	washGlobal = &washState{}
	codes := []int{}
	for i := 0; i < 4; i++ {
		rec, _ := doWash(t, &Server{}, http.MethodGet, washTestMint, true)
		codes = append(codes, rec.Code)
	}
	if codes[0] != 200 || codes[3] != http.StatusTooManyRequests {
		t.Fatalf("rate limit codes %v", codes)
	}
}

// washClock makes washNow controllable without reassigning it mid-test.
type washClock struct{ ns atomic.Int64 }

func useWashClock(start time.Time) *washClock {
	c := &washClock{}
	c.ns.Store(start.UnixNano())
	washNow = func() time.Time { return time.Unix(0, c.ns.Load()) }
	return c
}

func (c *washClock) advance(d time.Duration) { c.ns.Add(int64(d)) }

func washSignalNames(body map[string]interface{}) map[string]map[string]interface{} {
	out := map[string]map[string]interface{}{}
	for _, raw := range body["signals"].([]interface{}) {
		m := raw.(map[string]interface{})
		out[m["name"].(string)] = m
	}
	return out
}

func TestTelegraphWashFreshTTLAvoidsRefetch(t *testing.T) {
	t.Setenv("TELEGRAPH_WASH_RPS_PER_KEY", "100") // the limiter runs on wall time
	swaps, clusters := washClusterSwaps()
	st := &washStubs{swaps: swaps, clusters: clusters, stored: &washScanContext{Source: "stored_scan", ScannedAt: time.Now().Add(100 * time.Hour)}}
	installWashStubs(t, st)
	clock := useWashClock(time.Now())
	doWash(t, &Server{}, http.MethodGet, washTestMint, true)
	for i := 0; i < 200 && atomic.LoadInt32(&st.saves) == 0; i++ {
		time.Sleep(5 * time.Millisecond)
	}
	if st.fetchCalls != 1 || atomic.LoadInt32(&st.saves) != 1 {
		t.Fatalf("first call: fetch=%d saves=%d", st.fetchCalls, st.saves)
	}
	clock.advance(25 * time.Minute)
	_, body := doWash(t, &Server{}, http.MethodGet, washTestMint, true)
	if body["cached"] != true || st.fetchCalls != 1 {
		t.Fatalf("25 min repeat should be cached: fetch=%d", st.fetchCalls)
	}
	clock.advance(10 * time.Minute) // 35 min: past the 30 min fresh window
	_, body = doWash(t, &Server{}, http.MethodGet, washTestMint, true)
	if body["cached"] == true || st.fetchCalls != 2 {
		t.Fatalf("35 min repeat should refetch: fetch=%d cached=%v", st.fetchCalls, body["cached"])
	}
}

func TestTelegraphWashBudgetSpentServesMemoryStale(t *testing.T) {
	swaps, clusters := washClusterSwaps()
	st := &washStubs{swaps: swaps, clusters: clusters, stored: &washScanContext{Source: "stored_scan", ScannedAt: time.Now().Add(100 * time.Hour)}}
	installWashStubs(t, st)
	clock := useWashClock(time.Now())
	_, first := doWash(t, &Server{}, http.MethodGet, washTestMint, true)
	clock.advance(3 * time.Hour)
	washBudgetAllowed = func(config.Config, int) (bool, string) { return false, "daily_limit" }
	rec, body := doWash(t, &Server{}, http.MethodGet, washTestMint, true)
	if rec.Code != 200 || body["stale"] != true || body["verdict"] != first["verdict"] || body["as_of"] != first["as_of"] {
		t.Fatalf("expected stale copy of first answer: %s", rec.Body.String())
	}
	sig := washSignalNames(body)["cached_answer"]
	if sig == nil || !strings.Contains(sig["note"].(string), "budget") || sig["evidence"].(map[string]interface{})["store"] != "memory" {
		t.Fatalf("cached_answer signal: %v", sig)
	}
	if st.fetchCalls != 1 {
		t.Fatalf("budget spent must not fetch: %d", st.fetchCalls)
	}
}

func TestTelegraphWashCapReachedServesPersisted(t *testing.T) {
	t.Setenv("TELEGRAPH_WASH_MAX_FETCHES_PER_DAY", "0")
	old := time.Now().Add(-72 * time.Hour)
	st := &washStubs{
		stored:    &washScanContext{Source: "stored_scan", ScannedAt: time.Now()},
		persisted: map[string]interface{}{"subject": washTestMint, "verdict": "likely_wash_trading", "wash_volume_pct": 22.5, "signals": []interface{}{map[string]interface{}{"name": "swap_sample", "category": "observed"}}, "summary": "x", "as_of": old.UTC().Format(time.RFC3339)},
		persistAt: old,
	}
	installWashStubs(t, st)
	rec, body := doWash(t, &Server{}, http.MethodGet, washTestMint, true)
	if rec.Code != 200 || body["stale"] != true || body["verdict"] != "likely_wash_trading" || body["wash_volume_pct"].(float64) != 22.5 || body["as_of"] != old.UTC().Format(time.RFC3339) {
		t.Fatalf("expected persisted stale answer: %s", rec.Body.String())
	}
	sig := washSignalNames(body)["cached_answer"]
	if sig == nil || sig["evidence"].(map[string]interface{})["store"] != "persisted" || st.fetchCalls != 0 {
		t.Fatalf("signal=%v fetch=%d", sig, st.fetchCalls)
	}
}

func TestTelegraphWashHeliusFailure(t *testing.T) {
	t.Setenv("TELEGRAPH_WASH_RPS_PER_KEY", "100")
	st := &washStubs{fetchErr: fmt.Errorf("helius 503"), stored: &washScanContext{Source: "stored_scan", ScannedAt: time.Now()}}
	installWashStubs(t, st)
	// No prior answer anywhere: inconclusive.
	_, body := doWash(t, &Server{}, http.MethodGet, washTestMint, true)
	if body["verdict"] != "inconclusive" || body["stale"] != nil || body["wash_volume_pct"] != nil {
		t.Fatalf("expected inconclusive: %v", body)
	}
	// A persisted answer exists: serve it stale. Failures are not cached, so
	// the next call retries the fetch.
	st.persisted = map[string]interface{}{"subject": washTestMint, "verdict": "organic", "wash_volume_pct": 1.0, "signals": []interface{}{}, "as_of": "2026-11-10T00:00:00Z"}
	st.persistAt = time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	_, body = doWash(t, &Server{}, http.MethodGet, washTestMint, true)
	if body["verdict"] != "organic" || body["stale"] != true || body["as_of"] != "2026-11-10T00:00:00Z" || st.fetchCalls != 2 {
		t.Fatalf("expected stale persisted answer: %v fetch=%d", body, st.fetchCalls)
	}
	if !strings.Contains(washSignalNames(body)["cached_answer"]["note"].(string), "Helius") {
		t.Fatalf("reason should name Helius")
	}
}

func TestTelegraphWashResolvesFundersForCrossWalletPairs(t *testing.T) {
	s := []scanner.WashSwap{}
	for k := 0; k < 8; k++ {
		b := float64(k) * 6
		s = append(s, washSwapAt(2*k, "X", "buy", 1, b), washSwapAt(2*k+1, "Y", "sell", 0.98, b+1))
	}
	for k := 0; k < 6; k++ {
		s = append(s, washSwapAt(100+k, fmt.Sprintf("O%d", k), "buy", 0.3+float64(k)*0.41, float64(k)*9))
	}
	st := &washStubs{swaps: s, clusters: map[string]string{}, resolvable: map[string]string{"X": "F1", "Y": "F1"},
		stored: &washScanContext{Source: "stored_scan", PoolAddress: "Pool1", ScannedAt: time.Now().Add(-2 * time.Minute)}}
	installWashStubs(t, st)
	_, body := doWash(t, &Server{}, http.MethodGet, washTestMint, true)
	// X/Y is the heavy pair; O2's 1.12 SOL buy one minute before a 0.98 SOL Y
	// sell also qualifies, so O2 is looked up too (Y already is).
	if st.funderLookups != 3 {
		t.Fatalf("expected 3 funder lookups, got %d", st.funderLookups)
	}
	// Shared-funder matches lift the share from 0 to roughly 58%, which is
	// likely_wash_trading under the 70/35 cutoffs.
	if body["verdict"] != "likely_wash_trading" {
		t.Fatalf("verdict=%v pct=%v", body["verdict"], body["wash_volume_pct"])
	}
	var shared map[string]interface{}
	for _, sig := range body["signals"].([]interface{}) {
		m := sig.(map[string]interface{})
		if m["name"] == "shared_funder_round_trips" {
			shared = m["evidence"].(map[string]interface{})
		}
	}
	if shared == nil || shared["matched_pairs"].(float64) < 7 {
		t.Fatalf("shared funder pairs: %v", shared)
	}
	cost := body["cost"].(map[string]interface{})
	if cost["funder_lookups"].(float64) != 3 || cost["helius_pages"].(float64) != 1 || cost["budget_units"].(float64) != 350 {
		t.Fatalf("cost=%v", cost)
	}
	if st.lastPlan.MaxPages != 3 || st.lastPlan.Sources[0] != "Pool1" {
		t.Fatalf("plan=%+v", st.lastPlan)
	}

	// Disabled by env: no paid lookups.
	t.Setenv("TELEGRAPH_WASH_FUNDER_LOOKUPS", "0")
	washGlobal = &washState{}
	st.funderLookups = 0
	_, _ = doWash(t, &Server{}, http.MethodGet, washTestMint, true)
	if st.funderLookups != 0 {
		t.Fatalf("lookups disabled but %d ran", st.funderLookups)
	}
}

func TestWashVerdictCutoffs(t *testing.T) {
	cases := []struct {
		pct  float64
		want string
	}{
		{0, "organic"}, {19.3, "organic"}, {34.99, "organic"},
		{35, "likely_wash_trading"}, {60, "likely_wash_trading"}, {69.99, "likely_wash_trading"},
		{70, "wash_trading"}, {100, "wash_trading"},
	}
	for _, c := range cases {
		p := c.pct
		if got := WashVerdict(scanner.WashMeasurement{Sufficient: true, WashVolumePct: &p}); got != c.want {
			t.Errorf("WashVerdict(%v) = %s, want %s", c.pct, got, c.want)
		}
	}
	p := 90.0
	if got := WashVerdict(scanner.WashMeasurement{Sufficient: false, WashVolumePct: &p}); got != "inconclusive" {
		t.Errorf("insufficient sample = %s, want inconclusive", got)
	}
}

// Two wallets flip similar clips within seconds (the deep-pool bot pattern).
func washBotSwaps() []scanner.WashSwap {
	s := []scanner.WashSwap{}
	i := 0
	for k := 0; k < 6; k++ {
		b := float64(k) * 0.3
		s = append(s, washSwapAt(i, "BOT1", "buy", 2, b), washSwapAt(i+1, "BOT1", "sell", 1.95, b+0.1))
		s = append(s, washSwapAt(i+2, "BOT2", "sell", 0.72, b+0.05), washSwapAt(i+3, "BOT2", "buy", 0.72, b+0.15))
		i += 4
	}
	for k := 0; k < 12; k++ {
		s = append(s, washSwapAt(100+k, fmt.Sprintf("U%d", k), "buy", 0.5+float64(k)*0.37, float64(k)*0.2))
	}
	return s
}

func TestTelegraphWashUnmatchesMultiMarketWallets(t *testing.T) {
	st := &washStubs{swaps: washBotSwaps(), clusters: map[string]string{}, activity: map[string]scanner.WashWalletActivity{"BOT1": {OtherMints: 31, TradingTxs: 100, OtherTxs: 56}, "BOT2": {OtherMints: 12, TradingTxs: 100, OtherTxs: 60}},
		stored: &washScanContext{Source: "stored_scan", PoolAddress: "Pool1", ScannedAt: time.Now().Add(-2 * time.Minute)}}
	installWashStubs(t, st)
	_, body := doWash(t, &Server{}, http.MethodGet, washTestMint, true)
	if st.activityChecks != 2 {
		t.Fatalf("expected 2 activity checks, got %d", st.activityChecks)
	}
	if body["verdict"] != "organic" || body["wash_volume_pct"].(float64) != 0 {
		t.Fatalf("verdict=%v pct=%v", body["verdict"], body["wash_volume_pct"])
	}
	sig := washSignalNames(body)["multi_market_wallets_unmatched"]
	if sig == nil {
		t.Fatalf("missing multi_market_wallets_unmatched signal")
	}
	ev := sig["evidence"].(map[string]interface{})
	if len(ev["wallets"].([]interface{})) != 2 || ev["unmatched_volume_sol"].(float64) <= 0 {
		t.Fatalf("evidence: %v", ev)
	}
	if body["cost"].(map[string]interface{})["activity_pages"].(float64) != 2 {
		t.Fatalf("cost: %v", body["cost"])
	}
}

func TestTelegraphWashKeepsSingleMarketWallets(t *testing.T) {
	// Same pattern, but the wallets trade little else: a volume bot. Matched.
	st := &washStubs{swaps: washBotSwaps(), clusters: map[string]string{}, activity: map[string]scanner.WashWalletActivity{"BOT1": {OtherMints: 1, TradingTxs: 100, OtherTxs: 1},
		// Many side mints, but 95 of 100 transactions are in the subject: no exemption.
		"BOT2": {OtherMints: 9, TradingTxs: 100, OtherTxs: 5}},
		stored: &washScanContext{Source: "stored_scan", PoolAddress: "Pool1", ScannedAt: time.Now().Add(-2 * time.Minute)}}
	installWashStubs(t, st)
	_, body := doWash(t, &Server{}, http.MethodGet, washTestMint, true)
	if body["verdict"] != "likely_wash_trading" && body["verdict"] != "wash_trading" {
		t.Fatalf("verdict=%v pct=%v", body["verdict"], body["wash_volume_pct"])
	}
	if washSignalNames(body)["multi_market_wallets_unmatched"] != nil {
		t.Fatalf("no wallet should be unmatched")
	}
}

func TestTelegraphWashSkipsActivityChecksWhenOrganic(t *testing.T) {
	s := []scanner.WashSwap{}
	for k := 0; k < 25; k++ {
		s = append(s, washSwapAt(k, fmt.Sprintf("U%d", k%9), "buy", 0.5+float64(k)*0.3, float64(k)))
	}
	st := &washStubs{swaps: s, clusters: map[string]string{}, activity: map[string]scanner.WashWalletActivity{},
		stored: &washScanContext{Source: "stored_scan", PoolAddress: "Pool1", ScannedAt: time.Now().Add(-2 * time.Minute)}}
	installWashStubs(t, st)
	_, body := doWash(t, &Server{}, http.MethodGet, washTestMint, true)
	if body["verdict"] != "organic" || st.activityChecks != 0 {
		t.Fatalf("verdict=%v checks=%d", body["verdict"], st.activityChecks)
	}
}

