package mymq

import (
	"io"
	"log/slog"
)

// 표준 로깅 키 — slog 의 attr key 컨벤션. 운영팀이 grep / Loki query 시
// 일관된 키로 필터링할 수 있게 한다.
const (
	LogKeyComponent = "component"
	LogKeyApplName  = "appl"
	LogKeyChannel   = "channel"
	LogKeyHost      = "host"
	LogKeyPort      = "port"
	LogKeyConnID    = "scid"
	LogKeyAttempt   = "attempt"
	LogKeyBackoff   = "backoff"
	LogKeyError     = "error"
	LogKeyDuration  = "duration"
	LogKeyHeartbeat = "heartbeat"
)

// 표준 component 값.
const (
	logComponent = "libmymq-go"
)

// discardLogger 는 Options.Logger 미설정 시의 기본값. 모든 로그 무시.
var discardLogger = slog.New(slog.NewTextHandler(io.Discard, nil))

// logger 는 Client 의 logger 를 반환한다 (옵션 미설정 시 discard).
func (c *Client) logger() *slog.Logger {
	if c.opts.Logger != nil {
		return c.opts.Logger
	}
	return discardLogger
}

// logBase 는 모든 로그 라인에 공통으로 붙는 attribute 를 적용한 logger 를 반환한다.
func (c *Client) logBase() *slog.Logger {
	return c.logger().With(
		slog.String(LogKeyComponent, logComponent),
		slog.String(LogKeyApplName, c.opts.effectiveApplName()),
		slog.String(LogKeyChannel, string(c.opts.Channel)),
		slog.String(LogKeyHost, c.host),
		slog.Int(LogKeyPort, c.port),
	)
}

// 외부에서 호출 가능한 logging helper — 구조화된 attr 추가용.
//
// 사용 예: c.logBase().With(slog.Uint64("ckey", 0xDEADBEEF)).Info("call sent")
//
// Logger 미설정 시 모든 로그는 io.Discard 로 흘러간다 — 함수 호출 비용 외엔
// 영향 없음. slog 의 Enabled() 체크는 호출자가 판단하지 않아도 된다.
