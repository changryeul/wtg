package price

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	wtgpb "github.com/winwaysystems/wtg/pkg/wtgpb/v1"

	"github.com/winwaysystems/wtg/pkg/quote"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// algo.go — 시스템 트레이딩 (내부 algo 봇) 전용 시세 stream.
//
// Phase A (skeleton):
//   - BestConsumer downstream 으로 등록 → OnTick 매 tick 마다 subscriber 에게
//     fan-out. bid/ask 만 (Source=BEST 만 통과).
//   - 심볼별 monotonic seq 자체 할당 (upstream Tick.SeqNum 은 uint32 wrap 가능
//     하고 broker publish 단위라 심볼별 순서 보장 X — 여기서 재발급).
//   - Ring buffer 는 신설되지만 backfill 로직 (from_seq > 0) 은 Phase B.
//     Phase A 는 from_seq=0 이 아니면 Unimplemented 반환.
//   - Per-client send: non-blocking channel default drop + 카운터. slow client
//     격리 (timeout + graceful disconnect) 는 Phase C.
//
// 자세히는 api/proto/wtg/v1/price.proto 의 SubscribeAlgo 주석.

// AlgoStreamServer — mci-price 안 gRPC subserver + BestConsumer 의 downstream
// TickConsumer 동시 역할. Server.AddConsumer 로 등록.
type AlgoStreamServer struct {
	logger *slog.Logger

	// per-symbol monotonic seq 발급기. atomic.Uint64 로 lock-free.
	mu      sync.RWMutex
	seqGens map[string]*atomic.Uint64 // symbol → seq counter

	// per-symbol ring buffer (Phase B backfill 대비). Phase A 는 write 만.
	rings    map[string]*algoRing // symbol → ring
	ringSize int

	// active subscribers — 심볼별 subscriber set.
	subsMu sync.RWMutex
	subs   map[*algoSub]struct{} // 전체 subscriber (심볼별 필터는 각 sub 내부).

	// Phase C — per-client isolation.
	clientBufSize int
	slowTimeout   time.Duration // 0 = disconnect 비활성
	stopWatch     chan struct{} // watcher goroutine 종료 signal

	// 카운터.
	subscribersActive atomic.Int64
	ticksEmitted      atomic.Uint64
	sendDrops         atomic.Uint64
	backfillEmitted   atomic.Uint64 // Phase B — replay 로 보낸 tick 누적
	backfillGaps      atomic.Uint64 // Phase B — sequence_gap 반환한 요청 누적
	dedupSkipped      atomic.Uint64 // Phase B — replay 후 live 시 중복 skip 누적
	disconnectedSlow  atomic.Uint64 // Phase C — slow client timeout 으로 끊은 수
}

// algoSub — 활성 subscribe stream 의 상태.
type algoSub struct {
	clientID   string
	symbolSet  map[string]struct{} // 빈 map = 모두 허용
	ch         chan *wtgpb.AlgoQuote
	dropsLocal atomic.Uint64

	// Phase C — slow client 감지. firstDropAt = 첫 drop 시각 (unix nano). 0 =
	// 정상. successful send 마다 0 으로 리셋 (일시적 폭주 후 회복은 허용).
	// watcher goroutine 이 (now - firstDropAt) > slowTimeout 이면 disconnect.
	firstDropAt atomic.Int64

	// done — watcher 가 close() 하면 SubscribeAlgo 의 for-select 가 나감.
	done chan struct{}

	// timeout 판정된 여부 (double-close 방지 + status 반환 판정).
	slowFired atomic.Bool
}

// algoRing — 심볼별 최근 N tick 을 원형 저장. Phase B backfill 지원.
//
// AlgoQuote 는 protobuf message (내부 MessageState 에 sync.Mutex 포함) 라
// 반드시 pointer 로만 다룸 — value copy 는 go vet lock-copy 위반.
type algoRing struct {
	mu   sync.Mutex
	buf  []*wtgpb.AlgoQuote // capacity = ringSize
	head int                // 다음 쓸 위치
	full bool
}

