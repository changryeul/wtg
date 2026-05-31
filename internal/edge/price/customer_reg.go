package price

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"

	wtgpb "github.com/winwaysystems/wtg/pkg/wtgpb/v1"
)

// customerRegManager — Phase 4c. mci-edge-price 측 RegisterCustomer 장기 stream
// 매니저. ws 클라이언트의 connect/disconnect 마다 Register / Unregister 이벤트를
// upstream (mci-price) 에 enqueue → stream 으로 송신.
//
// 책임:
//   - 단일 bidirectional RegisterCustomer stream 유지 + 끊김 자동 재연결.
//   - Register / Unregister 호출자는 큐에 enqueue (non-blocking) 만 하고 즉시 반환.
//   - 재연결 시 현재 활성 등록 set (active) 을 self-heal 로 재등록.
//   - upstream 으로부터 ack 수신 (ok=false 면 warn 로깅, 외 동작 영향 없음).
//
// 운영 가정:
//   - mci-price 가 SIGABRT / restart 해도 edge 는 자동 복구. 짧은 stale 구간은
//     CustomerRegistry 가 다음 tick 까지 비활성 — quote 잠시 안 흐름. ack 미도착
//     상태에서 Register 호출하면 enqueue 만, 실제 등록은 재연결 후.
type customerRegManager struct {
	logger       *slog.Logger
	upstream     *grpc.ClientConn
	subscriberID string

	// active : 현재 등록 의도된 customer set (재연결 self-heal 용).
	// edge 가 ws disconnect 시 Unregister 도 명시 호출 → 여기서 삭제.
	amu    sync.Mutex
	active map[string]string // customerID → profileKey

	// 직렬 송신 큐. 단일 RegisterCustomer stream 의 send 는 직렬화 필요 (gRPC
	// stream 동시 Send 금지).
	queue chan wtgpb.CustomerRegistration

	totalRegistered   atomic.Uint64
	totalUnregistered atomic.Uint64
	totalAckOK        atomic.Uint64
	totalAckErr       atomic.Uint64
	totalReconnects   atomic.Uint64
}

// newCustomerRegManager — 새 매니저. Start() 로 가동.
func newCustomerRegManager(upstream *grpc.ClientConn, subscriberID string, logger *slog.Logger) *customerRegManager {
	if logger == nil {
		logger = slog.Default()
	}
	return &customerRegManager{
		logger:       logger,
		upstream:     upstream,
		subscriberID: subscriberID,
		active:       make(map[string]string),
		queue:        make(chan wtgpb.CustomerRegistration, 1024),
	}
}

// Start — 매니저 가동. ctx 종료까지 stream loop 유지 + 끊김 시 재연결.
//
// 비블로킹. 호출 즉시 반환 — 내부 goroutine 이 처리.
func (m *customerRegManager) Start(ctx context.Context) {
	go m.streamLoop(ctx)
}

// Register — customer 등록 enqueue. ws connect 핸들러에서 호출.
//
// 호출 즉시 active set 에 추가 (재연결 self-heal 용). 실제 stream send 는
// streamLoop 의 다음 send 사이클에서.
func (m *customerRegManager) Register(customerID, profileKey string) {
	if customerID == "" || profileKey == "" {
		return
	}
	m.amu.Lock()
	m.active[customerID] = profileKey
	m.amu.Unlock()
	m.enqueue(wtgpb.CustomerRegistration{
		Op:         wtgpb.CustomerRegistration_OP_REGISTER,
		CustomerId: customerID,
		ProfileKey: profileKey,
	})
}

// Unregister — customer 등록 해제 enqueue. ws disconnect 핸들러에서 호출.
func (m *customerRegManager) Unregister(customerID string) {
	if customerID == "" {
		return
	}
	m.amu.Lock()
	delete(m.active, customerID)
	m.amu.Unlock()
	m.enqueue(wtgpb.CustomerRegistration{
		Op:         wtgpb.CustomerRegistration_OP_UNREGISTER,
		CustomerId: customerID,
	})
}

// enqueue — non-blocking. queue full 이면 drop + warn (재연결 시 active set
// 으로 self-heal 되므로 일부 등록 누락 허용).
func (m *customerRegManager) enqueue(reg wtgpb.CustomerRegistration) {
	select {
	case m.queue <- reg:
	default:
		m.logger.Warn("customerRegManager: queue 가득 — drop",
			slog.String("op", reg.GetOp().String()),
			slog.String("customer_id", reg.GetCustomerId()),
		)
	}
}

