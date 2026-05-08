package admin

import (
	"context"
	"errors"

	"github.com/winwaysystems/wtg/pkg/mymq"
)

// anyCaller 는 admin / api/handlers 양쪽 Caller 인터페이스를 동시에 만족시키는
// alias — 두 인터페이스가 같은 시그니처라 같은 객체로 주입 가능.
type anyCaller interface {
	Call(ctx context.Context, in *mymq.FrameInput) (*mymq.Reply, error)
}

// errNoBroker — --no-broker 모드에서 broker 호출 시도 시 반환되는 에러.
//
// 호출자 (mapBrokerError) 가 ErrReconnecting 과 동일하게 503 으로 매핑하도록
// errors.Is(err, mymq.ErrReconnecting) 을 통해 알아본다 — 별도 sentinel 추가
// 비용을 피하기 위해 mymq.ErrReconnecting 을 그대로 wrapping.
var errNoBroker = errors.New("broker 미연결 (--no-broker 모드)")

// unavailableCaller — Call 호출 시 항상 ErrReconnecting wrapped 에러 반환.
// 핸들러는 503 (broker_unavailable) 응답을 그대로 클라이언트에게 전달.
type unavailableCaller struct{}

func (unavailableCaller) Call(_ context.Context, _ *mymq.FrameInput) (*mymq.Reply, error) {
	return nil, errors.Join(mymq.ErrReconnecting, errNoBroker)
}