func newAlgoRing(size int) *algoRing {
	return &algoRing{buf: make([]*wtgpb.AlgoQuote, size)}
}

func (r *algoRing) push(q *wtgpb.AlgoQuote) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf[r.head] = q
	r.head++
	if r.head >= len(r.buf) {
		r.head = 0
		r.full = true
	}
}

// snapshot — from_seq 이후의 tick 을 seq 순으로 리턴.
//
// 반환:
//   - ticks: seq > fromSeq 인 tick들 (오래된 → 최신 순)
//   - oldest: ring 안 가장 오래된 seq (없으면 0). gap 판정용
//   - gap: fromSeq 이 ring 밖으로 밀려나감 (client 가 놓친 부분 복구 불가)
//
// gap 발생 조건: fromSeq+1 < oldest (client 요구 이후 tick 이 이미 덮어써짐).
// 이 때 caller 는 client 에 sequence_gap 에러를 리턴하고 snapshot 재구독을
// 유도해야 함.
//
// fromSeq >= newest 이면 ticks 비어있고 gap=false. caller 는 그냥 live 이어감.
func (r *algoRing) snapshot(fromSeq int64) (ticks []*wtgpb.AlgoQuote, oldest int64, gap bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := len(r.buf)
	var count, startIdx int
	if r.full {
		count = n
		startIdx = r.head
	} else {
		count = r.head
		startIdx = 0
	}
	if count == 0 {
		return nil, 0, false
	}
	oldest = r.buf[startIdx].GetSeq()
	// gap: fromSeq+1 < oldest → client 가 놓친 tick 이 이미 덮어써짐.
	// 예: oldest=105, fromSeq=100 → 101~104 손실 → gap.
	if fromSeq+1 < oldest {
		return nil, oldest, true
	}
	// 순회하며 seq > fromSeq 만 수집. buf 는 push 순서라 자연스레 seq 오름차순.
	for i := 0; i < count; i++ {
		idx := (startIdx + i) % n
		if r.buf[idx].GetSeq() > fromSeq {
			ticks = append(ticks, r.buf[idx])
		}
	}
	return ticks, oldest, false
}

// AlgoStreamOptions — NewAlgoStreamServer 옵션. 0 값은 default 로 대체.
type AlgoStreamOptions struct {
	RingSize          int           // 심볼별 ring buffer 크기 (default 100_000)
	ClientBufferSize  int           // per-client channel 깊이 (default 1024)
	SlowClientTimeout time.Duration // slow client disconnect 임계 (default 5s, 0=비활성)
}

// NewAlgoStreamServer — logger nil 이면 slog.Default(). watcher goroutine 은
// SlowClientTimeout > 0 이면 자동 시작.
func NewAlgoStreamServer(logger *slog.Logger, opts AlgoStreamOptions) *AlgoStreamServer {
	if logger == nil {
		logger = slog.Default()
	}
	if opts.RingSize <= 0 {
		opts.RingSize = 100_000
	}
	if opts.ClientBufferSize <= 0 {
		opts.ClientBufferSize = 1024
	}
	s := &AlgoStreamServer{
		logger:        logger,
		seqGens:       make(map[string]*atomic.Uint64),
		rings:         make(map[string]*algoRing),
		ringSize:      opts.RingSize,
		subs:          make(map[*algoSub]struct{}),
		clientBufSize: opts.ClientBufferSize,
		slowTimeout:   opts.SlowClientTimeout,
		stopWatch:     make(chan struct{}),
	}
	if opts.SlowClientTimeout > 0 {
		go s.watchLoop()
	}
	return s
}

// Stop — watcher goroutine 종료. mci-price shutdown 시 호출 권장 (nice-to-have).
func (s *AlgoStreamServer) Stop() {
	select {
	case <-s.stopWatch:
		// already closed
	default:
		close(s.stopWatch)
	}
}

// watchLoop — 500ms 주기로 sub 순회, slow client timeout 검사. Phase C.
func (s *AlgoStreamServer) watchLoop() {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-s.stopWatch:
			return
		case <-ticker.C:
			s.evictSlowSubs()
		}
	}
}

