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

대부분의 운영 AP 는 **공통 main 루프** (`win/src/lib/db2stub/dev_main.c`) 사용 —
한 곳 적용으로 58개 AP 전체 자동 반영.

| AP / 위치 | log 통합 상태 | 비고 |
|----------|---------------|------|
| `mymq/test/integration/test_service.c` | ✅ | 샘플 (mymq 패키지) |
| `win/src/lib/db2stub/dev_main.c` | ✅ | **공통 main** — `[dev_main] received rkey=[...] trcid=...` |
| `win/src/trn/WECHO/WECHO.c` | ✅ | 자체 main — 에러 시 trcid 동봉 |
| `win/src/trn/W1100`~`W3300` (57개) | ✅ (간접) | `dev_main` 통과 |
| `win/src/trn/WECHOSTD/WECHOSTD.c` | ✅ (간접) | 핸들러만 정의 → `dev_main` 통과 |
| broker (`mymqd`) audit log | ⏳ 후속 | broker 자체 log 에 trcid 동봉 (옵션) |

## 6. 운영 흐름

### 6.1 W3C tracecontext (권장 — 전체 16B 사용)

```
사용자 → POST /v1/tx                 [traceparent: 00-<32hex>-<16hex>-01]
mci-edge-api
  ↓ proxy + JWT 검증                 [traceparent forward]
mci-api
  ↓ middleware.RequestID 가 처리      [TraceParent context 주입]
  ↓ transaction handler              [TraceIDHexFromContext (32 hex)]
  ↓ env.BuildFrame(..., 32hex)       [TraceIDFromHex → trcid[0..15] 전체]
broker (mymqd)                       [trcid passthrough]
  ↓ broadcast
trn                                  [log: trcid=<32hex>]
```

### 6.2 X-Request-ID 폴백 (호환성 — 8B만)

```
사용자 → POST /v1/tx                 [X-Request-ID: 0123456789abcdef]
mci-edge-api
  ↓ proxy                            [X-Request-ID forward + 새 traceparent 생성]
mci-api
  ↓ middleware.RequestID              [trace_id 새로 생성: 앞 8B = 입력 X-Request-ID]
  ↓ transaction handler              [TraceIDHexFromContext (32 hex)]
  ↓ env.BuildFrame(..., 32hex)       [trcid[0..15] — 앞 8B 는 사용자 입력 보존]
```

### 6.3 W3C / RequestID 비교

| 측면 | X-Request-ID 8B | W3C traceparent 16B |
|------|-----------------|----------------------|
| wire | 16 hex char | `00-<32hex>-<16hex>-<flags>` |
| broker frame trcid | `[0..7]` 만 | `[0..15]` 전체 |
| span tree 표현 | X (단일 ID) | parent-id 로 가능 |
| OTel / Jaeger 호환 | X | ✅ |
| 운영 backward compat | ✅ (legacy 도구) | 신규 |

middleware 가 **둘 다 자동 처리** — 호출자는 차이 인식 X.

각 service 의 log 에 동일 trace_id 등장 → 한 요청의 path 전체 추적 가능.

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

## 8. OTel SDK + span 발행 (PR 2)

W3C tracecontext 의 trace_id 가 정해진 후 (PR 1) 외부 backend (Jaeger /
Tempo / Datadog / Honeycomb) 에 span tree 발행하려면 OTel SDK 도입.

### 8.1 운영 활성

```bash
mci-api \
  --otel-endpoint otel-collector:4317 \
  --otel-insecure \
  --otel-sample 0.1

mci-edge-api \
  --otel-endpoint otel-collector:4317 \
  --otel-insecure
```

flag 비면 비활성 (span 미수집). `--otel-stdout` 으로 stdout 출력 (debug).

### 8.2 자동 propagation

W3C tracecontext propagator 가 등록되어 — 외부 client → mci-edge-api →
mci-api 의 traceparent 헤더가 자동 trace tree 로 연결.

### 8.3 현재 발행되는 span

| service | span | attributes |
|---------|------|------------|
| mci-api | `broker.call` | broker.xchg, broker.rkey, broker.usid |

후속 (PR 3): edge-api proxy, gRPC, etcd watch, Redis call.

### 8.4 sampling

운영 권장 `--otel-sample 0.01..0.1` (1~10%). dev 1.0 (전체).
`ParentBased(TraceIDRatioBased(ratio))` — 외부 client 가 sampled bit
설정한 trace 는 100% 수집.

## 9. wire codec 운영 — image rebuild + win 측 동기화

wire schema (mqhdr 100B + trcid) 변경 시 양쪽 합의 + big-bang deploy.

### 9.1 변경 대상 파일

| repo / 파일 | 역할 |
|-------------|------|
| `mymq/src/inc/mq.h` (line 215) | wire mqhdr `trcid[16]` 필드 |
| `mymq/src/inc/mymq.h` (line 111) | content `trcid[16]` + `MQ_TRCID_HEX_LEN` + `mq_trcid_hex()` |
| `mymq/src/lib/mymq/mq_frame.c` | wire encode/decode 시 trcid 복사 |
| `win/src/inc/com/mymq.h` | AP 측 content 동일 필드 + helper (운영 trn 빌드 시 노출) |

