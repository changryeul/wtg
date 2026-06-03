package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/winwaysystems/wtg/pkg/mymq"
	"github.com/winwaysystems/wtg/pkg/quote"
	wtgpb "github.com/winwaysystems/wtg/pkg/wtgpb/v1"
)

// Publisher 는 envelope batch 1회 send 의 추상화.
//
// 구현체:
//   - brokerPublisher : 기존 동작. broker PRICE exchange 로 publish.
//   - grpcPublisher   : mci-price PublishTick stream 으로 직접 push.
//   - multiPublisher  : broker + grpc 둘 다 (dual-write 진단).
//
// publishOne 안에서 envelope 묶음을 받아 batch encoding + transport 책임.
type Publisher interface {
	Publish(envs []quote.JSONEnvelope) error
	Close() error
}

// ───────── broker publisher (기존 동작) ─────────

type brokerPublisher struct {
	mq *mymq.Client
}

func newBrokerPublisher(mq *mymq.Client) *brokerPublisher {
	return &brokerPublisher{mq: mq}
}

func (p *brokerPublisher) Publish(envs []quote.JSONEnvelope) error {
	var wire []byte
	var err error
	if len(envs) == 1 {
		wire, err = quote.EncodePushdataV1(envs[0])
	} else {
		wire, err = quote.EncodePushdataBatch(envs)
	}
	if err != nil {
		return err
	}
	return publishBroadcast(p.mq, wire)
}

func (p *brokerPublisher) Close() error { return nil } // mq.Close 는 main 에서 별도 처리

// ───────── grpc publisher (broker 우회) ─────────

// grpcPublisher 는 mci-price 의 PublishTick bidi stream 에 envelope 을 send.
// 4 feed worker 가 동시 호출하므로 send 는 sendMu 로 직렬화. ack 는 별도
// goroutine 이 받아 통계 누적.
//
// reconnect supervisor — stream 이 끊기면 (mci-price 재기동 등) 자동 재연결:
//   - Publish 시 send 실패 → stream 상태 streamFailed 로 표시 + 즉시 error 반환 (drop)
//   - supervisor goroutine 이 backoff 로 새 stream 시도 → 성공 시 streamOK 로 복귀
//   - 그 동안 Publish 는 빠르게 drop (sendMu 의 long block 없음 — 운영 latency 보호)
type grpcPublisher struct {
	conn   *grpc.ClientConn
	client wtgpb.PriceServiceClient
	addr   string

	// stream + 그 lifetime context. 재연결 시 둘 다 교체.
	streamMu     sync.Mutex // stream/streamCancel 교체 보호
	stream       wtgpb.PriceService_PublishTickClient
	streamCtx    context.Context
	streamCancel context.CancelFunc
	streamGen    atomic.Uint64 // 재연결 마다 +1 — recvAckLoop 가 본인 stream 만 처리

	sendMu sync.Mutex // 4 feed 의 stream.Send 직렬화. streamMu 와 다름.

	parentCtx    context.Context // 전체 lifetime (외부 ctx)
	parentCancel context.CancelFunc

	logger *slog.Logger
	closed atomic.Bool

	// 상태 flag. atomic CAS 로 동시 호출 분리.
	//   0 = streamOK       (Publish 가능)
	//   1 = streamFailed   (supervisor 가 재연결 대기 중)
	//   2 = reconnecting   (supervisor 가 dial/stream 시도 중)
	status atomic.Int32

	// 통계.
	accepted   atomic.Uint64 // server 가 ack 한 누계 (마지막 stream 의)
	dropped    atomic.Uint64 // server drop 누계
	publishOK  atomic.Uint64 // Publish 성공한 envelope 수
	publishErr atomic.Uint64 // Publish 실패 (drop) envelope 수
	reconnects atomic.Uint64 // 재연결 성공 횟수
}

const (
	statusOK          int32 = 0
	statusFailed      int32 = 1
	statusReconnect   int32 = 2
	reconnectBackoff0       = 1 * time.Second
	reconnectBackoffM       = 30 * time.Second
)

