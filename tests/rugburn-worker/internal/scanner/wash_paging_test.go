package scanner

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

const pagingMint = "Mint1111111111111111111111111111111111111111"

var pagingNow = time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)

// swapTx builds a SWAP-typed transaction where wallet buys or sells the
// subject against pool, paying/receiving quoteAmt of quoteMint ("" = native
// SOL moved to/from the pool). A 0.002 SOL tip to an unrelated account is
// always present, which is what the old all-native-transfers price picked up.
func swapTx(sig string, ts time.Time, wallet, side, quoteMint string, quoteAmt float64) enhancedTransaction {
	tx := enhancedTransaction{Signature: sig, Type: "SWAP", FeePayer: wallet, Timestamp: ts.Unix(), Slot: ts.Unix()}
	pool := "PoolVault"
	if side == "buy" {
		tx.TokenTransfers = append(tx.TokenTransfers, enhancedTokenTransfer{FromUserAccount: pool, ToUserAccount: wallet, Mint: pagingMint, TokenAmount: 1000})
		if quoteMint == "" {
			tx.NativeTransfers = append(tx.NativeTransfers, enhancedNativeTransfer{FromUserAccount: wallet, ToUserAccount: pool, Amount: quoteAmt * 1e9})
		} else {
			tx.TokenTransfers = append(tx.TokenTransfers, enhancedTokenTransfer{FromUserAccount: wallet, ToUserAccount: pool, Mint: quoteMint, TokenAmount: quoteAmt})
		}
	} else {
		tx.TokenTransfers = append(tx.TokenTransfers, enhancedTokenTransfer{FromUserAccount: wallet, ToUserAccount: pool, Mint: pagingMint, TokenAmount: 1000})
		if quoteMint == "" {
			tx.NativeTransfers = append(tx.NativeTransfers, enhancedNativeTransfer{FromUserAccount: pool, ToUserAccount: wallet, Amount: quoteAmt * 1e9})
		} else {
			tx.TokenTransfers = append(tx.TokenTransfers, enhancedTokenTransfer{FromUserAccount: pool, ToUserAccount: wallet, Mint: quoteMint, TokenAmount: quoteAmt})
		}
	}
	tx.NativeTransfers = append(tx.NativeTransfers, enhancedNativeTransfer{FromUserAccount: wallet, ToUserAccount: "JitoTip", Amount: 0.002 * 1e9})
	return tx
}

func TestWashQuoteSOLPricesOnlyTheTradersLeg(t *testing.T) {
	cases := []struct {
		name      string
		tx        enhancedTransaction
		solUSD    float64
		wantSOL   float64
		wantQuote string
	}{
		{"native SOL to pool, tip ignored", swapTx("a", pagingNow, "W", "buy", "", 1.5), 0, 1.5, "sol"},
		{"wSOL leg", swapTx("b", pagingNow, "W", "sell", washMintWSOL, 0.7), 0, 0.7, "sol"},
		{"USDC leg converted", swapTx("c", pagingNow, "W", "buy", washMintUSDC, 300), 150, 2, "usd"},
		{"USDC leg without a rate is unpriced, not the tip", swapTx("d", pagingNow, "W", "buy", washMintUSDC, 300), 0, 0, ""},
		{"unknown quote token is unpriced", swapTx("e", pagingNow, "W", "sell", "OtherMint", 5), 150, 0, ""},
	}
	for _, c := range cases {
		sw := ExtractWashSwapsPriced([]enhancedTransaction{c.tx}, pagingMint, c.solUSD)
		if len(sw) != 1 {
			t.Fatalf("%s: expected one swap, got %d", c.name, len(sw))
		}
		if d := sw[0].SOL - c.wantSOL; d > 1e-9 || d < -1e-9 || sw[0].Quote != c.wantQuote {
			t.Fatalf("%s: got %.6f %q want %.6f %q", c.name, sw[0].SOL, sw[0].Quote, c.wantSOL, c.wantQuote)
		}
	}
}

func TestWashQuoteSOLSwapEvent(t *testing.T) {
	tx := enhancedTransaction{Signature: "ev", Type: "SWAP", FeePayer: "W", Timestamp: pagingNow.Unix()}
	tx.Events.Swap = &enhancedSwapEvent{
		TokenInputs:  []enhancedSwapToken{{UserAccount: "W", Mint: washMintUSDT, RawTokenAmount: enhancedRawTokenAmount{TokenAmount: "75000000", Decimals: 6}}},
		TokenOutputs: []enhancedSwapToken{{UserAccount: "W", Mint: pagingMint, TokenAmount: 10}},
	}
	sw := ExtractWashSwapsPriced([]enhancedTransaction{tx}, pagingMint, 150)
	if len(sw) != 1 || sw[0].Side != "buy" || sw[0].Quote != "usd" || sw[0].SOL != 0.5 {
		t.Fatalf("got %+v", sw)
	}
}

// fakePages serves a per-address history newest-first in pages of
// WashPageSize, honouring before and since, and records calls.
type fakePages struct {
	hist  map[string][]enhancedTransaction
	calls []string
	fail  map[string]bool
}

