package price

import (
	"context"
	"io"
	"log/slog"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	wtgpb "github.com/winwaysystems/wtg/pkg/wtgpb/v1"
)

// TickSubscriber — mci-price 가 quote-forwarder 의 TickIngestService 에 dial-in 해서
// raw tick stream 을 구독하고 Server.IngestEnvelopes 로 주입한다.
//
// 시세 gRPC-only HA fan-in (docs/price-ha-grpc.md): broker subscribe 를 대체.
// forwarder 가 dial-in 한 구독자 전체에 fan-out 하므로, 다중 mci-price 가 각자
// dial → 각자 full 스트림 → 결정적 BEST (Active-Active). forwarder 재시작/끊김 시
// backoff 재연결 (self-heal).
//
// 다중 forwarder(dual-active HA) 구독 시 중복 tick 이 오므로, 무손실을 원하면
// (source, seq) dedup 이 필요하다 — 현재는 단일 source 기준 scaffold.
type TickSubscriber struct {
	serv         *Server
	addr         string
	subscriberID string
	logger       *slog.Logger
}

// NewTickSubscriber — addr 은 forwarder 의 --tick-listen 주소.
func NewTickSubscriber(serv *Server, addr, subscriberID string, logger *slog.Logger) *TickSubscriber {
	if logger == nil {
		logger = slog.Default()
	}
	return &TickSubscriber{
		serv:         serv,
		addr:         addr,
		subscriberID: subscriberID,
		logger:       logger.With(slog.String("tick_source", addr)),
	}
}

// Run — ctx 취소 전까지 구독 유지 + backoff 재연결. 별도 goroutine 으로 호출.
func (t *TickSubscriber) Run(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		err := t.subscribeOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		t.logger.Warn("tick 구독 끊김 — 재연결 대기", slog.Any("error", err), slog.Duration("backoff", backoff))
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

// subscribeOnce — 1회 dial + 구독 loop. 끊기면 error 반환 (Run 이 재연결).
func (t *TickSubscriber) subscribeOnce(ctx context.Context) error {
	conn, err := grpc.NewClient(t.addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return err
	}
	defer conn.Close()

	cli := wtgpb.NewTickIngestServiceClient(conn)
	stream, err := cli.SubscribeTicks(ctx, &wtgpb.SubscribeTicksRequest{SubscriberId: t.subscriberID})
	if err != nil {
		return err
	}
	t.logger.Info("forwarder tick 구독 시작")

	for {
		tick, err := stream.Recv()
		if err == io.EOF {
			return io.EOF
		}
		if err != nil {
			return err
		}
		if len(tick.GetBody()) == 0 {
			continue
		}
		// PublishTick handler(grpc.go)와 동일 dispatch — proto Tick → internal base
		// Tick → IngestEnvelopes (BestConsumer/PricingConsumer/Aggregator 경로).
		base := &Tick{
			MarketID: tick.GetMarketId(),
			Symbol:   tick.GetSymbol(),
			SeqNum:   tick.GetSeqNum(),
			Mask:     tick.GetMask(),
			Type:     uint8(tick.GetType()),
			Flag:     uint8(tick.GetFlag()),
		}
		t.serv.IngestEnvelopes(tick.GetBody(), base)
	}
}
