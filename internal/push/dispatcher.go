package push

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync/atomic"

	"github.com/winwaysystems/wtg/pkg/mymq"
)

// Subscriber 는 mymq.Client 의 Subscribe() 를 추상화한 인터페이스.
// *mymq.Client 가 자동으로 만족 — 테스트는 mock 으로 대체 가능.
type Subscriber interface {
	Subscribe() <-chan *mymq.Unsolicited
}

// Dispatcher 는 broker 가 보낸 unsolicited 메시지를 사용자별 fan-out 한다.
//
// 책임:
//   - mymq.Client 의 Subscribe() 채널을 단일 goroutine 으로 소비
//   - broadcast prefix 의 LogonID/Channel 을 보고 Registry 에서 대상 조회
//   - WS envelope (JSON) 으로 직렬화 후 Registry.FanoutToUser
//   - 필터링: FCCast/FCPush/FCSignal 만 처리, 그 외는 metric 카운트만
//
// passthrough 원칙: data 페이로드는 broker 가 보낸 그대로 (raw JSON 또는 string).
// 메시지 종류별 transformer 를 두지 않는다.
// UnsolicitedHook 은 Dispatcher 가 ws Registry fan-out 외에 추가로 호출할
// 다운스트림 (gRPC PushService 등). 각 hook 은 자체 fan-out 책임.
type UnsolicitedHook interface {
	OnUnsolicited(*mymq.Unsolicited)
}

type Dispatcher struct {
	sub      Subscriber
	registry *Registry
	hooks    []UnsolicitedHook
	logger   *slog.Logger

	// ── 메트릭 카운터 (외부에서 조회 가능). 누적 단조증가 ──
	//
	// totalRecv         : Run() 이 채널에서 가져온 모든 메시지.
	// totalDeliver      : 어느 사용자에게든 ws 전송 1+회 성공한 메시지 수
	//                     (sent>0 인 fan-out 1 회당 1 증가, send 횟수는 NOT).
	// totalDrop         : sent=0 인 fan-out — 아래 4 사유의 합.
	//   ├ dropUnsupp    : Func 가 FCCast/FCPush/FCSignal 아님
	//   ├ dropEnvelope  : json marshal 실패
	//   ├ dropUnknownUser: LogonID 명시 됐는데 그 user 의 conn 없음
	//   └ dropNoBroadcast: LogonID 빈값인데 등록된 conn 0
	// sendFailed        : fan-out 안에서 일부 conn 에 send 실패 (slow / closed).
	//                     deliver 와는 별개 — 한 fan-out 이 sent=2 failed=1 이면
	//                     deliver+=1, sendFailed+=1.
	totalRecv       atomic.Uint64
	totalDeliver    atomic.Uint64
	totalDrop       atomic.Uint64
	dropUnsupp      atomic.Uint64
	dropEnvelope    atomic.Uint64
	dropUnknownUser atomic.Uint64
	dropNoBroadcast atomic.Uint64
	sendFailed      atomic.Uint64
}

// DispatcherOptions 는 Dispatcher 구성 의존성.
type DispatcherOptions struct {
	Sub      Subscriber
	Registry *Registry
	Logger   *slog.Logger
}

// NewDispatcher 는 Dispatcher 를 생성한다 (Run 호출 전 까진 idle).
func NewDispatcher(opts DispatcherOptions) *Dispatcher {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &Dispatcher{
		sub:      opts.Sub,
		registry: opts.Registry,
		logger:   opts.Logger,
	}
}

// AddHook 는 추가 다운스트림(gRPC fan-out 등)을 등록.
func (d *Dispatcher) AddHook(h UnsolicitedHook) {
	d.hooks = append(d.hooks, h)
}

// Run 은 Subscribe 채널을 소비하면서 fan-out 한다. ctx 취소 또는 채널 close 시 반환.
//
// 단일 goroutine 으로 호출하면 된다 — 내부에서 별도 goroutine 분기 없음.
func (d *Dispatcher) Run(ctx context.Context) {
	ch := d.sub.Subscribe()
	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-ch:
			if !ok {
				d.logger.Info("subscribe 채널 종료 — Dispatcher 종료")
				return
			}
			d.handle(msg)
		}
	}
}

