package krx

import (
	"log/slog"
	"sync"
	"sync/atomic"

	wtgpb "github.com/winwaysystems/wtg/pkg/wtgpb/v1"
)

// KRX 이벤트 gRPC fan-out — 통합 시세 엣지(edge-price)가 fan-in 하는 상류.
// WS Hub 와 독립된 별도 fan-out 이며, Server.fanout() 이 양쪽(WS + gRPC)에 흘린다.
// docs/unified-quote-edge-design.md §5·§7.

// grpcSub — 단일 gRPC 구독자의 큐 + 종목 필터.
type grpcSub struct {
	ch     chan *wtgpb.KrxEvent
	mu     sync.RWMutex
	filter map[string]struct{} // nil = all
}

func (s *grpcSub) matches(code string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.filter == nil {
		return true
	}
	_, ok := s.filter[code]
	return ok
}

// GRPCHub — gRPC 구독자 집합 + 종목 라우팅. slow consumer 는 drop(격리).
type GRPCHub struct {
	mu   sync.RWMutex
	subs map[uint64]*grpcSub
	seq  atomic.Uint64
}

// NewGRPCHub — 빈 hub.
func NewGRPCHub() *GRPCHub { return &GRPCHub{subs: map[uint64]*grpcSub{}} }

func (h *GRPCHub) add(s *grpcSub) uint64 {
	id := h.seq.Add(1)
	h.mu.Lock()
	h.subs[id] = s
	h.mu.Unlock()
	return id
}

func (h *GRPCHub) remove(id uint64) {
	h.mu.Lock()
	delete(h.subs, id)
	h.mu.Unlock()
}

// Count — 구독자 수 (진단).
func (h *GRPCHub) Count() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.subs)
}

// broadcast — code 매칭 구독자에게 이벤트 non-blocking 송신 (포화 시 drop).
func (h *GRPCHub) broadcast(code string, ev *wtgpb.KrxEvent) (sent, dropped int) {
	h.mu.RLock()
	snapshot := make([]*grpcSub, 0, len(h.subs))
	for _, s := range h.subs {
		if s.matches(code) {
			snapshot = append(snapshot, s)
		}
	}
	h.mu.RUnlock()
	for _, s := range snapshot {
		select {
		case s.ch <- ev:
			sent++
		default:
			dropped++ // slow consumer 격리 — 전체 지연 방지
		}
	}
	return sent, dropped
}

// GRPCServer — KrxPriceService 구현. hub 에 구독자 등록 후 stream 으로 push.
type GRPCServer struct {
	wtgpb.UnimplementedKrxPriceServiceServer
	hub    *GRPCHub
	logger *slog.Logger
}

// NewGRPCServer — hub + logger.
func NewGRPCServer(hub *GRPCHub, logger *slog.Logger) *GRPCServer {
	if logger == nil {
		logger = slog.Default()
	}
	return &GRPCServer{hub: hub, logger: logger}
}

// SubscribeKrx — 종목 필터 구독. 큐 → stream flush, ctx 종료 시 정리.
func (s *GRPCServer) SubscribeKrx(req *wtgpb.KrxSubscribeRequest, stream wtgpb.KrxPriceService_SubscribeKrxServer) error {
	sub := &grpcSub{ch: make(chan *wtgpb.KrxEvent, 1024)}
	if syms := req.GetSymbols(); len(syms) > 0 {
		sub.filter = make(map[string]struct{}, len(syms))
		for _, c := range syms {
			if c != "" {
				sub.filter[c] = struct{}{}
			}
		}
	}
	id := s.hub.add(sub)
	defer s.hub.remove(id)
	s.logger.Info("SubscribeKrx 시작",
		slog.String("subscriber_id", req.GetSubscriberId()),
		slog.Int("filter", len(sub.filter)))

	ctx := stream.Context()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev := <-sub.ch:
			if err := stream.Send(ev); err != nil {
				return err
			}
		}
	}
}

// krxEventFrom — legacy struct JSON → KrxEvent (v2 헤더 + payload). buildKrxV2 와
// 동일 메타 파싱 재사용 (kind/time/asset_class).
func krxEventFrom(code string, legacy []byte) *wtgpb.KrxEvent {
	kind, tm, err := krxMeta(legacy)
	if err != nil {
		return nil
	}
	return &wtgpb.KrxEvent{
		Type:       "krx." + kind,
		AssetClass: assetClassForKind(kind),
		Symbol:     code,
		TsUnixNano: krxTimeToUnixNano(tm),
		Payload:    legacy,
	}
}