// newGRPCPublisher 는 mci-price 와 PublishTick stream 을 시작한다.
// addr 예: "127.0.0.1:50051". TLS 는 1차 미지원 (사내망 가정).
//
// 초기 connect 실패면 error 반환 — main 이 즉시 알 수 있게. 이후 runtime 에서의
// 연결 끊김은 supervisor 가 자동 복구 (errpath 가 다름).
func newGRPCPublisher(ctx context.Context, logger *slog.Logger, addr string) (*grpcPublisher, error) {
	conn, err := grpc.NewClient(addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithStatsHandler(otelgrpc.NewClientHandler()))
	if err != nil {
		return nil, fmt.Errorf("grpc dial %s: %w", addr, err)
	}
	pctx, pcancel := context.WithCancel(ctx)
	p := &grpcPublisher{
		conn: conn, client: wtgpb.NewPriceServiceClient(conn),
		addr: addr, logger: logger,
		parentCtx: pctx, parentCancel: pcancel,
	}
	if err := p.openStream(); err != nil {
		pcancel()
		_ = conn.Close()
		return nil, fmt.Errorf("PublishTick stream open: %w", err)
	}
	logger.Info("grpc publisher 연결 OK", slog.String("addr", addr))
	go p.supervisor()
	return p, nil
}

// openStream — 새 stream 생성 + recvAckLoop 기동. streamMu 잡고 호출.
// 성공 시 status=OK.
func (p *grpcPublisher) openStream() error {
	streamCtx, cancel := context.WithCancel(p.parentCtx)
	stream, err := p.client.PublishTick(streamCtx)
	if err != nil {
		cancel()
		return err
	}
	gen := p.streamGen.Add(1)
	p.streamMu.Lock()
	p.stream = stream
	p.streamCtx = streamCtx
	p.streamCancel = cancel
	p.streamMu.Unlock()
	p.status.Store(statusOK)
	go p.recvAckLoop(stream, gen)
	return nil
}

