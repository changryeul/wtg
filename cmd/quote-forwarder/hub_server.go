package main

import (
	"log/slog"
	"net"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"

	"github.com/winwaysystems/wtg/internal/forwarder/tickhub"
	"github.com/winwaysystems/wtg/pkg/quote"
	wtgpb "github.com/winwaysystems/wtg/pkg/wtgpb/v1"
)

// hubPublisher — Publisher 구현 (--publish-mode hub). envelope → wtgpb.Tick →
// marshal → Hub.Broadcast. dial-in 한 mci-price 구독자 전체에 fan-out
// (slow consumer 자동 격리). 시세 gRPC-only HA fan-in (docs/price-ha-grpc.md).
type hubPublisher struct {
	hub *tickhub.Hub
}

func newHubPublisher(hub *tickhub.Hub) *hubPublisher { return &hubPublisher{hub: hub} }

func (p *hubPublisher) Publish(envs []quote.JSONEnvelope) error {
	for i := range envs {
		body, err := quote.EncodeJSONEnvelope(envs[i])
		if err != nil {
			return err
		}
		tick := &wtgpb.Tick{Symbol: envs[i].Sym, Body: body}
		b, err := proto.Marshal(tick)
		if err != nil {
			return err
		}
		p.hub.Broadcast(b) // 구독자 0 이어도 무해 (no-op)
	}
	return nil
}

func (p *hubPublisher) Close() error { return nil }

// tickIngestServer — TickIngestService 구현. mci-price 가 SubscribeTicks 로 dial-in.
type tickIngestServer struct {
	wtgpb.UnimplementedTickIngestServiceServer
	hub    *tickhub.Hub
	logger *slog.Logger
}

// SubscribeTicks — 구독자 하나를 Hub 에 등록하고, Broadcast 된 tick 을 stream 으로 전달.
// stream 종료(ctx.Done)/격리(sub.Done) 시 정리. 구독자별 독립 send 큐라 느린
// mci-price 가 남을 막지 않는다.
func (s *tickIngestServer) SubscribeTicks(req *wtgpb.SubscribeTicksRequest, stream wtgpb.TickIngestService_SubscribeTicksServer) error {
	sub := tickhub.NewSubscriber(tickhub.SubscriberOptions{})
	s.hub.Add(sub)
	defer sub.Close()
	s.logger.Info("mci-price tick 구독 시작",
		slog.String("subscriber_id", req.GetSubscriberId()), slog.Int("subs", s.hub.Count()))

	ctx := stream.Context()
	var tick wtgpb.Tick
	for {
		select {
		case <-ctx.Done():
			s.logger.Info("mci-price tick 구독 종료 (ctx)", slog.String("subscriber_id", req.GetSubscriberId()))
			return nil
		case <-sub.Done():
			s.logger.Warn("mci-price tick 구독 격리 (slow)", slog.String("subscriber_id", req.GetSubscriberId()))
			return nil
		case b := <-sub.C():
			tick.Reset()
			if err := proto.Unmarshal(b, &tick); err != nil {
				continue
			}
			if err := stream.Send(&tick); err != nil {
				return err
			}
		}
	}
}

// startTickHubServer — --tick-listen 에 TickIngestService gRPC 서버 기동.
func startTickHubServer(addr string, hub *tickhub.Hub, logger *slog.Logger) (*grpc.Server, error) {
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	srv := grpc.NewServer()
	wtgpb.RegisterTickIngestServiceServer(srv, &tickIngestServer{hub: hub, logger: logger})
	go func() {
		logger.Info("tick 허브 gRPC 서버 listen", slog.String("addr", addr))
		if err := srv.Serve(lis); err != nil {
			logger.Error("tick 허브 서버 종료", slog.Any("error", err))
		}
	}()
	return srv, nil
}