func (s *AlgoStreamServer) evictSlowSubs() {
	nowNs := time.Now().UnixNano()
	thresholdNs := s.slowTimeout.Nanoseconds()
	// snapshot sub 리스트 (하 동안 subsMu 짧게).
	s.subsMu.RLock()
	list := make([]*algoSub, 0, len(s.subs))
	for sub := range s.subs {
		list = append(list, sub)
	}
	s.subsMu.RUnlock()
	for _, sub := range list {
		firstDrop := sub.firstDropAt.Load()
		if firstDrop == 0 {
			continue
		}
		if nowNs-firstDrop < thresholdNs {
			continue
		}
		if !sub.slowFired.CompareAndSwap(false, true) {
			continue
		}
		s.disconnectedSlow.Add(1)
		s.logger.Warn("algo slow client — disconnect",
			slog.String("client_id", sub.clientID),
			slog.Uint64("drops_local", sub.dropsLocal.Load()),
			slog.Int64("first_drop_ns", firstDrop),
			slog.Int64("age_ms", (nowNs-firstDrop)/1_000_000))
		close(sub.done)
	}
}

// OnTick — TickConsumer. BestConsumer 가 fan-out 하는 합성 BEST tick 만 처리
// (다른 Source 는 skip — raw stream 은 algo 에 부적합).
func (s *AlgoStreamServer) OnTick(t *Tick) {
	if t == nil || t.Symbol == "" || t.Source != SourceBest {
		return
	}
	env, err := quote.DecodeJSONEnvelope(t.Body)
	if err != nil {
		return
	}

	// 심볼별 seq 발급.
	seq := s.nextSeq(t.Symbol)

	q := &wtgpb.AlgoQuote{
		Sym:            t.Symbol,
		Bid:            env.Bid,
		Ask:            env.Ask,
		Seq:            seq,
		TsSourceUnixNs: env.TS.UnixNano(),
		TsWtgUnixNs:    t.Received.UnixNano(),
		IsBackfill:     false,
		Last:           env.Last, // 최근 시장 체결가 (mds fillprc 대응)
		LastQty:        env.LastQty,
	}

	// ring buffer (Phase B backfill 용). 값 복사로 저장.
	if ring := s.ringFor(t.Symbol); ring != nil {
		ring.push(q)
	}

	s.ticksEmitted.Add(1)

	// active subscriber 에게 fan-out. non-blocking — 다른 sub 격리.
	// Phase C: drop 시 firstDropAt 기록 (watcher 가 timeout 판정에 사용).
	s.subsMu.RLock()
	for sub := range s.subs {
		if len(sub.symbolSet) > 0 {
			if _, ok := sub.symbolSet[t.Symbol]; !ok {
				continue
			}
		}
		select {
		case sub.ch <- q:
			// send OK — 만약 slow 상태였으면 리셋 (일시적 폭주 회복 관대 정책).
			sub.firstDropAt.Store(0)
		default:
			sub.dropsLocal.Add(1)
			s.sendDrops.Add(1)
			// 첫 drop 이면 시각 기록 (지속 drop watcher 가 판정).
			sub.firstDropAt.CompareAndSwap(0, time.Now().UnixNano())
		}
	}
	s.subsMu.RUnlock()
}

func (s *AlgoStreamServer) nextSeq(sym string) int64 {
	s.mu.RLock()
	gen, ok := s.seqGens[sym]
	s.mu.RUnlock()
	if ok {
		return int64(gen.Add(1))
	}
	// 첫 등장 — write lock.
	s.mu.Lock()
	if gen, ok = s.seqGens[sym]; !ok {
		gen = &atomic.Uint64{}
		s.seqGens[sym] = gen
	}
	s.mu.Unlock()
	return int64(gen.Add(1))
}

func (s *AlgoStreamServer) ringFor(sym string) *algoRing {
	s.mu.RLock()
	ring, ok := s.rings[sym]
	s.mu.RUnlock()
	if ok {
		return ring
	}
	s.mu.Lock()
	if ring, ok = s.rings[sym]; !ok {
		ring = newAlgoRing(s.ringSize)
		s.rings[sym] = ring
	}
	s.mu.Unlock()
	return ring
}