`win/src/inc/com/mymq.h` 는 fork (`mymq/src/inc/mymq.h`) 와 **ABI 일치 검증
필수** — 같은 offset (`trcid` @ 260, `content_t` size 608).

### 9.2 image rebuild 절차

```bash
# 1) mymq fork → wtg-mymqd:amd64
cd ~/mywork/mymq
docker build --platform linux/amd64 \
  --build-context win=~/mywork/win \
  -f scripts/Dockerfile.runtime \
  -t wtg-mymqd:amd64 .

# 2) (선택) win-builder image — Oracle Pro*C 빌드 시
cd ~/mywork/win/docker
docker compose build win-builder

# 3) broker container 교체
docker stop mymqd && docker rm mymqd
docker run --rm -d --platform linux/amd64 -p 11217:11217 --name mymqd wtg-mymqd:amd64
```

### 9.3 검증

```bash
# mqhdr 크기 100 확인 (옛 빌드면 84)
docker run --rm wtg-mymqd:amd64 sh -c '
cat > /tmp/sz.c << EOF
#include <stdio.h>
#include <stddef.h>
#include <mq.h>
int main(){printf("%zu\n", sizeof(mqhdr_t));return 0;}
EOF
gcc -I/opt/mymq/include /tmp/sz.c -o /tmp/sz && /tmp/sz'
# 기대: 100
```

### 9.4 주의 — Oracle Instant Client precomp

2026-06 시점 Oracle 카탈로그 개편으로 `oracle-instantclient-precomp`
(Pro*C precompiler) 패키지 제거. `win-builder` Dockerfile 은 본 패키지를
빼고 `oracle-instantclient-tools` 만 설치. Pro*C 가 필요한 sqc 빌드는 별도
zip 다운로드 (oracle.com 무료 라이선스). DB-free 빌드 (db2stub +
sqc_strip) 트랙은 영향 없음.

## 10. e2e 검증 (실 broker 라이브)

### 10.1 정상 흐름

```bash
# broker + WECHO 자동 entrypoint
docker run --rm -d --platform linux/amd64 -p 11217:11217 --name mymqd wtg-mymqd:amd64

# mci-api 띄움 (단일 인스턴스 — port 충돌 주의)
mci-api --dev --broker-host=127.0.0.1 --broker-port=11217 --listen=:8080

# traceparent 박은 /v1/tx
curl -is -X POST http://127.0.0.1:8080/v1/tx \
  -H "X-WTG-User: alice" \
  -H "traceparent: 00-deadbeef0123456789abcdef01234567-1122334455667788-01" \
  -H "Content-Type: application/json" \
  -d '{"alias":"WECHO_PING","data":""}'

# 기대 응답:
#   HTTP/1.1 200 OK
#   Traceparent: 00-deadbeef0123456789abcdef01234567-1122334455667788-01
#   X-Request-Id: deadbeef01234567
#   {"data":"PONG"}
```

### 10.2 image 안 W1100 + dev_main 검증

```bash
# image 안 W1100 + dev_main IO 표준화 실 동작
docker exec -d mymqd sh -c \
  'DEV_MAIN_LOG=info /opt/mymq/bin/W1100 -h 127.0.0.1:11217 -e WTRN -n W1100 > /tmp/w1100.log 2>&1'

# 호출
docker exec mymqd /opt/mymq/bin/test_client \
  -h 127.0.0.1:11217 -n W1T -e WTRN -r W1101S01 -m call "TEST"

# dev_main log 확인
docker exec mymqd cat /tmp/w1100.log
# [dev_main] evt=recv rkey=[W1101S01] len=4 trcid=-
# [dev_main] evt=reply rkey=[W1101S01] trcid=- ret=0 lat_us=20 sndl=0

# SIGUSR1 → 통계 dump
docker exec mymqd sh -c 'kill -USR1 $(pgrep -f W1100)'
docker exec mymqd cat /tmp/w1100.log | tail -3
# [dev_main] === stats dump pid=NN ===
# [dev_main]   rkey=W1101S01 count=K err=0 avg_us=NNN max_us=NNN
```

### 10.3 디버깅 — port 충돌 진단

`/v1/tx` 가 즉시 `503 reconnecting` + `time<1ms` 반환하면 **이미 같은 port
의 다른 mci-api 인스턴스가 떠 있어 backoff 상태일 가능성**:

```bash
pgrep -fl mci-api
# 옛 인스턴스가 있으면 kill PID
```

### 10.4 wire frame 디버그 (필요 시)

`pkg/mymq/client.go` 의 `readLoop` 에 임시 `WTG_MYMQ_FRAME_DEBUG=1` env
hook 을 추가하면 모든 frame 의 `func/subc/ckey/errn/body_len` stderr 출력.
A3 진단 사례에서 이 패턴으로 root cause (port 충돌 → 옛 인스턴스의 무한
backoff) 식별. 진단 후 패치 revert 권장.

## 11. 향후

- 매매 AP (trn) 측 trcid 로그 통합 (운영 W3xxx 추가 적용)
- broker 측 audit log 에 trcid 동봉
- WTG 의 broker call wrapper 가 자동 propagation (현재는 호출자 명시)
- 추가 instrumentation — gRPC / etcd watch / Redis (PR 3)
- Jaeger/Tempo backend 운영 가이드
