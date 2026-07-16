package quote

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// JSONEnvelope 는 cooker / quote-forwarder 등 시세 producer 가 broker 의
// pushdata.msgb 안에 wire 페이로드로 싣는 평면 envelope (v1).
//
// 정식 스펙: docs/cooker-quote-schema.md
//
// mci-price 의 Aggregator 가 이 envelope 을 parsing 해 bid/ask 를 추출한다.
// Sym 필드는 SymbolMap 으로 session.Pair 로 정규화하는 단계에서 사용된다.
type JSONEnvelope struct {
	Sym string    `json:"sym"`
	Bid float64   `json:"bid"`
	Ask float64   `json:"ask"`
	TS  time.Time `json:"ts"`
	Src string    `json:"src,omitempty"`
	Seq uint64    `json:"seq,omitempty"`
	// Last 는 시장 체결가 (FIX MDEntryType=2 Trade → mds fillprc 대응). optional —
	// bid/ask 와 같은 메시지에 체결이 함께 온 경우에만 채워진다. 0 이면 생략.
	// mds MDFOLD.fillprc 와 달리 forwarder 는 stateless 라 "이 tick 의 체결가" 만
	// 담고, 최근값 유지(persist)는 BestConsumer 가 담당.
	Last float64 `json:"last,omitempty"`
	// LastQty 는 체결 수량 (FIX 271 MDEntrySize). USD/KRW·CNH/KRW 는 항상 0.
	LastQty float64 `json:"last_qty,omitempty"`
}

// 디코딩 에러.
var (
	ErrEnvelopeEmpty         = errors.New("quote: 빈 envelope")
	ErrEnvelopeInvalidBidAsk = errors.New("quote: bid/ask 부적합 (양수 + ask>=bid 필요)")
	ErrEnvelopeMissingSym    = errors.New("quote: sym 필드 누락")
)

// DecodeJSONEnvelope 는 wire bytes 를 JSONEnvelope 로 파싱·검증한다.
//
// 검증:
//   - sym 비어있지 않을 것
//   - bid > 0, ask > 0, ask >= bid
//
// ts 의 zero 값은 호출자 책임으로 처리 (필드 누락은 zero-time 으로 반환).
func DecodeJSONEnvelope(body []byte) (JSONEnvelope, error) {
	var env JSONEnvelope
	if len(body) == 0 {
		return env, ErrEnvelopeEmpty
	}
	// broker 가 fixed buffer 안에 NUL padding 을 남기는 경우가 있어 trailing NUL 정리.
	body = bytes.TrimRight(body, "\x00")
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		return env, ErrEnvelopeEmpty
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return env, fmt.Errorf("quote: JSON 파싱: %w", err)
	}
	if env.Sym == "" {
		return env, ErrEnvelopeMissingSym
	}
	if env.Bid <= 0 || env.Ask <= 0 || env.Ask < env.Bid {
		return env, ErrEnvelopeInvalidBidAsk
	}
	return env, nil
}

// EncodeJSONEnvelope 는 JSONEnvelope 을 wire bytes 로 직렬화한다.
//
// 운영에서는 cooker (C) 가 publish 측이므로 이 함수는 주로 테스트 / 시뮬레이터 /
// quote-forwarder 의 평면 envelope 출력에 사용된다.
func EncodeJSONEnvelope(env JSONEnvelope) ([]byte, error) {
	return json.Marshal(env)
}
