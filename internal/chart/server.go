package chart

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/winwaysystems/wtg/pkg/quote"
	"github.com/winwaysystems/wtg/pkg/session"
)

// Server 는 mci-chart 의 핵심 컴포넌트.
//
// HTTP / WS 흐름:
//
//	/v1/ping           — health
//	/v1/chart-stats    — 누적 카운터
//	/v1/chart          — 봉 조회 (Repository.QueryBars) — historical
//	/v1/chart/stream   — ws (Hub fan-out) — 라이브 closed 봉 (UpstreamGRPC 활성 시)
//
// 라이브 봉 흐름:
//
//	mci-price.Aggregator → onClose → GRPCServer.PublishBar
//	  → SubscribeBar stream → mci-chart.subscribeBarLoop
//	  → Hub.Publish(pair, tf, JSON) → matching ws clients
type Server struct {
	cfg    Config
	repo   Repository
	pool   *pgxpool.Pool // owned by server when nil-Repository 로 시작한 경우
	logger *slog.Logger
	hub    *Hub

	totalRequests atomic.Uint64
	totalRows     atomic.Uint64
	totalErrors   atomic.Uint64
	totalBarsRecv atomic.Uint64

	http *http.Server
}

// NewServer 는 Server 를 구성 (DB 연결은 Start 에서).
func NewServer(cfg Config, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{
		cfg:    cfg,
		logger: logger,
		hub:    NewHub(logger),
	}
}

// WithRepository 는 테스트 / dev 용으로 Repository 를 주입.
// 호출하지 않으면 Start 시 cfg.DSN 으로 pgxpool 을 만든다.
func (s *Server) WithRepository(repo Repository) *Server {
	s.repo = repo
	return s
}

// Start 는 ctx 가 종료될 때까지 HTTP 서버를 운영한다 (블로킹).
// Repository 가 미설정이면 cfg.DSN 으로 pgxpool 을 생성한다 (Shutdown 에서 닫음).
func (s *Server) Start(ctx context.Context) error {
	if s.repo == nil {
		if s.cfg.DSN == "" {
			return errors.New("chart: Repository 미주입이고 DSN 도 비어있음")
		}
		poolCfg, err := pgxpool.ParseConfig(s.cfg.DSN)
		if err != nil {
			return fmt.Errorf("chart: DSN 파싱: %w", err)
		}
		poolCfg.MaxConns = int32(s.cfg.PoolMaxConns)
		pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
		if err != nil {
			return fmt.Errorf("chart: pgxpool.New: %w", err)
		}
		s.pool = pool
		s.repo = NewPgxRepository(pool)
		s.logger.Info("TimescaleDB 연결 풀 생성",
			slog.Int("max_conns", s.cfg.PoolMaxConns),
		)
	}

	if s.cfg.StatsInterval > 0 {
		go s.statsLoop(ctx)
	}

	// 라이브 stream — UpstreamGRPC 가 채워진 경우만.
	if s.cfg.UpstreamGRPC != "" {
		go s.subscribeBarLoop(ctx)
	} else {
		s.logger.Info("라이브 봉 stream 비활성 (--upstream 미설정)")
	}

	return s.startHTTP(ctx)
}

func (s *Server) startHTTP(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/ping", s.handlePing)
	mux.HandleFunc("GET /v1/chart-stats", s.handleStats)
	mux.Handle("GET /v1/chart", s.wrapMetrics(handleChart(s.repo, s.cfg.QueryMaxRows, s.logger)))
	mux.HandleFunc("GET /v1/chart/stream", s.handleChartStream)

	// 챠트 SPA — embed UI. /v1/* 외의 모든 경로는 정적 파일 서빙.
	// http.ServeMux 의 패턴 매칭 — "/" 는 catch-all 이라 미지정 path 는 UI 로.
	mux.Handle("GET /", UIHandler())

	s.http = &http.Server{
		Addr:         s.cfg.ListenAddr,
		Handler:      mux,
		ReadTimeout:  s.cfg.ReadTimeout,
		WriteTimeout: s.cfg.WriteTimeout,
		BaseContext:  func(_ net.Listener) context.Context { return ctx },
	}

	s.logger.Info("mci-chart HTTP listen 시작",
		slog.String("addr", s.cfg.ListenAddr),
	)

	errCh := make(chan error, 1)
	go func() {
		err := s.http.ListenAndServe()
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		errCh <- err
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return s.Shutdown(shutdownCtx)
	case err := <-errCh:
		return err
	}
}

