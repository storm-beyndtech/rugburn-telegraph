package scanner

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

var washT0 = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

func washSwap(i int, wallet, side string, sol float64, minute float64) WashSwap {
	return WashSwap{Signature: fmt.Sprintf("sig%03d", i), Wallet: wallet, Side: side, SOL: sol, Time: washT0.Add(time.Duration(minute * float64(time.Minute))), Slot: int64(i)}
}

// WashClusterFixture: three wallets sharing one funding tx cycle ~1 SOL back and
// forth, and one wallet round-trips on its own, next to ordinary one-way buyers.
func WashClusterFixture() ([]WashSwap, map[string]string) {
	s := []WashSwap{}
	i := 0
	add := func(w, side string, sol, min float64) { s = append(s, washSwap(i, w, side, sol, min)); i++ }
	for k := 0; k < 8; k++ {
		base := float64(k) * 6
		add("ClusterA", "buy", 1.00, base)
		add("ClusterB", "sell", 0.97, base+1)
		add("ClusterC", "buy", 1.02, base+2)
		add("ClusterA", "sell", 0.99, base+3)
	}
	add("Solo", "buy", 2.0, 50)
	add("Solo", "sell", 1.9, 52)
	for k := 0; k < 6; k++ {
		add(fmt.Sprintf("Organic%d", k), "buy", 0.3+float64(k)*0.41, float64(k)*9)
	}
	clusters := map[string]string{"ClusterA": "fundtx1", "ClusterB": "fundtx1", "ClusterC": "fundtx1", "Organic0": "binanceish"}
	return s, clusters
}

func CleanFixture() ([]WashSwap, map[string]string) {
	s := []WashSwap{}
	for k := 0; k < 30; k++ {
		s = append(s, washSwap(k, fmt.Sprintf("W%02d", k), "buy", 0.2+float64(k)*0.13, float64(k)*3))
	}
	// A few holders sell much later and at different sizes.
	for k := 0; k < 5; k++ {
		s = append(s, washSwap(100+k, fmt.Sprintf("W%02d", k), "sell", 3.0+float64(k), 200+float64(k)))
	}
	return s, map[string]string{"W01": "f1", "W02": "f2"}
}

func TestMeasureWashCluster(t *testing.T) {
	swaps, clusters := WashClusterFixture()
	m := MeasureWash(swaps, clusters, DefaultWashParams())
	if !m.Sufficient || m.WashVolumePct == nil {
		t.Fatalf("expected sufficient sample, got %+v", m)
	}
	if m.Clusters != 1 || m.LargestCluster != 3 {
		t.Fatalf("clusters=%d largest=%d", m.Clusters, m.LargestCluster)
	}
	if m.SelfPairs < 1 || m.ClusterPairs < 8 {
		t.Fatalf("self=%d cluster=%d", m.SelfPairs, m.ClusterPairs)
	}
	if *m.WashVolumePct < 70 {
		t.Fatalf("wash pct too low: %v", *m.WashVolumePct)
	}
	if len(m.TopEntities) == 0 || m.TopEntities[0].Kind != "funder_cluster" || m.TopEntities[0].Key != "fundtx1" {
		t.Fatalf("top entity: %+v", m.TopEntities)
	}
}

func TestMeasureWashClean(t *testing.T) {
	swaps, clusters := CleanFixture()
	m := MeasureWash(swaps, clusters, DefaultWashParams())
	if !m.Sufficient || m.WashVolumePct == nil {
		t.Fatalf("expected sufficient: %+v", m)
	}
	if *m.WashVolumePct != 0 {
		t.Fatalf("clean token should have 0 matched volume, got %v", *m.WashVolumePct)
	}
}

func TestMeasureWashInsufficient(t *testing.T) {
	swaps := []WashSwap{washSwap(1, "A", "buy", 1, 0), washSwap(2, "A", "sell", 1, 1), washSwap(3, "B", "buy", 1, 2)}
	m := MeasureWash(swaps, nil, DefaultWashParams())
	if m.Sufficient || m.WashVolumePct != nil || m.InsufficientWhy == "" {
		t.Fatalf("expected insufficient: %+v", m)
	}
	// Few wallets also blocks a share even with enough swaps.
	many := []WashSwap{}
	for k := 0; k < 30; k++ {
		many = append(many, washSwap(k, fmt.Sprintf("X%d", k%3), "buy", 1+float64(k), float64(k)))
	}
	if m := MeasureWash(many, nil, DefaultWashParams()); m.Sufficient {
		t.Fatalf("3 wallets should be insufficient")
	}
}