// SubscribeAlgo — gRPC server 구현. Phase A 는 from_seq=0 (지금부터), Phase B
// 는 from_seq > 0 시 ring 에서 replay (is_backfill=true) 후 live 이어감.
//
// Race-free 순서:
//  1. sub 등록 (mu 잡음) — 이 순간부터 live tick 이 sub.ch 로 push 됨.
//  2. from_seq > 0 이면 심볼별 ring.snapshot(from_seq) 로 replay 슬라이스 획득.
//     - gap 발생 시 FailedPrecondition 반환 (client 는 snapshot 재구독 유도).
//  3. replay 를 client 에 write (is_backfill=true). 이 사이 sub.ch 에는 live
//     tick 이 buffer 로 쌓임.
//  4. live 스트림 시작. 심볼별 replayEndSeq 로 dedup — sub.ch 에서 온 tick 이
//     replay 범위와 겹치면 skip.
func (s *AlgoStreamServer) SubscribeAlgo(req *wtgpb.AlgoSubscribeRequest,
	stream wtgpb.PriceService_SubscribeAlgoServer) error {

	sub := &algoSub{
		clientID:  req.GetClientId(),
		symbolSet: make(map[string]struct{}, len(req.GetSymbols())),
		ch:        make(chan *wtgpb.AlgoQuote, s.clientBufSize),
		done:      make(chan struct{}),
	}
	for _, sym := range req.GetSymbols() {
		if sym != "" {
			sub.symbolSet[sym] = struct{}{}
		}
	}
	// 1) sub 등록 — 이 시점부터 새 tick 은 sub.ch 로 push.
	s.subsMu.Lock()
	s.subs[sub] = struct{}{}
	s.subsMu.Unlock()
	s.subscribersActive.Add(1)

	s.logger.Info("algo subscribe 시작",
		slog.String("client_id", sub.clientID),
		slog.Int("symbols", len(sub.symbolSet)),
		slog.Int64("from_seq", req.GetFromSeq()))

	defer func() {
		s.subsMu.Lock()
		delete(s.subs, sub)
		s.subsMu.Unlock()
		s.subscribersActive.Add(-1)
		s.logger.Info("algo subscribe 종료",
			slog.String("client_id", sub.clientID),
			slog.Uint64("drops_local", sub.dropsLocal.Load()))
	}()

	// 2~3) backfill replay.
	replayEndSeq := map[string]int64{}
	if req.GetFromSeq() > 0 {
		syms := s.replaySymbolList(sub.symbolSet)
		for _, sym := range syms {
			ring := s.ringFor(sym)
			ticks, oldest, gap := ring.snapshot(req.GetFromSeq())
			if gap {
				s.backfillGaps.Add(1)
				s.logger.Warn("algo backfill gap — snapshot 재구독 필요",
					slog.String("client_id", sub.clientID),
					slog.String("symbol", sym),
					slog.Int64("from_seq", req.GetFromSeq()),
					slog.Int64("oldest_available", oldest))
				return status.Errorf(codes.FailedPrecondition,
					"sequence_gap sym=%s from_seq=%d oldest_available=%d — from_seq=0 으로 재구독하여 snapshot 부터 시작",
					sym, req.GetFromSeq(), oldest)
			}
			for _, orig := range ticks {
				// ring 원본을 수정하면 다음 subscribe 에게도 backfill=true 로 보임.
				// 새 message 로 재구성 (proto message 를 value copy 하면 lock 복사).
				q := &wtgpb.AlgoQuote{
					Sym:            orig.GetSym(),
					Bid:            orig.GetBid(),
					Ask:            orig.GetAsk(),
					Seq:            orig.GetSeq(),
					TsSourceUnixNs: orig.GetTsSourceUnixNs(),
					TsWtgUnixNs:    orig.GetTsWtgUnixNs(),
					IsBackfill:     true,
					Last:           orig.GetLast(),
					LastQty:        orig.GetLastQty(),
				}
				if err := stream.Send(q); err != nil {
					return err
				}
				if q.GetSeq() > replayEndSeq[sym] {
					replayEndSeq[sym] = q.GetSeq()
				}
				s.backfillEmitted.Add(1)
			}
		}
	}

	// 4) live 스트림.
	ctx := stream.Context()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-sub.done:
			// Phase C — watcher 가 slow 판정 → 정중히 종료.
			return status.Errorf(codes.Aborted,
				"slow_client_timeout — buffer 폭주 %v 이상 지속. drops=%d. 재접속 시 from_seq 재개 권장",
				s.slowTimeout, sub.dropsLocal.Load())
		case q := <-sub.ch:
			// dedup: replay 이미 한 seq 는 skip. 심볼별 최종 replay seq 이하는
			// 이미 client 가 backfill 로 받았음.
			if end, ok := replayEndSeq[q.GetSym()]; ok && q.GetSeq() <= end {
				s.dedupSkipped.Add(1)
				continue
			}
			if err := stream.Send(q); err != nil {
				return err
			}
		}
	}
}

