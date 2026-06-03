# Broker trace_id 전파

WTG 의 HTTP 요청별 X-Request-ID 를 broker frame 의 `mqhdr.trcid[16]` 으로
전파해 broker / 매매 AP 의 로그와 cross-correlation 가능.

코드 참조: `pkg/mymq/types.go` (`Header.TraceID`, `TraceIDSize`),
`pkg/mymq/traceid.go` (TraceIDFromHex/ToHex), `pkg/mymq/frame.go`
(EncodeHeader/DecodeHeader codec),
`internal/api/transform/envelope.go` (BuildFrame propagation).

---

## 1. mqhdr 확장 (84 → 100 byte)

```
mqhdr (100B)
  0..3   msgl       전체 길이
  4..11  func/subc/nvia/dirf/msgf/ctlf/reserved/keyc
  12..19 xchg[8]
  20..35 rkey[16]
  36..39 ckey
  40..43 clid
  44..51 wkey[8]
  52..55 chan[4]
  56..59 errn
  60..79 coff/soff/errm/pkey/nkey
  80..83 body
  ──────────────────────
  84..99 trcid[16]   ← 신규 (W3C tracecontext trace-id 16 byte)
```

### 호환성 (big-bang)

- 84B 옛 클라이언트 ↔ 100B 신규 broker = **wire 길이 불일치로 거부**
- 따라서 **broker / 모든 C AP / WTG Go 동시 deploy** 필수
- broker C struct (`mq.h` mqhdr_t) 끝에 `uint8_t trcid[16]` 추가 — `sizeof(struct mqhdr)` 가 100 으로 자동 갱신, 모든 read/write 영향 자동 반영

## 2. WTG 측 사용

### Header / FrameInput

```go
type Header struct {
    // ... 기존 필드 ...
    TraceID [16]byte  // 신규
}

type FrameInput struct {
    // ...
    TraceID [16]byte  // 명시적으로 채우면 그대로 wire 에
}
```

### 헬퍼

```go
mymq.TraceIDFromHex(s)   // hex 문자열 → [16]byte (짧으면 zero pad)
mymq.TraceIDToHex(t)     // [16]byte → hex (trailing 0 trim)
```

### HTTP middleware → broker call propagation

`internal/api/handlers/transaction.go`:

```go
rid := middleware.RequestIDFromContext(r.Context())
frame, err := env.BuildFrame(0, p.Usid, rid, deps.Routes)
```

`BuildFrame` 이 `mymq.TraceIDFromHex(rid)` 호출해서 자동 변환.

## 3. trace_id 형식

| 출처 | hex 길이 | byte 길이 | 패딩 |
|------|---------|-----------|------|
| WTG X-Request-ID | 16 char | 8 byte | trcid[0..7], 나머지 0 |
| W3C tracecontext trace-id | 32 char | 16 byte | trcid[0..15] |

`TraceIDFromHex` 가 양쪽 호환. `TraceIDToHex` 는 trailing 0 만 trim해서 원래 길이 복원.

## 4. broker 측 동작

broker (`mymqd`) 는 trcid 를 **echo / passthrough** — 라우팅 X.

- request 의 trcid 가 reply 의 trcid 로 그대로 복사 (broker 의 일반 mqhdr 처리 흐름)
- broadcast 시 모든 수신자에게 동일 trcid 전달
- broker 의 audit log 에 trcid 포함 시 cross-service trace 가능 (향후)

## 5. 매매 AP 측 사용

AP layer 의 `content_t` (mq.h) 에 `trcid[16]` 추가 + `mq.h` 의 inline helper
`mq_trcid_hex()` 로 logging.

### content_t 자동 채움

`mq_frame.c` 의 pack/unpack 이 `mqhdr.trcid ↔ content.trcid` 자동 복사:

```c
/* mq_frame.c pack (송신) */
memcpy(mqhdr->trcid, content->trcid, sizeof(mqhdr->trcid));

/* mq_frame.c unpack (수신) */
memcpy(content->trcid, mqhdr->trcid, sizeof(mqhdr->trcid));
```

AP 가 별도 코드 추가 없이 `content->trcid` 사용 가능.

### log 동봉 패턴

`mymq.h` 의 inline helper:

```c
#define MQ_TRCID_HEX_LEN 33     /* 16 byte * 2 hex + NUL */
static inline void mq_trcid_hex(const content_t *content, char *buf);
```

사용 예 (test_service.c):

