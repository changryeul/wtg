//go:build integration

package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/winwaysystems/wtg/pkg/pricing"
	"github.com/winwaysystems/wtg/pkg/session"
	"github.com/winwaysystems/wtg/test/pgxtest"
)

// 기본 시나리오 — 24 봉 (1일치 1m, 실제 dev seed 처럼) seed + 재계산 호출 →
// 각 봉의 마진 / 통계 / 샘플 응답 검증.
func TestAdmin_MarginRecompute_Override(t *testing.T) {
	pool := pgxtest.StartTimescale(t)

	// quote_bars seed — 1시간치 1m 봉, USD/KRW.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	now := time.Now().UTC().Truncate(time.Minute)
	for i := 0; i < 60; i++ {
		opened := now.Add(-time.Duration(60-i) * time.Minute)
		closed := opened.Add(time.Minute)
		raw := 1400.00 + float64(i)*0.01
		_, err := pool.Exec(ctx, `
			INSERT INTO quote_bars (pair, tf, opened_at, closed_at,
				open_bid, open_ask, high_bid, high_ask, low_bid, low_ask,
				close_bid, close_ask, tick_count)
			VALUES ($1, '1m', $2, $3, $4, $5, $4, $5, $4, $5, $4, $5, 20)
		`, "USD/KRW", opened, closed, raw, raw+0.05)
		if err != nil {
			t.Fatalf("seed bar %d: %v", i, err)
		}
	}

	deps := &MarginRecomputeDeps{
		Pool:    pool,
		EtcdKey: "wtg/pricing/table",
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/admin/margin/recompute", PostMarginRecompute(deps))
	ts := httptest.NewServer(mux)
	defer ts.Close()

	// 가상 PricingTable — bid -0.10, ask +0.10 균등 마진.
	override := pricing.PricingTableDoc{
		Version: 99,
		HQMargin: []pricing.HQEntryDoc{
			{Pair: "USD/KRW", Tier: session.TierVIP, BidAmount: 0.10, AskAmount: 0.10},
		},
	}
	req := marginRecomputeRequest{
		From:          now.Add(-time.Hour),
		To:            now,
		Pair:          "USD/KRW",
		Profile:       session.Profile{Channel: session.ChannelWeb, Site: session.SiteHQ, Tier: session.TierVIP},
		TableOverride: &override,
		SampleLimit:   5,
	}
	body, _ := json.Marshal(req)
	resp, err := http.Post(ts.URL+"/v1/admin/margin/recompute",
		"application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, b)
	}
	var out marginRecomputeResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.BarsProcessed != 60 {
		t.Errorf("bars=%d, want 60", out.BarsProcessed)
	}
	if out.TableSource != "override" {
		t.Errorf("source=%q, want override", out.TableSource)
	}
	if out.TableVersion != 99 {
		t.Errorf("version=%d, want 99", out.TableVersion)
	}
	if len(out.Samples) != 5 {
		t.Errorf("samples=%d, want 5", len(out.Samples))
	}
	// 마진 적용 검증 — bid -0.10, ask +0.10.
	for i, s := range out.Samples {
		if s.BidMargin > 0 || s.BidMargin < -0.15 {
			t.Errorf("[%d] BidMargin=%v, want ~-0.10", i, s.BidMargin)
		}
		if s.AskMargin < 0 || s.AskMargin > 0.15 {
			t.Errorf("[%d] AskMargin=%v, want ~+0.10", i, s.AskMargin)
		}
		if s.CustomerBid >= s.RawBid {
			t.Errorf("[%d] CustomerBid (%v) > RawBid (%v) — bid 는 더 낮아야",
				i, s.CustomerBid, s.RawBid)
		}
		if s.CustomerAsk <= s.RawAsk {
			t.Errorf("[%d] CustomerAsk (%v) < RawAsk (%v) — ask 는 더 높아야",
				i, s.CustomerAsk, s.RawAsk)
		}
	}
}

// validation 시나리오 — 잘못된 입력.
func TestAdmin_MarginRecompute_Validation(t *testing.T) {
	pool := pgxtest.StartTimescale(t)
	deps := &MarginRecomputeDeps{Pool: pool, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/admin/margin/recompute", PostMarginRecompute(deps))
	ts := httptest.NewServer(mux)
	defer ts.Close()

	cases := []struct {
		name string
		body string
		code int
	}{
		{"빈 from", `{"to":"2026-05-01T00:00:00Z","pair":"USD/KRW","profile":{"Channel":"WEB","Site":"HQ","Tier":"VIP"}}`, 400},
		{"from > to", `{"from":"2026-05-02T00:00:00Z","to":"2026-05-01T00:00:00Z","pair":"USD/KRW","profile":{"Channel":"WEB","Site":"HQ","Tier":"VIP"}}`, 400},
		{"pair 없음", `{"from":"2026-05-01T00:00:00Z","to":"2026-05-02T00:00:00Z","profile":{"Channel":"WEB","Site":"HQ","Tier":"VIP"}}`, 400},
		{"profile 없음", `{"from":"2026-05-01T00:00:00Z","to":"2026-05-02T00:00:00Z","pair":"USD/KRW"}`, 400},
		{"잘못된 JSON", `not-json`, 400},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, _ := http.Post(ts.URL+"/v1/admin/margin/recompute",
				"application/json", bytes.NewReader([]byte(c.body)))
			defer r.Body.Close()
			if r.StatusCode != c.code {
				b, _ := io.ReadAll(r.Body)
				t.Errorf("status=%d, want %d body=%s", r.StatusCode, c.code, b)
			}
		})
	}
}

// pool 없으면 503.
func TestAdmin_MarginRecompute_NoPool(t *testing.T) {
	deps := &MarginRecomputeDeps{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/admin/margin/recompute", PostMarginRecompute(deps))
	ts := httptest.NewServer(mux)
	defer ts.Close()

	body := []byte(`{"from":"2026-05-01T00:00:00Z","to":"2026-05-02T00:00:00Z","pair":"USD/KRW","profile":{"Channel":"WEB","Site":"HQ","Tier":"VIP"}}`)
	r, _ := http.Post(ts.URL+"/v1/admin/margin/recompute", "application/json", bytes.NewReader(body))
	defer r.Body.Close()
	if r.StatusCode != 503 {
		t.Errorf("pool nil 시 status=%d, want 503", r.StatusCode)
	}
}
