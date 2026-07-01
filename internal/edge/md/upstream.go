package md

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	wtgpb "github.com/winwaysystems/wtg/pkg/wtgpb/v1"
)

// GrpcQuoteSource — mci-price 의 PriceService.SubscribeQuote 를 소비해 심볼별
// 최신 quote 를 캐시한다. Phase B-2a 는 MDR 응답 시 static provider 대신 이
// 캐시를 우선 사용 (스냅샷 1회). Phase B-2b 에서 subscribe(35=X 증분) 붙임.
//
// 하나의 gRPC connection + 하나의 stream 을 프로세스 전체가 공유. profileKeys
// 로 필터 (등록된 카운터파티 profile 의 합집합. Phase B-2a 는 편의상 빈 리스트
// = 서버측 default 로 두어 모두 수신).
type GrpcQuoteSource struct {
	upstreamAddr string
	profileKeys  []string
	subscriberID string
	logger       *slog.Logger

	mu    sync.RWMutex
	cache map[string]*wtgpb.CustomerQuote // pair → 최신 quote

	// 카운터 (진단).
	received atomic.Uint64
	errors   atomic.Uint64
}

// NewGrpcQuoteSource — 시작 전 초기화만.
func NewGrpcQuoteSource(upstreamAddr, subscriberID string, profileKeys []string, logger *slog.Logger) *GrpcQuoteSource {
	if logger == nil {
		logger = slog.Default()
	}
	return &GrpcQuoteSource{
		upstreamAddr: upstreamAddr,
		profileKeys:  profileKeys,
		subscriberID: subscriberID,
		logger:       logger,
		cache:        make(map[string]*wtgpb.CustomerQuote),
	}
}

// Latest — 심볼의 최신 quote. cache miss 시 (nil, false).
func (g *GrpcQuoteSource) Latest(sym string) (*wtgpb.CustomerQuote, bool) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	cq, ok := g.cache[sym]
	return cq, ok
}

// Stats — 진단용 스냅샷.
type GrpcQuoteSourceStats struct {
	Received   uint64 `json:"received"`
	Errors     uint64 `json:"errors"`
	CacheSize  int    `json:"cache_size"`
}

func (g *GrpcQuoteSource) Stats() GrpcQuoteSourceStats {
	g.mu.RLock()
	sz := len(g.cache)
	g.mu.RUnlock()
	return GrpcQuoteSourceStats{
		Received:  g.received.Load(),
		Errors:    g.errors.Load(),
		CacheSize: sz,
	}
}

// StartLoop — gRPC connection + SubscribeQuote stream 유지. 끊김 시 backoff 재시도.
// 블로킹. ctx cancel 로 종료. 별도 goroutine 에서 호출.
func (g *GrpcQuoteSource) StartLoop(ctx context.Context) {
	if g.upstreamAddr == "" {
		g.logger.Warn("GrpcQuoteSource: upstream 주소 빈값 — loop skip")
		return
	}
	// Phase B-2a — DevMode 편의로 plaintext. 운영은 mTLS 붙임 예정.
	conn, err := grpc.NewClient(g.upstreamAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		g.logger.Error("GrpcQuoteSource dial 실패 — loop 종료",
			slog.String("addr", g.upstreamAddr), slog.Any("err", err))
		return
	}
	defer conn.Close()
	client := wtgpb.NewPriceServiceClient(conn)
	g.logger.Info("GrpcQuoteSource 연결",
		slog.String("addr", g.upstreamAddr),
		slog.String("subscriber_id", g.subscriberID),
		slog.Any("profile_keys", g.profileKeys))

	backoff := 500 * time.Millisecond
	for {
		if ctx.Err() != nil {
			return
		}
		err := g.consumeOnce(ctx, client)
		if errors.Is(err, context.Canceled) || ctx.Err() != nil {
			return
		}
		g.errors.Add(1)
		g.logger.Warn("GrpcQuoteSource stream 끊김 — 재시도",
			slog.Any("err", err), slog.Duration("backoff", backoff))
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > 10*time.Second {
			backoff = 10 * time.Second
		}
	}
}

func (g *GrpcQuoteSource) consumeOnce(ctx context.Context, client wtgpb.PriceServiceClient) error {
	req := &wtgpb.QuoteSubscribeRequest{
		SubscriberId: g.subscriberID,
		ProfileKeys:  g.profileKeys,
	}
	stream, err := client.SubscribeQuote(ctx, req)
	if err != nil {
		return err
	}
	g.logger.Info("SubscribeQuote 시작",
		slog.String("subscriber_id", g.subscriberID),
		slog.Any("profile_keys", g.profileKeys))

	for {
		cq, err := stream.Recv()
		if err == io.EOF {
			return errors.New("upstream quote EOF")
		}
		if err != nil {
			return err
		}
		g.mu.Lock()
		g.cache[cq.GetPair()] = cq
		g.mu.Unlock()
		g.received.Add(1)
	}
}
