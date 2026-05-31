package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

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
type grpcPublisher struct {
	conn      *grpc.ClientConn
	client    wtgpb.PriceServiceClient
	stream    wtgpb.PriceService_PublishTickClient
	sendMu    sync.Mutex
	cancel    context.CancelFunc
	closed    atomic.Bool
	logger    *slog.Logger
	accepted  atomic.Uint64
	dropped   atomic.Uint64
	streamErr atomic.Pointer[error]
}

// newGRPCPublisher 는 mci-price 와 PublishTick stream 을 시작한다.
// addr 예: "127.0.0.1:50051". TLS 는 1차 미지원 (사내망 가정).
func newGRPCPublisher(ctx context.Context, logger *slog.Logger, addr string) (*grpcPublisher, error) {
	streamCtx, cancel := context.WithCancel(ctx)
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		cancel()
		return nil, fmt.Errorf("grpc dial %s: %w", addr, err)
	}
	client := wtgpb.NewPriceServiceClient(conn)
	stream, err := client.PublishTick(streamCtx)
	if err != nil {
		cancel()
		_ = conn.Close()
		return nil, fmt.Errorf("PublishTick stream open: %w", err)
	}
	p := &grpcPublisher{
		conn: conn, client: client, stream: stream,
		cancel: cancel, logger: logger,
	}
	go p.recvAckLoop()
	logger.Info("grpc publisher 연결 OK", slog.String("addr", addr))
	return p, nil
}

// recvAckLoop — server 의 PublishAck 를 받아 통계만 갱신. backpressure 시
// stream 자체가 send 측에서 block 또는 error 반환하므로 ack 는 단순 모니터링.
func (p *grpcPublisher) recvAckLoop() {
	for {
		ack, err := p.stream.Recv()
		if err == io.EOF {
			return
		}
		if err != nil {
			p.streamErr.Store(&err)
			if !p.closed.Load() {
				p.logger.Warn("PublishTick stream 끊김", slog.Any("error", err))
			}
			return
		}
		p.accepted.Store(ack.GetAccepted())
		p.dropped.Store(ack.GetDropped())
	}
}

// Publish — envelope batch 1건을 grpc Tick 메시지 N개로 stream send.
// batch 안의 envelope 별로 별도 Tick 으로 보낸다 (mci-price 가 한 Tick 안에서
// ParseEnvelopes 로 다시 split 하지만, broker 와 동일하게 single envelope per Tick
// 이 가장 자연스러움). batch 13~14개 평균 → grpc HTTP/2 multiplexing 으로 처리.
func (p *grpcPublisher) Publish(envs []quote.JSONEnvelope) error {
	if p.closed.Load() {
		return fmt.Errorf("publisher closed")
	}
	p.sendMu.Lock()
	defer p.sendMu.Unlock()
	for _, env := range envs {
		body, err := quote.EncodeJSONEnvelope(env)
		if err != nil {
			return fmt.Errorf("envelope encode: %w", err)
		}
		tick := &wtgpb.Tick{
			Symbol: env.Sym,
			Body:   body,
		}
		if err := p.stream.Send(tick); err != nil {
			return fmt.Errorf("stream send: %w", err)
		}
	}
	return nil
}

func (p *grpcPublisher) Close() error {
	if !p.closed.CompareAndSwap(false, true) {
		return nil
	}
	_ = p.stream.CloseSend()
	p.cancel()
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
