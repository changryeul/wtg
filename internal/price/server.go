package price

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/winwaysystems/wtg/pkg/metrics"
	"github.com/winwaysystems/wtg/pkg/mymq"
	"github.com/winwaysystems/wtg/pkg/tlsutil"
)

// Subscriber 는 mymq.Client 의 unsolicited 채널 추상화 (테스트 mock 가능).
type Subscriber interface {
	Subscribe() <-chan *mymq.Unsolicited
}

// TickConsumer 는 정규화된 Tick 을 받아가는 다운스트림 인터페이스.
//
// Phase 4 1차 prototype: stdout dump / stats / 단위 테스트용 collector.
// Phase 5 (DMZ edge) 에서 gRPC stream 구현체로 대체.
type TickConsumer interface {
	OnTick(*Tick)
}

// TickConsumerFunc 는 함수를 TickConsumer 로 어댑트.
type TickConsumerFunc func(*Tick)

func (f TickConsumerFunc) OnTick(t *Tick) { f(t) }

// Server 는 mci-price 의 핵심 컴포넌트.
//
// 흐름:
//
//	mymq Subscribe → 필터 (Func == FCCast/FCPush, Exchange == cfg.Exchange)
//	  → DecodePushData → Conflation.Update → consumer.OnTick
//	  → 통계 카운터
//
// HTTP 서버는 헬스체크 + stats 만 제공 (별도 fanout 인터페이스는 consumer 로
// 추상화).
type Server struct {
	cfg    Config
	logger *slog.Logger

	mq         *mymq.Client
	conflation *Conflation
	consumers  []TickConsumer
	metrics    *metrics.Registry

	totalRecv  atomic.Uint64
	totalMatch atomic.Uint64 // exchange 필터 통과 건수
	totalDrop  atomic.Uint64 // 디코딩 실패 등

	http *http.Server
}

// NewServer 는 Server 를 구성 (broker 미접속 상태).
func NewServer(cfg Config, logger *slog.Logger, consumers ...TickConsumer) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{
		cfg:        cfg,
		logger:     logger,
		conflation: NewConflation(),
		consumers:  consumers,
		metrics:    metrics.NewRegistry(),
	}
}

// AddConsumer 는 Tick 다운스트림 소비자를 추가한다 (Start 전 호출).
func (s *Server) AddConsumer(c TickConsumer) {
	s.consumers = append(s.consumers, c)
}

// Conflation 은 외부에서 latest 시세 조회용 (모니터링/디버깅).
func (s *Server) Conflation() *Conflation { return s.conflation }

// Start 는 broker 연결 + 구독 + HTTP 서버를 가동한다 (블로킹).
func (s *Server) Start(ctx context.Context) error {
	brokerTLS, err := loadBrokerTLS(&s.cfg)
	if err != nil {
		return fmt.Errorf("broker TLS 구성: %w", err)
	}
	mq, err := mymq.Open(ctx, s.cfg.BrokerHost, s.cfg.BrokerPort, mymq.Options{
		ApplName:         s.cfg.ApplName,
		Instance:         s.cfg.Instance,
		Channel:          mymq.ChannelWeb,
		DialTimeout:      s.cfg.DialTimeout,
		HandshakeTimeout: s.cfg.HandshakeTimeout,
		Logger:           s.logger,
		TLS:              brokerTLS,
		Queue: &mymq.QueueOptions{
			Name:  s.cfg.QueueName,
			Attr:  mymq.QtClient,
			Flags: mymq.QfUnsolMsg | mymq.QfUnsolHdr,
		},
		Reconnect: &mymq.ReconnectOptions{
			InitialBackoff: 1 * time.Second,
			MaxBackoff:     30 * time.Second,
			BackoffFactor:  2.0,
		},
	})
	if err != nil {
		return fmt.Errorf("mymq.Open: %w", err)
	}
	s.mq = mq

	// 구독 goroutine.
	subCtx, subCancel := context.WithCancel(ctx)
	defer subCancel()
	go s.subscribeLoop(subCtx, mq)

	// 통계 출력 goroutine.
	if s.cfg.StatsInterval > 0 {
		go s.statsLoop(subCtx)
	}

	// gRPC PriceService — TickConsumer 로 등록 후 Subscribe stream 노출.
	if s.cfg.GRPCAddr != "" {
		grpcSrv := NewGRPCServer(s.logger, s.cfg.GRPCBufSize)
		s.AddConsumer(grpcSrv)

		var grpcOpts []grpc.ServerOption
		if s.cfg.GRPCTLSCertFile != "" && s.cfg.GRPCTLSKeyFile != "" {
			tlsCfg, err := tlsutil.LoadServer(tlsutil.ServerOptions{
				CertFile:     s.cfg.GRPCTLSCertFile,
				KeyFile:      s.cfg.GRPCTLSKeyFile,
				ClientCAFile: s.cfg.GRPCTLSClientCAFile,
			})
			if err != nil {
				return fmt.Errorf("gRPC TLS 구성: %w", err)
			}
			grpcOpts = append(grpcOpts, grpc.Creds(credentials.NewTLS(tlsCfg)))
			s.logger.Info("gRPC TLS 활성화",
				slog.String("addr", s.cfg.GRPCAddr),
				slog.Bool("mtls", s.cfg.GRPCTLSClientCAFile != ""),
			)
		}

		go func() {
			if err := grpcSrv.Serve(subCtx, s.cfg.GRPCAddr, grpcOpts...); err != nil {
				s.logger.Error("gRPC 서버 종료", slog.Any("error", err))
			}
		}()
	}

	// HTTP 서버.
	if err := s.startHTTP(ctx); err != nil {
		return err
	}
	return nil
}