// Shutdown 은 HTTP + (소유한 경우) DB pool + Hub 정리.
func (s *Server) Shutdown(ctx context.Context) error {
	var first error
	if s.http != nil {
		if err := s.http.Shutdown(ctx); err != nil {
			first = err
		}
	}
	if s.hub != nil {
		s.hub.CloseAll()
	}
	if s.pool != nil {
		s.pool.Close()
	}
	return first
}

// handleChartStream 은 GET /v1/chart/stream — ws upgrade + Hub 등록.
//
// 쿼리 파라미터:
//
//	pairs=USD/KRW,EUR/KRW  (콤마 구분; 비면 모든 pair)
//	tfs=1m,5m              (콤마 구분; 비면 모든 tf)
//	since=RFC3339          (gap fill — ws 재연결 후 missing bar backfill)
//
// since 가 있고 pairs+tfs 명시되면 ws upgrade 직후 Repository.QueryBars 로
// since~now 범위의 closed 봉을 backfill 송신 (`"source":"backfill"`) 후
// `{"type":"backfill_done"}` 센티넬 → 일반 live stream 시작. 클라이언트는
// 마지막 봉 ts 를 since 로 재연결 → gap 자동 충전.
//
// ws 메시지 (in): {"op":"sub","pairs":[...],"tfs":[...]} — 런타임 필터 갱신.
// ws 메시지 (out): {"type":"bar", ...} (encodeBarJSON 결과) / {"type":"backfill_done"}
func (s *Server) handleChartStream(w http.ResponseWriter, r *http.Request) {
	upgrader := &websocket.Upgrader{
		ReadBufferSize:  4096,
		WriteBufferSize: 4096,
		// mci-chart 는 인증 없는 사내망 서비스 (운영에선 mci-edge-chart 가 DMZ 인증 책임).
		// 모든 origin 허용 — admin UI(:9090) / 직접 접속(:8086) / edge-chart proxy 모두 통과.
		CheckOrigin: func(r *http.Request) bool { return true },
	}
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		s.logger.Warn("chart ws upgrade 실패", slog.Any("error", err))
		return
	}
	sub := NewSubscriber(ws, SubscriberOptions{
		SendQueueSize: s.cfg.WsSendQueueSize,
		Logger:        s.logger,
		OnClose: func(sb *Subscriber) {
			s.hub.Remove(sb)
		},
	})
	// 초기 필터: query 파라미터.
	pairs := splitFilter(r.URL.Query().Get("pairs"))
	tfs := splitFilter(r.URL.Query().Get("tfs"))
	sub.SetFilters(pairs, tfs)

	go s.chartWriteLoop(sub)
	go s.chartReadLoop(sub)

	// gap fill — since 가 있고 pairs+tfs 모두 명시되면 backfill.
	// since 만 있고 pairs/tfs 한쪽이라도 비면 무리 (전체 fetch 위험) — skip + warn.
	if sinceStr := r.URL.Query().Get("since"); sinceStr != "" {
		s.backfillSubscriber(r.Context(), sub, pairs, tfs, sinceStr)
	}

	// live stream 등록 — backfill 후 (ordering: backfill 봉 먼저, 그 다음 live).
	s.hub.Add(sub)
}

// backfillSubscriber — since 이후 closed 봉을 DB 에서 fetch 해서 sub 의 send
// 채널로 enqueue. 채널 capacity 초과 시 drop + 카운트 (sub.SendBlocked).
//
// 한도: pair × tf 조합 당 maxBackfillBars (default 500). 그 이상은 운영자가
// REST /v1/chart 로 직접 fetch.
const maxBackfillBars = 500