// supervisor — status=Failed 감지 시 backoff 로 재연결. closed 면 종료.
func (p *grpcPublisher) supervisor() {
	backoff := reconnectBackoff0
	for {
		if p.closed.Load() {
			return
		}
		// status 가 Failed 가 될 때까지 대기 (가벼운 polling).
		if p.status.Load() != statusFailed {
			select {
			case <-p.parentCtx.Done():
				return
			case <-time.After(200 * time.Millisecond):
				continue
			}
		}
		// Failed → Reconnecting 으로 CAS. 이미 다른 supervisor 가 잡았으면 skip.
		if !p.status.CompareAndSwap(statusFailed, statusReconnect) {
			continue
		}
		p.logger.Info("PublishTick stream 재연결 시도", slog.Duration("backoff", backoff))
		// 이전 stream context cancel — 누수 방지.
		p.streamMu.Lock()
		if p.streamCancel != nil {
			p.streamCancel()
		}
		p.streamMu.Unlock()
		// backoff 대기.
		select {
		case <-p.parentCtx.Done():
			return
		case <-time.After(backoff):
		}
		if err := p.openStream(); err != nil {
			p.logger.Warn("PublishTick 재연결 실패", slog.Any("error", err),
				slog.Duration("next_backoff", minDuration(backoff*2, reconnectBackoffM)))
			p.status.Store(statusFailed) // 다음 cycle 에서 재시도
			backoff = minDuration(backoff*2, reconnectBackoffM)
			continue
		}
		p.reconnects.Add(1)
		p.logger.Info("PublishTick stream 재연결 OK", slog.Uint64("total_reconnects", p.reconnects.Load()))
		backoff = reconnectBackoff0
	}
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

// recvAckLoop — server 의 PublishAck 받아 통계 갱신. stream 끊김 시 status=Failed
// 마킹하고 종료. 새 stream 의 recvAckLoop 가 supervisor 에 의해 다시 시작.
//
// gen 은 본 goroutine 이 처리하는 stream 의 세대. publisher 가 새 stream 으로
// 교체된 후엔 이전 goroutine 이 status 를 잘못 건드리지 않도록 generation 비교.
func (p *grpcPublisher) recvAckLoop(stream wtgpb.PriceService_PublishTickClient, gen uint64) {
	for {
		ack, err := stream.Recv()
		if err == io.EOF {
			return
		}
		if err != nil {
			// 본 stream 이 현 active 면 status=Failed 로 마킹. 이전 stream 의
			// goroutine 이라면 무시.
			if p.streamGen.Load() == gen && !p.closed.Load() {
				p.status.Store(statusFailed)
				p.logger.Warn("PublishTick stream 끊김 — 재연결 대기", slog.Any("error", err))
			}
			return
		}
		p.accepted.Store(ack.GetAccepted())
		p.dropped.Store(ack.GetDropped())
	}
}

// Publish — envelope batch 1건을 grpc Tick 메시지 N개로 stream send.
// status != OK 면 즉시 drop (재연결 동안의 latency block 회피).
func (p *grpcPublisher) Publish(envs []quote.JSONEnvelope) error {
	if p.closed.Load() {
		return fmt.Errorf("publisher closed")
	}
	if p.status.Load() != statusOK {
		p.publishErr.Add(uint64(len(envs)))
		return fmt.Errorf("grpc stream not ready (status=%d)", p.status.Load())
	}
	p.streamMu.Lock()
	stream := p.stream
	p.streamMu.Unlock()
	if stream == nil {
		p.publishErr.Add(uint64(len(envs)))
		return fmt.Errorf("grpc stream nil")
	}

	p.sendMu.Lock()
	defer p.sendMu.Unlock()
	for _, env := range envs {
		body, err := quote.EncodeJSONEnvelope(env)
		if err != nil {
			p.publishErr.Add(1)
			return fmt.Errorf("envelope encode: %w", err)
		}
		tick := &wtgpb.Tick{
			Symbol: env.Sym,
			Body:   body,
		}
		if err := stream.Send(tick); err != nil {
			// stream 실패 — supervisor 가 재연결할 수 있게 status 마킹.
			p.status.Store(statusFailed)
			p.publishErr.Add(uint64(len(envs)))
			return fmt.Errorf("stream send: %w", err)
		}
	}
	p.publishOK.Add(uint64(len(envs)))
	return nil
}

func (p *grpcPublisher) Close() error {
	if !p.closed.CompareAndSwap(false, true) {
		return nil
	}
	p.streamMu.Lock()
	if p.stream != nil {
		_ = p.stream.CloseSend()
	}
	if p.streamCancel != nil {
		p.streamCancel()
	}
	p.streamMu.Unlock()
	p.parentCancel()
	_ = p.conn.Close()
	return nil
}

func (p *grpcPublisher) Stats() (accepted, dropped uint64) {
	return p.accepted.Load(), p.dropped.Load()
}

// ───────── multi publisher (dual-write) ─────────

// multiPublisher 는 envelope 1건을 broker + grpc 둘 다에 보낸다.
// 운영 마이그레이션 진단용 — broker fan-out 결과와 grpc PublishTick 결과를
// mci-price 양쪽에서 받아 비교. 정상 운영에서는 broker/grpc 중 하나만 권장.
type multiPublisher struct {
	pubs []Publisher
}

func newMultiPublisher(pubs ...Publisher) *multiPublisher {
	return &multiPublisher{pubs: pubs}
}

func (p *multiPublisher) Publish(envs []quote.JSONEnvelope) error {
	var firstErr error
	for _, sub := range p.pubs {
		if err := sub.Publish(envs); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (p *multiPublisher) Close() error {
	for _, sub := range p.pubs {
		_ = sub.Close()
	}
	return nil
}

// ───────── 통계 publisher (시간 기반 throttle) — 옵션 ─────────

// _ time 미사용 placeholder — 향후 backpressure metric 추가 시.
var _ = time.Now
