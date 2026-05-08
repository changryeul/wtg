package mymq

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// ReconnectOptions 는 자동 재연결 정책.
//
// Options.Reconnect 가 nil 이면 재연결 비활성 — 첫 끊김 시 Client 가
// 영구 종료된다 (1세대 동작). nil 이 아니면 supervisor goroutine 이
// connection 이 끊길 때마다 backoff 후 자동 재연결을 시도한다.
type ReconnectOptions struct {
	// InitialBackoff 는 첫 재연결 시도 전 대기 시간. 기본 1초.
	InitialBackoff time.Duration

	// MaxBackoff 는 backoff 상한. 기본 30초.
	MaxBackoff time.Duration

	// BackoffFactor 는 매 실패마다 backoff 를 곱하는 배율. 기본 2.0.
	BackoffFactor float64

	// MaxAttempts 는 재연결 시도 횟수 상한. 0 = 무제한.
	// 도달하면 Client 가 종료되고 readErr 에 사유 기록.
	MaxAttempts int

	// OnReconnect 는 재연결 성공 후 호출되는 콜백. nil 허용.
	// 호출자는 여기에 사용자 측 재구독 로직을 넣을 수 있다 (예: bind_service
	// 재호출). 콜백은 별도 goroutine 에서 실행되며 Client 메서드 호출 가능.
	OnReconnect func(*Client)

	// OnDisconnect 는 connection 이 끊어진 직후 (재연결 시도 전) 호출. nil 허용.
	OnDisconnect func(*Client, error)
}

// 재연결 관련 sentinel.
var (
	ErrReconnecting       = errors.New("mymq: 재연결 중")
	ErrReconnectExhausted = errors.New("mymq: 재연결 시도 횟수 초과")
)

// effectiveReconnect 는 ReconnectOptions 의 기본값을 채워넣는다.
func (r *ReconnectOptions) effective() ReconnectOptions {
	out := *r
	if out.InitialBackoff <= 0 {
		out.InitialBackoff = 1 * time.Second
	}
	if out.MaxBackoff <= 0 {
		out.MaxBackoff = 30 * time.Second
	}
	if out.BackoffFactor <= 1.0 {
		out.BackoffFactor = 2.0
	}
	return out
}

// supervisorLoop 은 현재 connection 의 doneCh 가 close 되기를 기다렸다가
// 재연결을 시도한다. opts.Reconnect 가 nil 이면 시작되지 않는다.
//
// supervisor 종료 조건:
//   - Close() 호출 (closing flag)
//   - MaxAttempts 도달
//   - 새 connection 의 핸드셰이크 자체가 영구 실패 (예: 잘못된 ApplName)
func (c *Client) supervisorLoop(ctx context.Context) {
	rc := c.opts.Reconnect.effective()
	attempt := 0
	for {
		// 현재 connection 이 끝날 때까지 대기.
		<-c.currentDoneCh()
		if c.closing.Load() {
			return
		}
		readErr := c.lastReadErr()
		c.logBase().With(slog.Any(LogKeyError, readErr)).Warn("connection 끊김 — 재연결 시작")

		if rc.OnDisconnect != nil {
			go rc.OnDisconnect(c, readErr)
		}

		// pending 들에 끊김 통보 (이미 dispatch 에서 처리될 수도 있지만
		// 누락 방지용). 새 connection 으로는 ckey 매칭 불가능하므로 모두 폐기.
		c.failPending(readErr)

		c.reconnecting.Store(true)
		backoff := rc.InitialBackoff

		var lastErr error
		for {
			attempt++
			if rc.MaxAttempts > 0 && attempt > rc.MaxAttempts {
				lastErr = fmt.Errorf("%w (마지막 에러: %v)", ErrReconnectExhausted, lastErr)
				c.storeReadErr(lastErr)
				c.closed.Store(true)
				c.reconnecting.Store(false)
				close(c.subCh) // 영구 종료 — 사용자가 끝났음을 알 수 있게
				return
			}

			select {
			case <-ctx.Done():
				c.storeReadErr(ctx.Err())
				c.closed.Store(true)
				c.reconnecting.Store(false)
				close(c.subCh)
				return
			case <-time.After(backoff):
			}

			if c.closing.Load() {
				return
			}

			c.logBase().With(
				slog.Int(LogKeyAttempt, attempt),
				slog.Duration(LogKeyBackoff, backoff),
			).Info("재연결 시도")

			if err := c.tryReconnect(ctx); err != nil {
				lastErr = err
				c.logBase().With(
					slog.Int(LogKeyAttempt, attempt),
					slog.Any(LogKeyError, err),
				).Warn("재연결 실패")
				// 다음 backoff 단계.
				backoff = nextBackoff(backoff, rc)
				continue
			}
			break // 성공
		}

		c.reconnecting.Store(false)
		attempt = 0 // 성공 시 attempt 카운터 초기화
		c.logBase().With(slog.Uint64(LogKeyConnID, uint64(c.whoamiSc))).Info("재연결 성공")

		if rc.OnReconnect != nil {
			go rc.OnReconnect(c)
		}
	}
}

