package chart

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/winwaysystems/wtg/pkg/quote"
	"github.com/winwaysystems/wtg/pkg/session"
)

// fakeRepository 는 Repository 의 테스트 더블.
type fakeRepository struct {
	mu    sync.Mutex
	calls []callRec
	bars  []quote.Bar
	err   error
}

type callRec struct {
	pair  session.Pair
	tf    quote.Timeframe
	from  time.Time
	to    time.Time
	limit int
}

func (f *fakeRepository) QueryBars(ctx context.Context, pair session.Pair, tf quote.Timeframe, from, to time.Time, limit int) ([]quote.Bar, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, callRec{pair, tf, from, to, limit})
	if f.err != nil {
		return nil, f.err
	}
	return f.bars, nil
}

func newTestServer(repo Repository) *httptest.Server {
	cfg := DefaultConfig()
	cfg.QueryMaxRows = 100
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/ping", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.Handle("GET /v1/chart", handleChart(repo, cfg.QueryMaxRows, slogDiscard()))
	return httptest.NewServer(mux)
}

func TestHandleChart_Success(t *testing.T) {
	from := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)
	to := from.Add(time.Hour)
	repo := &fakeRepository{
		bars: []quote.Bar{
			{
				Pair: "USD/KRW", TF: quote.TF1m,
				OpenedAt: from, ClosedAt: from.Add(time.Minute),
				OpenBid: 1399.5, OpenAsk: 1399.6,
				HighBid: 1400.0, HighAsk: 1400.1,
				LowBid: 1399.0, LowAsk: 1399.1,
				CloseBid: 1399.8, CloseAsk: 1399.9,
				TickCount: 30,
			},
		},
	}
	ts := newTestServer(repo)
	defer ts.Close()

	url := ts.URL + "/v1/chart?pair=USD/KRW&tf=1m" +
		"&from=" + from.Format(time.RFC3339) +
		"&to=" + to.Format(time.RFC3339)
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	var got chartResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Pair != "USD/KRW" || got.TF != quote.TF1m {
		t.Errorf("response pair/tf mismatch: %+v", got)
	}
	if got.Count != 1 || len(got.Bars) != 1 {
		t.Fatalf("Count=%d len(Bars)=%d", got.Count, len(got.Bars))
	}
	if got.Bars[0].OpenBid != 1399.5 || got.Bars[0].CloseAsk != 1399.9 {
		t.Errorf("bar values mismatch: %+v", got.Bars[0])
	}

	// repository 가 올바른 인자로 호출됐는지.
	if len(repo.calls) != 1 {
		t.Fatalf("repo call 수 = %d", len(repo.calls))
	}
	c := repo.calls[0]
	if c.pair != "USD/KRW" || c.tf != quote.TF1m {
		t.Errorf("repo call pair/tf: %+v", c)
	}
	if c.limit != 100 {
		t.Errorf("repo limit = %d, want 100 (default = maxRows)", c.limit)
	}
}

func TestHandleChart_BadRequest(t *testing.T) {
	repo := &fakeRepository{}
	ts := newTestServer(repo)
	defer ts.Close()

	tests := []struct {
		name  string
		query string
	}{
		{"missing pair", "tf=1m&from=2026-05-23T00:00:00Z&to=2026-05-23T01:00:00Z"},
		{"missing tf", "pair=USD/KRW&from=2026-05-23T00:00:00Z&to=2026-05-23T01:00:00Z"},
		{"tf=1s (비영속)", "pair=USD/KRW&tf=1s&from=2026-05-23T00:00:00Z&to=2026-05-23T01:00:00Z"},
		{"tf=bogus", "pair=USD/KRW&tf=bogus&from=2026-05-23T00:00:00Z&to=2026-05-23T01:00:00Z"},
		{"bad from", "pair=USD/KRW&tf=1m&from=oops&to=2026-05-23T01:00:00Z"},
		{"to <= from", "pair=USD/KRW&tf=1m&from=2026-05-23T01:00:00Z&to=2026-05-23T01:00:00Z"},
		{"bad limit", "pair=USD/KRW&tf=1m&from=2026-05-23T00:00:00Z&to=2026-05-23T01:00:00Z&limit=-1"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := http.Get(ts.URL + "/v1/chart?" + tc.query)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", resp.StatusCode)
			}
			var e errorResponse
			if err := json.NewDecoder(resp.Body).Decode(&e); err != nil {
				t.Fatal(err)
			}
			if e.Code != "bad_request" {
				t.Errorf("code = %q, want bad_request", e.Code)
			}
		})
	}
}

func TestHandleChart_RepositoryError(t *testing.T) {
	repo := &fakeRepository{err: errors.New("db down")}
	ts := newTestServer(repo)
	defer ts.Close()

	url := ts.URL + "/v1/chart?pair=USD/KRW&tf=1m" +
		"&from=2026-05-23T00:00:00Z&to=2026-05-23T01:00:00Z"
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", resp.StatusCode)
	}
}

func TestHandleChart_LimitCapping(t *testing.T) {
	repo := &fakeRepository{}
	ts := newTestServer(repo)
	defer ts.Close()

	// max=100 인데 limit=99999 요청 → repo 에는 100 으로 전달.
	url := ts.URL + "/v1/chart?pair=USD/KRW&tf=1m" +
		"&from=2026-05-23T00:00:00Z&to=2026-05-23T01:00:00Z&limit=99999"
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if len(repo.calls) != 1 {
		t.Fatal("repo not called")
	}
	if repo.calls[0].limit != 100 {
		t.Errorf("limit capped to %d, want 100", repo.calls[0].limit)
	}
}

func TestConfig_Validate(t *testing.T) {
	cfg := DefaultConfig()
	if err := cfg.Validate(); err == nil {
		t.Error("DSN 비어있는데 Validate 통과")
	}
	cfg.DSN = "postgres://localhost/test"
	if err := cfg.Validate(); err != nil {
		t.Errorf("정상 cfg Validate 실패: %v", err)
	}
}

func TestServer_StartFailsWithoutRepoAndDSN(t *testing.T) {
	cfg := DefaultConfig()
	// cfg.DSN 비어있고 Repository 도 미주입.
	srv := NewServer(cfg, slogDiscard())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := srv.Start(ctx); err == nil {
		t.Error("Repository 미주입 + DSN 비어있는데 Start 가 통과")
	}
}

// slogDiscard 는 테스트 출력 노이즈를 막기 위한 io.Discard 핸들러 logger.
func slogDiscard() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