// streamLoop — RegisterCustomer stream 유지 + 끊김 시 재연결 (exponential
// backoff up to 10s).
func (m *customerRegManager) streamLoop(ctx context.Context) {
	client := wtgpb.NewPriceServiceClient(m.upstream)
	backoff := 500 * time.Millisecond
	for {
		if ctx.Err() != nil {
			return
		}
		err := m.streamOnce(ctx, client)
		if errors.Is(err, context.Canceled) || ctx.Err() != nil {
			return
		}
		m.totalReconnects.Add(1)
		m.logger.Warn("RegisterCustomer stream 끊김 — 재시도",
			slog.Any("error", err),
			slog.Duration("backoff", backoff),
		)
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

// streamOnce — 단일 RegisterCustomer stream lifecycle.
//
// 흐름:
//   1. NewStream → 활성 active set 으로 self-heal 재등록.
//   2. recv goroutine 가동 (ack 수신 + 로깅).
//   3. send loop — queue 에서 꺼내 stream.Send.
//   4. ctx 종료 또는 stream 오류 시 종료.
func (m *customerRegManager) streamOnce(ctx context.Context, client wtgpb.PriceServiceClient) error {
	stream, err := client.RegisterCustomer(ctx)
	if err != nil {
		return err
	}
	m.logger.Info("RegisterCustomer stream 시작",
		slog.String("subscriber_id", m.subscriberID))

	// self-heal: 재연결 후 active set 의 모든 entry 를 다시 register 로 enqueue.
	m.amu.Lock()
	healCount := 0
	for cid, pkey := range m.active {
		select {
		case m.queue <- wtgpb.CustomerRegistration{
			Op:         wtgpb.CustomerRegistration_OP_REGISTER,
			CustomerId: cid,
			ProfileKey: pkey,
		}:
			healCount++
		default:
		}
	}
	m.amu.Unlock()
	if healCount > 0 {
		m.logger.Info("RegisterCustomer self-heal", slog.Int("count", healCount))
	}

	// recv goroutine — ack 수신.
	recvDone := make(chan error, 1)
	go func() {
		for {
			ack, err := stream.Recv()
			if err == io.EOF {
				recvDone <- nil
				return
			}
			if err != nil {
				recvDone <- err
				return
			}
			if ack.GetOk() {
				m.totalAckOK.Add(1)
			} else {
				m.totalAckErr.Add(1)
				m.logger.Warn("RegisterCustomer ack 실패",
					slog.String("customer_id", ack.GetCustomerId()),
					slog.String("error", ack.GetError()),
				)
			}
		}
	}()

	// send loop.
	for {
		select {
		case <-ctx.Done():
			_ = stream.CloseSend()
			<-recvDone
			return ctx.Err()
		case err := <-recvDone:
			return err
		case reg := <-m.queue:
			if err := stream.Send(&reg); err != nil {
				_ = stream.CloseSend()
				return err
			}
			switch reg.GetOp() {
			case wtgpb.CustomerRegistration_OP_REGISTER:
				m.totalRegistered.Add(1)
			case wtgpb.CustomerRegistration_OP_UNREGISTER:
				m.totalUnregistered.Add(1)
			}
		}
	}
}

// Stats — 매니저 누적 카운터.
type customerRegStats struct {
	Registered   uint64 `json:"registered"`
	Unregistered uint64 `json:"unregistered"`
	AckOK        uint64 `json:"ack_ok"`
	AckErr       uint64 `json:"ack_err"`
	Reconnects   uint64 `json:"reconnects"`
	ActiveCount  int    `json:"active"`
}

func (m *customerRegManager) Stats() customerRegStats {
	m.amu.Lock()
	active := len(m.active)
	m.amu.Unlock()
	return customerRegStats{
		Registered:   m.totalRegistered.Load(),
		Unregistered: m.totalUnregistered.Load(),
		AckOK:        m.totalAckOK.Load(),
		AckErr:       m.totalAckErr.Load(),
		Reconnects:   m.totalReconnects.Load(),
		ActiveCount:  active,
	}
}