func (f *fakePages) fetch(ctx context.Context, address, before string, since time.Time) ([]enhancedTransaction, error) {
	f.calls = append(f.calls, address+"@"+before)
	if f.fail[address] {
		return nil, errors.New("helius 503")
	}
	h := f.hist[address]
	start := 0
	if before != "" {
		for i, tx := range h {
			if tx.Signature == before {
				start = i + 1
				break
			}
		}
	}
	out := []enhancedTransaction{}
	for i := start; i < len(h) && len(out) < WashPageSize; i++ {
		if !since.IsZero() && h[i].Timestamp < since.Unix() {
			break
		}
		out = append(out, h[i])
	}
	return out, nil
}

// history builds n swaps newest-first, one every step, alternating wallets.
func history(prefix string, n int, step time.Duration) []enhancedTransaction {
	out := make([]enhancedTransaction, 0, n)
	for i := 0; i < n; i++ {
		side := "buy"
		if i%2 == 1 {
			side = "sell"
		}
		out = append(out, swapTx(fmt.Sprintf("%s%04d", prefix, i), pagingNow.Add(-time.Duration(i)*step), fmt.Sprintf("W%d", i%37), side, "", 0.1+float64(i%7)*0.05))
	}
	return out
}

func TestCollectWashSwapsPagesBackWithinLimit(t *testing.T) {
	f := &fakePages{hist: map[string][]enhancedTransaction{"Pool": history("p", 1000, 30*time.Second)}}
	plan := WashFetchPlan{Sources: []string{"Pool"}, MaxPages: 3}
	s, err := CollectWashSwaps(context.Background(), plan, pagingMint, f.fetch)
	if err != nil {
		t.Fatal(err)
	}
	if s.Pages != 3 || len(f.calls) != 3 || len(s.Swaps) != 300 || s.StopReason != "page_limit" {
		t.Fatalf("pages=%d calls=%v swaps=%d stop=%s", s.Pages, f.calls, len(s.Swaps), s.StopReason)
	}
	// Each later page continues from the previous page's oldest signature.
	if f.calls[1] != "Pool@p0099" || f.calls[2] != "Pool@p0199" {
		t.Fatalf("before-signature chain wrong: %v", f.calls)
	}
}

func TestCollectWashSwapsStopsWhenHistoryEnds(t *testing.T) {
	f := &fakePages{hist: map[string][]enhancedTransaction{"Pool": history("p", 140, time.Minute)}}
	s, _ := CollectWashSwaps(context.Background(), WashFetchPlan{Sources: []string{"Pool"}, MaxPages: 3}, pagingMint, f.fetch)
	if s.Pages != 2 || len(s.Swaps) != 140 || !s.Sources[0].Exhausted || s.StopReason != "history_exhausted" || !s.Cutoff.IsZero() {
		t.Fatalf("pages=%d swaps=%d src=%+v stop=%s cutoff=%v", s.Pages, len(s.Swaps), s.Sources[0], s.StopReason, s.Cutoff)
	}
}

func TestCollectWashSwapsStopsAtLookback(t *testing.T) {
	f := &fakePages{hist: map[string][]enhancedTransaction{"Pool": history("p", 1000, 10*time.Minute)}}
	plan := WashFetchPlan{Sources: []string{"Pool"}, MaxPages: 5, Since: pagingNow.Add(-24 * time.Hour)}
	s, _ := CollectWashSwaps(context.Background(), plan, pagingMint, f.fetch)
	// 24 h at one swap per 10 min is 145 transactions: two pages, then done.
	if s.Pages != 2 || len(s.Swaps) != 145 || s.StopReason != "history_exhausted" {
		t.Fatalf("pages=%d swaps=%d stop=%s", s.Pages, len(s.Swaps), s.StopReason)
	}
}

