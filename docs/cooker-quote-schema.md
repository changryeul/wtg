# Cooker Quote JSON 스키마 (v1)

cooker → broker → mci-price 의 wire 페이로드 (broker pushdata.msgb) 안에 들어가는
JSON envelope. mci-price.Aggregator 가 마진 적용 직전의 raw quote 를 얻기 위한
**단일 표준**이다.

## 설계 원칙

| 원칙 | 이유 |
|---|---|
| **평면 (flat) 구조** — bid/ask 직접 필드 | Aggregator 가 그대로 사용. FIX 의 multi-entry 파싱 부담 X |
| **JSON** | debuggability ↑, 확장성 ↑. 100tps × ~150B = 15KB/sec — 성능 무관 |
| **UTC ISO-8601 시각** | 모든 봉 경계가 UTC, 시간대 혼선 X |
| **확장 필드 옵션** | seq/src 등 감사용 필드 추가 가능. mci-price 는 무시 |

## 스키마 (v1)

```json
{
  "sym": "USDKRW",
  "bid": 1399.50,
  "ask": 1399.60,
  "ts":  "2026-05-23T03:21:45.123Z",
  "src": "COOKER",
  "seq": 1234567
}
```

### 필드

| 필드 | 타입 | 필수 | 설명 |
|---|---|---|---|
| `sym` | string | ✓ | 통화쌍의 외부 표기. SymbolMap 에서 `session.Pair` 로 정규화 (예: "USDKRW" → "USD/KRW"). |
| `bid` | float64 | ✓ | 매수호가 (고객이 base 통화를 팔 때 받는 가격). > 0. |
| `ask` | float64 | ✓ | 매도호가 (고객이 base 통화를 살 때 내는 가격). > 0, ≥ bid. |
| `ts`  | string | ✓ | RFC3339 (또는 RFC3339Nano). UTC 권장 — 타임존 명시 필수. |
| `src` | string | optional | 출처 식별 (예: "COOKER", "FIX_W", "FIX_X"). 감사/디버깅 용. |
| `seq` | uint64 | optional | 출처별 시퀀스. 멀티 인스턴스 dedup / 누락 탐지 용. |

### 검증 규칙 (mci-price 가 적용)

- `bid > 0` AND `ask > 0` AND `ask >= bid` — 조건 미달 시 tick drop
- `sym` SymbolMap 에 없으면 drop
- SymbolMap entry 의 `active == false` 면 drop
- `ts` 파싱 실패 시 drop (필드 누락은 zero-time 으로 fallback — 디코더가 결정)

### 예시

**정상 (단순)**
```json
{"sym":"USDKRW","bid":1399.50,"ask":1399.60,"ts":"2026-05-23T03:21:45.123Z"}
```

**감사 풍부**
```json
{
  "sym": "EURKRW",
  "bid": 1500.10,
  "ask": 1500.25,
  "ts": "2026-05-23T03:21:45.456Z",
  "src": "COOKER",
  "seq": 7891234
}
```

**reject 사례**
```json
// ask < bid — drop
{"sym":"USDKRW","bid":1400,"ask":1399,"ts":"..."}

// 음수 — drop
{"sym":"USDKRW","bid":-1,"ask":1,"ts":"..."}

// 미등록 심볼 — drop
{"sym":"XAUUSD","bid":2000,"ask":2001,"ts":"..."}
```

## C 측 (cooker) 작성 가이드

C 에서는 `snprintf` 한 줄로 생성 가능:

```c
char body[256];
int n = snprintf(body, sizeof(body),
    "{\"sym\":\"%s\",\"bid\":%.5f,\"ask\":%.5f,\"ts\":\"%s\",\"src\":\"COOKER\",\"seq\":%llu}",
    sym, bid, ask, iso_ts_utc, seq);
// n 을 pushdata.pushmsg.msgl 에 채우고 body 를 msgb 로 publish.
```

소수점 정밀도는 통화쌍에 따라 다르지만 통상 5자리 (`%.5f`) 권장. JPY 쌍 등
정수 단위 시세는 `%.3f` 도 가능.

## Go 측 (mci-price) 소비

```go
// pkg/quote/codec.go
type JSONEnvelope struct {
    Sym string    `json:"sym"`
    Bid float64   `json:"bid"`
    Ask float64   `json:"ask"`
    TS  time.Time `json:"ts"`
    Src string    `json:"src,omitempty"`
    Seq uint64    `json:"seq,omitempty"`
}

func DecodeJSONEnvelope(body []byte) (JSONEnvelope, error)
```

`Aggregator` 는 `internal/price/cookerdecoder.go` 의 `JSONCookerDecoder()` 를
주입해 `CookerBodyDecoder` 시그니처로 변환한 뒤 사용.

## 마이그레이션 노트

### 현재 상태
- `cooker` (C, mymq 엔진 측) — wire 포맷 미정. 이 문서 v1 으로 합의 권장.
- `quote-forwarder` (Go, UDP FIX 4.4 → broker publish) — 이미 FIX 구조 JSON 출력
  (`quoteEnvelope { ts, feed, seq, msgtype, symbol, entries: [{type, px, qty}] }`).

### 권장 마이그레이션 경로

**Phase A (지금)**
- cooker 가 v1 평면 envelope publish 시작 → mci-price 가 정상 처리
- quote-forwarder 는 그대로 FIX envelope (별도 consumer 가 사용)

**Phase B (선택)**
- quote-forwarder 에 `--flatten` 옵션 추가 → bid/offer entry 를 v1 평면 envelope 로 변환 publish
- 같은 ExchangePrice 큐에 평면 envelope 가 두 출처에서 들어와도 형태 동일 → mci-price 무차별 처리
- 기존 FIX envelope 는 별도 exchange / 큐로 분리 (관심사 분리)

**Phase C (장기)**
- quote-forwarder 가 평면 envelope 만 emit
- FIX 디버깅 필요시 별도 sniff 도구로 직접 UDP 캡처

## 운영 합의 체크리스트

cooker 팀과 합의 시 확인할 것:

- [ ] 위 v1 스키마 OK
- [ ] `sym` 의 표기 컨벤션 (붙임/구분자 — "USDKRW" vs "USD/KRW" vs "USD_KRW")
  - mci-price 의 SymbolMap 이 변환하므로 cooker 표기는 자유 — **합의된 한 가지로 통일**.
- [ ] `ts` 포맷 — RFC3339 / RFC3339Nano (밀리초 권장)
- [ ] `bid`/`ask` 소수점 정밀도 (통화쌍별)
- [ ] `seq` 필드 사용 여부 (있으면 누락 탐지에 활용)
- [ ] 최대 페이로드 크기 ≤ 1512 바이트 (pushmsg.msgb 한도)
  - v1 평균 페이로드 ~120 바이트 → 한도의 8% 사용. 여유 큼.

## 향후 v2 후보 (현재 미반영)

- `mid` 필드 — 사전 계산된 mid price (소비자 편의)
- `tenor` 필드 — forward 시세 만기
- `spread_hint` 필드 — cooker 가 제안하는 분기 등급
- `signature` 필드 — HMAC 같은 변조 방지