```c
char trcid_hex[MQ_TRCID_HEX_LEN];
mq_trcid_hex(content, trcid_hex);
if (trcid_hex[0])
    printf("[RECV #%d] trcid=%s ...\n", recv_count, trcid_hex);
```

- trailing 0 byte 자동 trim — 8 byte WTG ID 면 16 char, 16 byte W3C 면 32 char
- 전부 0 (미설정) 이면 빈 문자열 — 호출자가 분기 처리

### 매매 (FC_TRAN) reply 의 trcid

`mymq_reply()` 가 `content->trcid` 를 그대로 reply 의 `mqhdr->trcid` 로 echo
back. AP 는 `content_reset()` 후 `mymq_recv()` 만 호출하면 자동 동작.

### 적용 대상

| AP | log 통합 상태 |
|----|---------------|
| `test/integration/test_service.c` | ✅ (샘플) |
| `trn`, `WECHO`, `W*/BW*` 등 운영 AP | 후속 — 동일 패턴 적용 |

## 6. 운영 흐름

```
사용자 → POST /v1/tx                [X-Request-ID: 0123456789abcdef]
mci-edge-api                         [rid context 주입]
  ↓ proxy + JWT 검증                 [X-Request-ID forward]
mci-api
  ↓ transaction handler              [middleware.RequestIDFromContext]
  ↓ env.BuildFrame(ckey, usid, rid)  [TraceIDFromHex(rid)]
  ↓ mymq.Client.Call                 [wire frame.trcid[0..7] 채움]
broker (mymqd)                       [trcid passthrough]
  ↓ broadcast to 매매 AP
trn                                  [log: trcid=0123456789abcdef]
```

각 service 의 log 에 동일 trcid 등장 → 한 요청의 path 전체 추적 가능.

## 7. 검증

### 단위 테스트 (`pkg/mymq`)

```bash
go test -run='TestTraceID|TestFrameEncodeIncludesTraceID|TestFrameHdrSizeIs100' \
  ./pkg/mymq/
```

- TraceIDFromHex / ToHex round trip (short 8 byte + W3C 16 byte 양쪽)
- 빈 문자열 / 잘못된 hex → zero array
- FrameInput.TraceID → wire 100B → DecodeFrame 라운드트립
- HdrSize == 100 확인

### 통합 테스트 (fakeBroker 라운드트립)

```bash
go test -tags=integration -run='TestClientCallPropagatesTraceID' ./pkg/mymq/
```

- WTG client → wire frame.trcid (broker 수신 검증)
- broker → reply.trcid echo back → WTG decode 검증
- short hex (8 byte) 와 wire 16 byte 매핑 확인

### handler propagation 테스트 (`internal/api/transform`)

```bash
go test -run='TestEnvelopeBuildFrame_TraceID' ./internal/api/transform/
```

- BuildFrame 의 traceIDHex 인자가 frame.TraceID 로 정확히 전달
- 빈 rid 는 zero array

### 라이브 운영 검증

운영 환경에서 매매 1건 후 cross-service log 매칭:

```bash
# 1. test_service 띄움 (또는 운영 AP)
./test_service -e ECHOSVC -r PING

# 2. 매매 요청 + X-Request-ID 캡처
RID=$(curl -is -X POST http://127.0.0.1:8090/v1/tx \
  -H "Authorization: Bearer ..." \
  -H "Content-Type: application/json" \
  -d '{"alias":"WECHO_PING","data":""}' \
  | grep -i "X-Request-ID:" | awk '{print $2}' | tr -d '\r')
echo "rid: $RID"
# 예: 0123456789abcdef

# 3. mci-api log 에서 rid 추적
grep "rid=$RID" /var/log/mci-api/*.log

# 4. AP (test_service) stdout 의 trcid 매칭
# [RECV #N] trcid=0123456789abcdef data="..."
grep "trcid=$RID" /var/log/test_service/*.log

# 5. broker (mymqd) 의 audit log (있으면)
grep "trcid=$RID" /var/log/mymqd/*.log
```

각 service log 에 동일 rid/trcid 등장 → 한 요청의 path 전체 추적.

## 8. 향후

- 매매 AP (trn) 측 trcid 로그 통합
- broker 측 audit log 에 trcid 동봉
- WTG 의 broker call wrapper 가 자동 propagation (현재는 호출자 명시)
- W3C tracecontext (`traceparent` HTTP 헤더) 추가 — `traceparent: 00-<trace-id>-<parent-id>-01`
- OTel SDK 도입 — span 자동 생성/연결