// tryReconnect 는 단일 재연결 시도를 수행한다. TCP/TLS connect → 핸드셰이크
// → readLoop 재시작. 모두 성공하면 nil, 어느 단계든 실패하면 에러.
func (c *Client) tryReconnect(ctx context.Context) error {
	addr := fmt.Sprintf("%s:%d", c.host, c.port)
	conn, err := dialBroker(ctx, addr, &c.opts)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}

	// 새 connection 으로 client 상태 교체. mutex 보호.
	c.connMu.Lock()
	if c.conn != nil {
		_ = c.conn.Close()
	}
	c.conn = conn
	c.doneCh = make(chan struct{})
	c.readErr.Store(nil) // 새 connection 진단용으로 초기화
	c.lastRecv.Store(0)
	c.lastSent.Store(0)
	c.connMu.Unlock()

	// readLoop 를 새 doneCh 와 함께 시작.
	go c.readLoop()

	// 핸드셰이크 재실행 — Options.Queue 에 따라 자동 분기.
	hsCtx, cancel2 := context.WithTimeout(ctx, c.opts.HandshakeTimeout)
	defer cancel2()
	if c.opts.Queue != nil {
		err = c.connectHandshake(hsCtx)
	} else {
		err = c.handshake(hsCtx)
	}
	if err != nil {
		c.connMu.Lock()
		if c.conn != nil {
			_ = c.conn.Close()
		}
		c.connMu.Unlock()
		return fmt.Errorf("handshake: %w", err)
	}

	// 자동 heartbeat 재시작 (필요 시).
	if interval := c.heartbeatInterval(); interval > 0 {
		go c.heartbeatLoop(interval)
	}
	return nil
}

// failPending 은 현재 in-flight Call 들에 에러를 통보하고 정리한다.
// 재연결 직전에 호출되어 호출자가 무한 대기에 빠지지 않게 한다.
func (c *Client) failPending(cause error) {
	if cause == nil {
		cause = ErrBrokerClosed
	}
	c.pending.Range(func(k, v any) bool {
		if ch, ok := v.(chan *Reply); ok {
			select {
			case ch <- &Reply{Errn: ErrBroker, ErrMsg: cause.Error()}:
			default:
			}
		}
		c.pending.Delete(k)
		return true
	})
}

// nextBackoff 는 지수 backoff 의 다음 단계를 계산한다.
func nextBackoff(cur time.Duration, rc ReconnectOptions) time.Duration {
	next := time.Duration(float64(cur) * rc.BackoffFactor)
	if next > rc.MaxBackoff {
		return rc.MaxBackoff
	}
	return next
}

// ErrBrokerClosed 는 broker connection 이 끊어졌음을 나타낸다.
// pending Call 들의 reply 에 채워져서 호출자에게 전달된다.
var ErrBrokerClosed = errors.New("mymq: broker connection 끊김")