func TestCollectWashSwapsTrimsToCommonWindowAndDedups(t *testing.T) {
	// The pool's history ends after 60 transactions (thin or moved pool). The
	// mint feed is sparser, repeats the pool's first 20 signatures, and is still
	// going when the page limit hits.
	pool := history("p", 60, time.Minute)
	mint := append([]enhancedTransaction{}, pool[:20]...)
	for i := 0; i < 500; i++ {
		mint = append(mint, swapTx(fmt.Sprintf("m%04d", i), pagingNow.Add(-20*time.Minute-time.Duration(i)*20*time.Second), "Other", "buy", "", 1))
	}
	f := &fakePages{hist: map[string][]enhancedTransaction{"Pool": pool, pagingMint: mint}}
	s, err := CollectWashSwaps(context.Background(), WashFetchPlan{Sources: []string{"Pool", pagingMint, "Pool"}, MaxPages: 3}, pagingMint, f.fetch)
	if err != nil {
		t.Fatal(err)
	}
	// Sources are read in order and deduplicated: Pool (ends), then the mint.
	if len(f.calls) != 3 || f.calls[0] != "Pool@" || f.calls[1] != pagingMint+"@" || f.calls[2] != pagingMint+"@m0079" {
		t.Fatalf("calls=%v", f.calls)
	}
	if !s.Sources[0].Exhausted || s.Sources[1].Exhausted || len(s.Sources) != 2 {
		t.Fatalf("sources=%+v", s.Sources)
	}
	// The mint feed reaches back 20 min + 179×20 s; pool swaps older than that
	// fall outside the window the mint feed covers and are trimmed.
	mintOldest := pagingNow.Add(-20*time.Minute - 179*20*time.Second)
	if !s.Cutoff.Equal(mintOldest) {
		t.Fatalf("cutoff=%v want %v", s.Cutoff, mintOldest)
	}
	for _, sw := range s.Swaps {
		if sw.Time.Before(s.Cutoff) {
			t.Fatalf("swap %s before cutoff", sw.Signature)
		}
	}
	if s.Sources[1].Swaps != 180 {
		t.Fatalf("mint swaps after dedup = %d, want 180", s.Sources[1].Swaps)
	}
	if len(s.Swaps) != 60+180-s.TrimmedSwaps {
		t.Fatalf("swaps=%d trimmed=%d", len(s.Swaps), s.TrimmedSwaps)
	}
}

func TestCollectWashSwapsBudgetAndErrors(t *testing.T) {
	f := &fakePages{hist: map[string][]enhancedTransaction{pagingMint: history("m", 1000, time.Minute)}, fail: map[string]bool{"Pool": true}}
	allowed := 2
	pages := 0
	plan := WashFetchPlan{
		Sources:   []string{"Pool", pagingMint},
		MaxPages:  5,
		AllowPage: func(string) bool { allowed--; return allowed >= 0 },
		OnPage:    func(string, int, error) { pages++ },
	}
	s, err := CollectWashSwaps(context.Background(), plan, pagingMint, f.fetch)
	if err != nil {
		t.Fatal(err)
	}
	// Pool fails (billed, then skipped), mint p1 ok, budget stops the third.
	if s.Pages != 1 || s.FailedPages != 1 || pages != 2 || s.StopReason != "budget" || s.Sources[0].Error == "" || s.Sources[1].Pages != 1 {
		t.Fatalf("pages=%d failed=%d billed=%d stop=%s src=%+v", s.Pages, s.FailedPages, pages, s.StopReason, s.Sources)
	}

	f.fail[pagingMint] = true
	_, err = CollectWashSwaps(context.Background(), WashFetchPlan{Sources: []string{"Pool", pagingMint}, MaxPages: 3}, pagingMint, f.fetch)
	if err == nil {
		t.Fatal("all pages failed: expected an error so the route serves the stale answer")
	}
}

func TestCollectWashSwapsRespectsTimeAndTarget(t *testing.T) {
	f := &fakePages{hist: map[string][]enhancedTransaction{"Pool": history("p", 1000, time.Minute)}}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	s, _ := CollectWashSwaps(ctx, WashFetchPlan{Sources: []string{"Pool"}, MaxPages: 3, MinRemaining: 2 * time.Second}, pagingMint, f.fetch)
	if s.Pages != 0 || s.StopReason != "time_limit" {
		t.Fatalf("pages=%d stop=%s", s.Pages, s.StopReason)
	}
	s, _ = CollectWashSwaps(context.Background(), WashFetchPlan{Sources: []string{"Pool"}, MaxPages: 3, TargetSwaps: 150}, pagingMint, f.fetch)
	if s.Pages != 2 || s.StopReason != "target_reached" {
		t.Fatalf("pages=%d stop=%s", s.Pages, s.StopReason)
	}
}

func TestWashFunderCandidatesPairsOnly(t *testing.T) {
	swaps := []WashSwap{
		washSwap(1, "A", "buy", 1.0, 0),
		washSwap(2, "B", "sell", 0.98, 2), // A/B cross pair, 1.98 SOL
		washSwap(3, "C", "buy", 0.2, 5),
		washSwap(4, "D", "sell", 0.21, 6), // C/D cross pair, 0.41 SOL
		washSwap(5, "E", "buy", 3, 30),
		washSwap(6, "E", "sell", 3, 31), // self round trip: no lookup needed
		washSwap(7, "F", "buy", 5, 60),
		washSwap(8, "G", "sell", 0.1, 61), // sizes differ: not a pair
	}
	got := WashFunderCandidates(swaps, map[string]bool{}, DefaultWashParams(), 3)
	if len(got) != 2 || got[0] != "A" || got[1] != "B" {
		t.Fatalf("limit 3 must not split the C/D pair: %v", got)
	}
	got = WashFunderCandidates(swaps, map[string]bool{"A": true}, DefaultWashParams(), 4)
	if fmt.Sprint(got) != "[B C D]" {
		t.Fatalf("known A needs only B: %v", got)
	}
	if got := WashFunderCandidates(swaps, nil, DefaultWashParams(), 0); got != nil {
		t.Fatalf("limit 0 disables: %v", got)
	}
}