// subscribeLoop 는 mymq Subscribe 를 소비해서 conflation + consumer 호출.
func (s *Server) subscribeLoop(ctx context.Context, sub Subscriber) {
	ch := sub.Subscribe()
	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-ch:
			if !ok {
				s.logger.Info("Subscribe 채널 종료 — subscribeLoop 종료")
				return
			}
			s.totalRecv.Add(1)
			s.handleUnsolicited(msg)
		}
	}
}

// handleUnsolicited 는 단일 unsolicited 메시지를 처리.
func (s *Server) handleUnsolicited(msg *mymq.Unsolicited) {
	// FC_CAST / FC_PUSH 만 처리.
	switch msg.Header.Func {
	case mymq.FCCast, mymq.FCPush:
	default:
		return
	}

	// Exchange 필터 (cfg.ExchangeName 비어있으면 모두 통과).
	if s.cfg.ExchangeName != "" {
		if msg.Prefix == nil || msg.Prefix.ExchangeString() != s.cfg.ExchangeName {
			return
		}
	}
	s.totalMatch.Add(1)

	tick, err := DecodePushData(msg.Body)
	if err != nil {
		s.totalDrop.Add(1)
		s.logger.Debug("pushdata 디코딩 실패",
			slog.Any("error", err),
			slog.Int("body_len", len(msg.Body)),
		)
		return
	}

	// Conflation 업데이트.
	s.conflation.Update(tick)

	// 다운스트림 consumer 호출.
	for _, c := range s.consumers {
		c.OnTick(tick)
	}
}

// statsLoop 는 주기적으로 통계 로그를 출력한다.
func (s *Server) statsLoop(ctx context.Context) {
	t := time.NewTicker(s.cfg.StatsInterval)
	defer t.Stop()
	var prevRecv uint64
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			recv := s.totalRecv.Load()
			match := s.totalMatch.Load()
			drop := s.totalDrop.Load()
			cstats := s.conflation.Stats()
			rate := float64(recv-prevRecv) / s.cfg.StatsInterval.Seconds()
			prevRecv = recv

			s.logger.Info("price stats",
				slog.Time("ts", now),
				slog.Uint64("recv", recv),
				slog.Uint64("match", match),
				slog.Uint64("drop", drop),
				slog.Float64("rate_per_sec", rate),
				slog.Uint64("symbols", cstats.Symbols),
				slog.Uint64("conflation_swaps", cstats.Swaps),
			)
		}
	}
}

// Stats 는 Server 의 누적 카운터 + conflation 스냅샷.
type Stats struct {
	Received uint64          `json:"received"`
	Matched  uint64          `json:"matched"`
	Dropped  uint64          `json:"dropped"`
	Conf     ConflationStats `json:"conflation"`
}

// Stats 는 외부 노출용 카운터.
func (s *Server) Stats() Stats {
	return Stats{
		Received: s.totalRecv.Load(),
		Matched:  s.totalMatch.Load(),
		Dropped:  s.totalDrop.Load(),
		Conf:     s.conflation.Stats(),
	}
}

// startHTTP 는 헬스체크 + stats endpoint 만 제공.
func (s *Server) startHTTP(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/ping", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"status":  "ok",
			"service": "mci-price",
			"time":    time.Now().UTC().Format(time.RFC3339),
		})
	})
	mux.HandleFunc("GET /v1/price-stats", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, s.Stats())
	})
	mux.Handle("GET /metrics", s.metrics.Handler())

	s.http = &http.Server{
		Addr:         s.cfg.ListenAddr,
		Handler:      metrics.HTTPMiddleware(s.metrics, "mci-price")(mux),
		ReadTimeout:  s.cfg.ReadTimeout,
		WriteTimeout: s.cfg.WriteTimeout,
		BaseContext:  func(_ net.Listener) context.Context { return ctx },
	}

	s.logger.Info("HTTP listen 시작",
		slog.String("addr", s.cfg.ListenAddr),
		slog.String("broker", fmt.Sprintf("%s:%d", s.cfg.BrokerHost, s.cfg.BrokerPort)),
		slog.String("queue", s.cfg.QueueName),
		slog.String("exchange", s.cfg.ExchangeName),
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

// Shutdown 은 HTTP + MyMQ 정리.
func (s *Server) Shutdown(ctx context.Context) error {
	var first error
	if s.http != nil {
		if err := s.http.Shutdown(ctx); err != nil {
			first = err
		}
	}
	if s.mq != nil {
		if err := s.mq.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// loadBrokerTLS — Config 의 broker TLS 옵션이 있으면 *tls.Config, 아니면 nil.
func loadBrokerTLS(cfg *Config) (*tls.Config, error) {
	if cfg.BrokerTLSCertFile == "" && cfg.BrokerTLSCAFile == "" {
		return nil, nil
	}
	return tlsutil.LoadClient(tlsutil.ClientOptions{
		CertFile:     cfg.BrokerTLSCertFile,
		KeyFile:      cfg.BrokerTLSKeyFile,
		ServerCAFile: cfg.BrokerTLSCAFile,
		ServerName:   cfg.BrokerTLSSNI,
	})
}