func TestMeasureWashRespectsGapAndSize(t *testing.T) {
	p := DefaultWashParams()
	swaps := []WashSwap{
		washSwap(1, "A", "buy", 1, 0),
		washSwap(2, "A", "sell", 1, 30), // too late
		washSwap(3, "B", "buy", 1, 0),
		washSwap(4, "B", "sell", 3, 1), // size mismatch
	}
	p.MinSwaps, p.MinWallets = 1, 1
	m := MeasureWash(swaps, nil, p)
	if m.SelfPairs != 0 || *m.WashVolumePct != 0 {
		t.Fatalf("expected no matches: %+v", m)
	}
	// Unpriced swaps are excluded from both sides.
	swaps = append(swaps, WashSwap{Signature: "u", Wallet: "C", Side: "buy", Time: washT0})
	if m := MeasureWash(swaps, nil, p); m.UnpricedSwaps != 1 || m.PricedSwaps != 4 {
		t.Fatalf("unpriced handling: %+v", m)
	}
}

func TestExtractWashSwaps(t *testing.T) {
	mint := "MintAddr1111111111111111111111111111111111"
	raw := `[
	 {"signature":"s1","type":"SWAP","feePayer":"Alice","slot":10,"timestamp":1762000000,
	  "tokenTransfers":[{"fromUserAccount":"Pool","toUserAccount":"Alice","tokenAmount":100,"mint":"` + mint + `"}],
	  "nativeTransfers":[{"fromUserAccount":"Alice","toUserAccount":"Pool","amount":1500000000}]},
	 {"signature":"s2","type":"SWAP","feePayer":"Alice","slot":11,"timestamp":1762000060,
	  "tokenTransfers":[{"fromUserAccount":"Alice","toUserAccount":"Pool","tokenAmount":100,"mint":"` + mint + `"}],
	  "nativeTransfers":[{"fromUserAccount":"Pool","toUserAccount":"Alice","amount":1400000000}]},
	 {"signature":"s2","type":"SWAP","feePayer":"Alice","slot":11,"timestamp":1762000060},
	 {"signature":"s3","type":"SWAP","feePayer":"Bob","slot":12,"timestamp":1762000070,"transactionError":{"x":1},
	  "tokenTransfers":[{"fromUserAccount":"Pool","toUserAccount":"Bob","tokenAmount":5,"mint":"` + mint + `"}]},
	 {"signature":"s4","type":"TRANSFER","feePayer":"Carol","slot":13,"timestamp":1762000080}
	]`
	var txs []enhancedTransaction
	if err := json.Unmarshal([]byte(raw), &txs); err != nil {
		t.Fatal(err)
	}
	got := ExtractWashSwaps(txs, mint)
	if len(got) != 2 {
		t.Fatalf("want 2 swaps, got %+v", got)
	}
	if got[0].Side != "buy" || got[0].Wallet != "Alice" || got[0].SOL != 1.5 {
		t.Fatalf("first swap: %+v", got[0])
	}
	if got[1].Side != "sell" || got[1].SOL != 1.4 {
		t.Fatalf("second swap: %+v", got[1])
	}
}

func TestMeasureWashUnmatchedWalletsKeepVolume(t *testing.T) {
	t0 := time.Unix(1762000000, 0)
	sw := []WashSwap{}
	for k := 0; k < 10; k++ {
		at := t0.Add(time.Duration(k) * time.Minute)
		sw = append(sw, WashSwap{Signature: fmt.Sprintf("a%d", k), Wallet: "BOT", Side: "buy", SOL: 1, Time: at},
			WashSwap{Signature: fmt.Sprintf("b%d", k), Wallet: "BOT", Side: "sell", SOL: 1, Time: at.Add(10 * time.Second)})
		sw = append(sw, WashSwap{Signature: fmt.Sprintf("u%d", k), Wallet: fmt.Sprintf("U%d", k), Side: "buy", SOL: 1, Time: at})
	}
	p := DefaultWashParams()
	before := MeasureWash(sw, nil, p)
	if before.WashVolumePct == nil || *before.WashVolumePct < 60 {
		t.Fatalf("baseline pct %v", before.WashVolumePct)
	}
	p.Unmatched = map[string]bool{"BOT": true}
	after := MeasureWash(sw, nil, p)
	if after.WashVolumePct == nil || *after.WashVolumePct != 0 || after.TotalVolume != before.TotalVolume {
		t.Fatalf("after pct %v total %v vs %v", after.WashVolumePct, after.TotalVolume, before.TotalVolume)
	}
	if after.UnmatchedWallets != 1 || after.UnmatchedSOL != 20 {
		t.Fatalf("unmatched %d %.2f", after.UnmatchedWallets, after.UnmatchedSOL)
	}
}

func TestWashMultiMarketCandidates(t *testing.T) {
	m := WashMeasurement{TotalVolume: 100, TopEntities: []WashEntity{
		{Kind: "wallet", Key: "A", MatchedSOL: 30},
		{Kind: "funder_cluster", Key: "F", MatchedSOL: 20},
		{Kind: "wallet", Key: "B", MatchedSOL: 10},
		{Kind: "wallet", Key: "C", MatchedSOL: 4},
		{Kind: "wallet", Key: "D", MatchedSOL: 3},
	}}
	got := WashMultiMarketCandidates(m, 3, 0.05)
	if len(got) != 2 || got[0] != "A" || got[1] != "B" {
		t.Fatalf("got %v", got)
	}
	if got := WashMultiMarketCandidates(m, 1, 0.05); len(got) != 1 || got[0] != "A" {
		t.Fatalf("limit: %v", got)
	}
}