// handle 은 단일 unsolicited 메시지를 처리한다.
func (d *Dispatcher) handle(msg *mymq.Unsolicited) {
	d.totalRecv.Add(1)

	// FC_CAST / FC_PUSH / FC_SIGNAL 만 처리. 그 외는 unsupported 로 카운트
	// (broker 가 보낸 다른 종류 — heartbeat / control / 등 — 가시화).
	switch msg.Header.Func {
	case mymq.FCCast, mymq.FCPush, mymq.FCSignal:
	default:
		d.dropUnsupp.Add(1)
		d.totalDrop.Add(1)
		return
	}

	// 추가 다운스트림(gRPC PushService 등) 에도 fan-out.
	// 각 hook 은 자체 격리/큐 가지므로 비차단.
	for _, h := range d.hooks {
		h.OnUnsolicited(msg)
	}

	envelope, err := buildEnvelope(msg)
	if err != nil {
		d.logger.Warn("envelope 직렬화 실패",
			slog.Any("error", err),
			slog.Int("body_len", len(msg.Body)),
		)
		d.dropEnvelope.Add(1)
		d.totalDrop.Add(1)
		return
	}

	// LogonID 가 명시된 경우 → 단일 사용자 fan-out.
	// 비어있으면 → 전체 broadcast.
	if msg.Prefix != nil && msg.Prefix.LogonIDString() != "" {
		usid := msg.Prefix.LogonIDString()
		sent, failed := d.registry.FanoutToUser(usid, envelope)
		if failed > 0 {
			d.sendFailed.Add(uint64(failed))
		}
		if sent > 0 {
			d.totalDeliver.Add(uint64(sent))
		} else {
			d.dropUnknownUser.Add(1)
			d.totalDrop.Add(1)
		}
		return
	}

	// LogonID 없는 broadcast — 전체 사용자에게.
	sent, failed := d.registry.FanoutBroadcast(envelope)
	if failed > 0 {
		d.sendFailed.Add(uint64(failed))
	}
	if sent > 0 {
		d.totalDeliver.Add(uint64(sent))
	} else {
		d.dropNoBroadcast.Add(1)
		d.totalDrop.Add(1)
	}
}

// Stats 는 dispatcher 의 누적 카운터를 반환한다 (모니터링/테스트).
//
// Drop 사유는 합산값 Dropped 와 별개로 reason 별로도 노출된다 —
// Dropped == DropUnsupp + DropEnvelope + DropUnknownUser + DropNoBroadcast.
type Stats struct {
	Received        uint64 `json:"received"`
	Delivered       uint64 `json:"delivered"`
	Dropped         uint64 `json:"dropped"`
	DropUnsupp      uint64 `json:"drop_unsupp"`       // Func 가 FCCast/FCPush/FCSignal 아님
	DropEnvelope    uint64 `json:"drop_envelope"`     // json marshal 실패
	DropUnknownUser uint64 `json:"drop_unknown_user"` // LogonID 명시 됐는데 conn 없음
	DropNoBroadcast uint64 `json:"drop_no_broadcast"` // LogonID 빈값 + 등록 conn 0
	SendFailed      uint64 `json:"send_failed"`       // fan-out 내 일부 conn send 실패 (slow/closed)
}

// Stats 는 dispatcher 의 누적 카운터.
func (d *Dispatcher) Stats() Stats {
	return Stats{
		Received:        d.totalRecv.Load(),
		Delivered:       d.totalDeliver.Load(),
		Dropped:         d.totalDrop.Load(),
		DropUnsupp:      d.dropUnsupp.Load(),
		DropEnvelope:    d.dropEnvelope.Load(),
		DropUnknownUser: d.dropUnknownUser.Load(),
		DropNoBroadcast: d.dropNoBroadcast.Load(),
		SendFailed:      d.sendFailed.Load(),
	}
}

// WSEnvelope 는 클라이언트에 전달되는 JSON 메시지 포맷.
//
//	{
//	  "func": 13,         // FC_CAST=4, FC_PUSH=13, FC_SIGNAL=14
//	  "subc": 54,
//	  "exchange": "EXEC",
//	  "channel": "WEB",
//	  "logon_id": "trader01",
//	  "data": { ... } | "raw text"
//	}
type WSEnvelope struct {
	Func     uint8           `json:"func"`
	Subc     uint8           `json:"subc"`
	Exchange string          `json:"exchange,omitempty"`
	Channel  string          `json:"channel,omitempty"`
	LogonID  string          `json:"logon_id,omitempty"`
	Data     json.RawMessage `json:"data,omitempty"`
}

// buildEnvelope 는 Unsolicited 메시지를 클라이언트 JSON envelope 로 직렬화한다.
// data 는 raw JSON 이거나 string-wrapped (broker 가 binary 보낸 경우).
func buildEnvelope(msg *mymq.Unsolicited) ([]byte, error) {
	env := WSEnvelope{
		Func: uint8(msg.Header.Func),
		Subc: uint8(msg.Header.Subc),
	}
	if msg.Prefix != nil {
		env.Exchange = msg.Prefix.ExchangeString()
		env.Channel = msg.Prefix.ChanString()
		env.LogonID = msg.Prefix.LogonIDString()
	}
	if len(msg.Body) > 0 {
		if json.Valid(msg.Body) {
			env.Data = json.RawMessage(msg.Body)
		} else {
			b, err := json.Marshal(string(msg.Body))
			if err != nil {
				return nil, err
			}
			env.Data = b
		}
	}
	return json.Marshal(env)
}