func (s *Server) backfillSubscriber(ctx context.Context, sub *Subscriber, pairs, tfs []string, sinceStr string) {
	if len(pairs) == 0 || len(tfs) == 0 {
		s.logger.Warn("backfill skip — pairs+tfs 둘 다 명시 필요",
			slog.String("since", sinceStr))
		return
	}
	if s.repo == nil {
		s.logger.Warn("backfill skip — repository nil (DB 미연결)")
		return
	}
	since, err := time.Parse(time.RFC3339, sinceStr)
	if err != nil {
		s.logger.Warn("backfill skip — since RFC3339 파싱 실패",
			slog.String("since", sinceStr), slog.Any("err", err))
		return
	}
	now := time.Now().UTC()

	queryCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	totalSent := 0
	for _, pairStr := range pairs {
		for _, tfStr := range tfs {
			tf := quote.Timeframe(tfStr)
			if tf.Duration() <= 0 {
				continue
			}
			bars, qerr := s.repo.QueryBars(queryCtx, session.Pair(pairStr), tf, since, now, maxBackfillBars)
			if qerr != nil {
				s.logger.Warn("backfill query 실패",
					slog.String("pair", pairStr), slog.String("tf", tfStr),
					slog.Any("err", qerr))
				continue
			}
			for i := range bars {
				payload, perr := encodeBarJSONFromQuote(bars[i])
				if perr != nil {
					continue
				}
				if serr := sub.Send(payload); serr != nil {
					// 채널 full — backfill 양 과다. 운영자에게 알릴 수 있게 break.
					s.logger.Warn("backfill send queue full",
						slog.String("pair", pairStr), slog.String("tf", tfStr),
						slog.Int("sent_before_drop", totalSent),
						slog.Any("err", serr))
					goto done
				}
				totalSent++
			}
		}
	}
done:
	// sentinel — 클라이언트가 backfill 종료 알 수 있게.
	doneMsg, _ := json.Marshal(map[string]any{
		"type":  "backfill_done",
		"count": totalSent,
		"since": sinceStr,
	})
	_ = sub.Send(doneMsg)
	s.logger.Info("backfill 완료",
		slog.String("since", sinceStr),
		slog.Int("count", totalSent),
		slog.Int("pairs", len(pairs)),
		slog.Int("tfs", len(tfs)))
}

func splitFilter(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func (s *Server) chartWriteLoop(sub *Subscriber) {
	ticker := time.NewTicker(s.cfg.WsPingInterval)
	defer ticker.Stop()
	defer sub.Close()
	for {
		select {
		case <-sub.closeC:
			return
		case payload, ok := <-sub.send:
			if !ok {
				return
			}
			sub.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := sub.conn.WriteMessage(websocket.TextMessage, payload); err != nil {
				return
			}
		case <-ticker.C:
			sub.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
			if err := sub.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

// chartReadLoop 는 클라이언트가 보낸 {"op":"sub", ...} 로 필터 갱신.
func (s *Server) chartReadLoop(sub *Subscriber) {
	defer sub.Close()
	sub.conn.SetReadDeadline(time.Now().Add(s.cfg.WsPongTimeout))
	sub.conn.SetPongHandler(func(string) error {
		sub.conn.SetReadDeadline(time.Now().Add(s.cfg.WsPongTimeout))
		return nil
	})
	for {
		_, msg, err := sub.conn.ReadMessage()
		if err != nil {
			return
		}
		var in struct {
			Op    string   `json:"op"`
			Pairs []string `json:"pairs"`
			Tfs   []string `json:"tfs"`
		}
		if err := json.Unmarshal(msg, &in); err != nil {
			continue // 잘못된 메시지는 무시
		}
		if in.Op == "sub" {
			sub.SetFilters(in.Pairs, in.Tfs)
		}
	}
}

// wrapMetrics 는 응답 후 카운터를 갱신한다 (간단한 inline 미들웨어).
func (s *Server) wrapMetrics(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, status: 200}
		next.ServeHTTP(rec, r)
		s.totalRequests.Add(1)
		if rec.status >= 500 {
			s.totalErrors.Add(1)
		}
	}
}

func (s *Server) handlePing(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "ok",
		"service": "mci-chart",
		"time":    time.Now().UTC().Format(time.RFC3339),
	})
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, ServerStats{
		Requests: s.totalRequests.Load(),
		Errors:   s.totalErrors.Load(),
		Rows:     s.totalRows.Load(),
		Bars:     s.totalBarsRecv.Load(),
		Hub:      s.hub.Stats(),
	})
}

// ServerStats 는 외부 노출 카운터.
type ServerStats struct {
	Requests uint64   `json:"requests"`
	Errors   uint64   `json:"errors"`
	Rows     uint64   `json:"rows"`
	Bars     uint64   `json:"bars_received"`
	Hub      HubStats `json:"hub"`
}

// AddRows 는 핸들러가 반환한 row 수를 누적 (현재는 wrap 외부에서 호출 안 함;
// 향후 정밀 메트릭이 필요하면 chartResponse 응답 후 합산).
func (s *Server) AddRows(n int) {
	if n > 0 {
		s.totalRows.Add(uint64(n))
	}
}

// statusRecorder 는 응답 상태 코드를 캡처.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// statsLoop 는 주기적으로 stats 를 로그한다.
func (s *Server) statsLoop(ctx context.Context) {
	t := time.NewTicker(s.cfg.StatsInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			s.logger.Info("chart stats",
				slog.Time("ts", now),
				slog.Uint64("requests", s.totalRequests.Load()),
				slog.Uint64("errors", s.totalErrors.Load()),
				slog.Uint64("rows", s.totalRows.Load()),
			)
		}
	}
}
