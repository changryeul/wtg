package chart

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Server 는 mci-chart 의 핵심 컴포넌트.
//
// HTTP 흐름:
//
//	/v1/ping        — health
//	/v1/chart-stats — 누적 카운터
//	/v1/chart       — 봉 조회 (Repository.QueryBars)
//
// 라이브 tick 은 mci-edge-price 의 ws 가 별도 담당. mci-chart 는 historical
// 봉만 책임진다 (관심사 분리).
type Server struct {
	cfg    Config
	repo   Repository
	pool   *pgxpool.Pool // owned by server when nil-Repository 로 시작한 경우
	logger *slog.Logger

	totalRequests atomic.Uint64
	totalRows     atomic.Uint64
	totalErrors   atomic.Uint64

	http *http.Server
}

// NewServer 는 Server 를 구성 (DB 연결은 Start 에서).
func NewServer(cfg Config, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{cfg: cfg, logger: logger}
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

	return s.startHTTP(ctx)
}

func (s *Server) startHTTP(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/ping", s.handlePing)
	mux.HandleFunc("GET /v1/chart-stats", s.handleStats)
	mux.Handle("GET /v1/chart", s.wrapMetrics(handleChart(s.repo, s.cfg.QueryMaxRows, s.logger)))

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

// Shutdown 은 HTTP + (소유한 경우) DB pool 정리.
func (s *Server) Shutdown(ctx context.Context) error {
	var first error
	if s.http != nil {
		if err := s.http.Shutdown(ctx); err != nil {
			first = err
		}
	}
	if s.pool != nil {
		s.pool.Close()
	}
	return first
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
	})
}

// ServerStats 는 외부 노출 카운터.
type ServerStats struct {
	Requests uint64 `json:"requests"`
	Errors   uint64 `json:"errors"`
	Rows     uint64 `json:"rows"`
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