func TestWashWalletActivity(t *testing.T) {
	tr := func(mint, from, to string) enhancedTokenTransfer {
		return enhancedTokenTransfer{Mint: mint, FromUserAccount: from, ToUserAccount: to, TokenAmount: 1}
	}
	txs := []enhancedTransaction{
		{TokenTransfers: []enhancedTokenTransfer{tr("SUBJ", "W", "POOL"), tr(washMintWSOL, "POOL", "W")}},
		{TokenTransfers: []enhancedTokenTransfer{tr("M1", "W", "POOL"), tr(washMintUSDC, "POOL", "W")}},
		{TokenTransfers: []enhancedTokenTransfer{tr("M2", "POOL", "W"), tr("M3", "POOLX", "POOLY")}},
		{TokenTransfers: []enhancedTokenTransfer{tr("M1", "POOL", "W"), tr(washMintUSDT, "W", "POOL")}},
		{TokenTransfers: []enhancedTokenTransfer{tr("M9", "A", "B")}},
	}
	a := WashWalletActivityOf(txs, "W", "SUBJ")
	if a.OtherMints != 2 || a.TradingTxs != 4 || a.OtherTxs != 3 {
		t.Fatalf("got %+v, want 2 other mints (route-only M3 and quotes ignored), 4 trading, 3 other", a)
	}
	cases := []struct {
		a    WashWalletActivity
		want bool
	}{
		{WashWalletActivity{OtherMints: 12, TradingTxs: 100, OtherTxs: 56}, true},
		{WashWalletActivity{OtherMints: 5, TradingTxs: 100, OtherTxs: 50}, true},
		{WashWalletActivity{OtherMints: 13, TradingTxs: 100, OtherTxs: 48}, true},
		{WashWalletActivity{OtherMints: 13, TradingTxs: 100, OtherTxs: 31}, true},
		{WashWalletActivity{OtherMints: 12, TradingTxs: 100, OtherTxs: 25}, true},
		{WashWalletActivity{OtherMints: 12, TradingTxs: 100, OtherTxs: 20}, false},
		{WashWalletActivity{OtherMints: 12, TradingTxs: 100, OtherTxs: 1}, false}, // side trades only
		{WashWalletActivity{OtherMints: 4, TradingTxs: 100, OtherTxs: 90}, false},
		{WashWalletActivity{}, false},
	}
	for _, c := range cases {
		if got := c.a.MultiMarket(); got != c.want {
			t.Errorf("%+v MultiMarket=%v want %v", c.a, got, c.want)
		}
	}
}

func TestExtractWashSwapsSkipsRouteHops(t *testing.T) {
	tr := func(mint, from, to string, amt float64) enhancedTokenTransfer {
		return enhancedTokenTransfer{Mint: mint, FromUserAccount: from, ToUserAccount: to, TokenAmount: amt}
	}
	// Arbitrage loop: SOL -> LST on pool A, LST -> USDC on pool B, USDC -> SOL.
	// Helius summarises it as "LST in, USDC out", as seen on mSOL/SOL.
	loop := enhancedTransaction{Signature: "loop", Type: "SWAP", FeePayer: "BOT", Timestamp: 1762000000,
		Events: enhancedEvents{Swap: &enhancedSwapEvent{
			TokenInputs:  []enhancedSwapToken{{UserAccount: "BOT", Mint: "LST", TokenAmount: 0.1925}},
			TokenOutputs: []enhancedSwapToken{{UserAccount: "POOLB", Mint: washMintUSDC, TokenAmount: 18.21}},
		}},
		TokenTransfers: []enhancedTokenTransfer{
			tr(washMintWSOL, "BOT", "POOLA", 0.2728), tr("LST", "POOLA", "BOT", 0.1925),
			tr("LST", "BOT", "POOLB", 0.1925), tr(washMintUSDC, "POOLB", "BOT", 18.21),
			tr(washMintUSDC, "BOT", "POOLC", 18.21), tr(washMintWSOL, "POOLC", "BOT", 0.2729),
		}}
	// Plain buy of the LST with wSOL.
	buy := enhancedTransaction{Signature: "buy", Type: "SWAP", FeePayer: "U", Timestamp: 1762000001,
		TokenTransfers: []enhancedTokenTransfer{tr(washMintWSOL, "U", "POOLA", 1), tr("LST", "POOLA", "U", 0.75)}}
	swaps, hops := extractWashSwaps([]enhancedTransaction{loop, buy}, "LST", 0)
	if hops != 1 || len(swaps) != 1 || swaps[0].Signature != "buy" || swaps[0].Side != "buy" {
		t.Fatalf("hops=%d swaps=%+v", hops, swaps)
	}
}