// replaySymbolList — replay 대상 심볼 리스트. sub.symbolSet 이 있으면 그
// 심볼만, 없으면 (모든 심볼 구독) 현재 ring 등록된 모든 심볼.
func (s *AlgoStreamServer) replaySymbolList(subSet map[string]struct{}) []string {
	if len(subSet) > 0 {
		out := make([]string, 0, len(subSet))
		for sym := range subSet {
			out = append(out, sym)
		}
		return out
	}
	s.mu.RLock()
	out := make([]string, 0, len(s.rings))
	for sym := range s.rings {
		out = append(out, sym)
	}
	s.mu.RUnlock()
	return out
}

// AlgoStats — 진단 스냅샷.
type AlgoStats struct {
	SubscribersActive int64  `json:"subscribers_active"`
	TicksEmitted      uint64 `json:"ticks_emitted"`
	SendDrops         uint64 `json:"send_drops"`
	BackfillEmitted   uint64 `json:"backfill_emitted"`
	BackfillGaps      uint64 `json:"backfill_gaps"`
	DedupSkipped      uint64 `json:"dedup_skipped"`
	DisconnectedSlow  uint64 `json:"disconnected_slow"`
	SymbolsWithRing   int    `json:"symbols_with_ring"`
	RingSize          int    `json:"ring_size"`
	ClientBufSize     int    `json:"client_buffer_size"`
}

func (s *AlgoStreamServer) Stats() AlgoStats {
	s.mu.RLock()
	syms := len(s.rings)
	s.mu.RUnlock()
	return AlgoStats{
		SubscribersActive: s.subscribersActive.Load(),
		TicksEmitted:      s.ticksEmitted.Load(),
		SendDrops:         s.sendDrops.Load(),
		BackfillEmitted:   s.backfillEmitted.Load(),
		BackfillGaps:      s.backfillGaps.Load(),
		DedupSkipped:      s.dedupSkipped.Load(),
		DisconnectedSlow:  s.disconnectedSlow.Load(),
		SymbolsWithRing:   syms,
		RingSize:          s.ringSize,
		ClientBufSize:     s.clientBufSize,
	}
}

// Ensure interface compliance.
var _ TickConsumer = (*AlgoStreamServer)(nil)
var _ wtgpb.PriceServiceServer = (*algoServiceAdapter)(nil)

// algoServiceAdapter — wtgpb.PriceServiceServer 의 나머지 method 는 이 파일에서
// 필요 없음 (본 서버는 SubscribeAlgo 만 구현). 실제 mci-price 에서는
// grpcServer 가 이미 PriceService 를 mount 하고 있고, SubscribeAlgo 도 그
// 서버에서 처리하므로 이 adapter 는 사용되지 않음. 인터페이스 확인용 no-op.
type algoServiceAdapter struct {
	wtgpb.UnimplementedPriceServiceServer
}

// Guard against Context unused warning.
var _ = context.Background
