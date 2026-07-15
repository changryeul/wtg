# cs framework → mci-edge-price WS 마이그레이션 가이드

legacy cs client (HTS/EMP, Visual C++) 가 `mymqd broker PRICE subscribe` 에서
`mci-edge-price WS` 로 전환하는 단계별 안내. broker 의 시세 fan-out 부하를
영구적으로 분리해서 broker 가 매매 transaction RPC 에만 집중하도록 한다.

## 1. 마이그레이션 목적

**Before** (legacy):
```
cs client  ─TCP mymq frame─→  mymqd  ─PRICE FANOUT─→  cs receives broadcast
                              (broker fan-out 부담 + SIGABRT 위험)
```

**After** (현 architecture):
```
cs client  ─WebSocket─→  mci-edge-price :8089
                            │
                            └─ Internal: gRPC ←─ mci-price (BestConsumer)
                                                   ↑
                                              broker 미경유
```

## 2. 변경 요약

| 영역 | Before | After |
|------|--------|-------|
| 연결 endpoint | `mymqd-host:11217` | `mci-edge-price-host:8089` |
| protocol | mymq TCP frame (84B mqhdr + body) | WebSocket (HTTP/1.1 upgrade + ws frame) |
| 인증 | broker handshake (DECLARE_SESSION) | URL query `?x_wtg_user=<id>` (dev) / `?access_token=<JWT>` (운영) |
| subscribe | broker connect 의 ExchangeName="PRICE" | ws connect 후 control message |
| envelope schema | `{ts, feed, seq, msgtype, symbol, entries:[...]}` | **동일** (legacy 옵션 사용 시) |
| TLS | broker TLS 옵션 | `wss://` (운영) |

**핵심** — envelope schema 가 broker subscribe 시 받던 그대로. cs 의 parser
코드 변경 없이 transport (mymq → ws) 만 교체.

## 3. WTG 측 endpoint 설정 (운영팀)

### 3.1 wtgctl 자동 기동 (권장)

`wtgctl` (user-local `~/mymq/bin/wtgctl`) 에 `WTG_EDGE_LEGACY=1` env 처리 추가:

```bash
WTG_EDGE=1 WTG_EDGE_LEGACY=1 wtgctl start
#   → mci-edge-price       :8083  (best, 신규 client 용)
#   → mci-edge-price-legacy :8089  (legacy cs, broker subscribe schema 1:1)
```

같은 mci-price 를 upstream 으로 공유. 자원 부담 거의 없음 (각각 별도 grpc
subscriber 로 등록 — subscriber_id: `mci-edge-price@host` vs `mci-edge-price-legacy`).

wtgctl 의 status 표에도 `mci-edge-price-legacy :8089` 표시됨.

### 3.2 직접 기동 (수동 / 운영 systemd)

```bash
# legacy cs 전용 인스턴스 — 별도 포트로 분리
./build/bin/mci-edge-price \
    --listen :8089 \
    --upstream 127.0.0.1:50051 \
    --envelope-format=legacy \
    --subscriber-id=mci-edge-price-legacy \
    --log-level=info
```

또는 환경변수:
```bash
WTG_EPRICE_ENVELOPE_FORMAT=legacy ./build/bin/mci-edge-price --listen :8089 ...
```

기존 `:8083` (best format) 인스턴스는 그대로 유지. 두 인스턴스가 같은 mci-price
를 upstream 으로 공유 — 자원 부담 거의 없음.

## 4. WebSocket 연결 절차 (cs client)

### 4.1 URL 구성

```
운영:  wss://edge-price.example.com:8089/v1/subscribe?access_token=<JWT>
DevMode: ws://10.0.0.10:8089/v1/subscribe?x_wtg_user=<usid>
```

브라우저 WebSocket API 처럼 cs 도 헤더 첨부가 어려운 경우 query 로 인증
전달. mci-edge-price 의 `BearerFromQuery` / `UserFromQuery` 미들웨어가
`Authorization: Bearer <JWT>` 또는 `X-WTG-User: <id>` 헤더로 변환.

### 4.2 WebSocket handshake (HTTP/1.1 upgrade)

```
GET /v1/subscribe?x_wtg_user=alice HTTP/1.1
Host: 10.0.0.10:8089
Upgrade: websocket
Connection: Upgrade
Sec-WebSocket-Version: 13
Sec-WebSocket-Key: <base64 16-byte random>
Origin: http://cs-client.internal     ← 사내망 origin (서버가 DevMode 면 *)
```

서버 응답:
```
HTTP/1.1 101 Switching Protocols
Upgrade: websocket
Connection: Upgrade
Sec-WebSocket-Accept: <base64 hash>
```

### 4.3 subscribe control message

연결 직후 client 가 보낼 메시지 (텍스트 ws frame):
```json
{"type":"subscribe","pairs":["USD/KRW","EUR/KRW","JPY/KRW","GBP/KRW","AUD/KRW","CNY/KRW"]}
```

서버 응답 (1회):
```json
{"type":"subscribed","pairs":["USD/KRW","EUR/KRW",...]}
```

이후 시세가 흐름. unsubscribe:
```json
{"type":"unsubscribe","pairs":["USD/KRW"]}
```

### 4.4 envelope 형식 (legacy = broker subscribe 와 동일)

```json
{
  "ts":      "2026-05-31T07:14:43.374Z",
  "feed":    "BEST",
  "seq":     12345,
  "msgtype": "incremental",
  "symbol":  "USDKRW",
  "entries": [
    {"type":"bid", "px": 1380.5608, "qty": 0},
    {"type":"ask", "px": 1380.8369, "qty": 0}
  ]
}
```

차이점 (broker subscribe 와 비교):
- `feed` 가 `BEST` (mci-price 가 다중시장 best 산정 후 합성 source)
- `qty` 가 항상 0 — BEST tick 은 호가량 정보 없음 (운영 cs 화면이 qty 무시한다면 영향 0)
- `msgtype` 은 항상 `incremental`

운영 cs 가 `entries[].type == "bid"/"ask"` 보고 `px` 만 사용한다면 변경 0.

### 4.5 ping / pong (heartbeat)

WebSocket protocol 의 표준 ping/pong frame 사용. mci-edge-price 가 주기적으로
ping 보냄 (default 30초 또는 `--ws-ping-interval`). cs 가 자동 pong 응답 (대부분
ws library 가 자동 처리).

연결이 끊기면 ws close frame (code=1006 등) 받음. cs 가 reconnect 로직 구현.

## 5. Visual C++ 예시 — WinHTTP WebSocket API

Windows 8+ 부터 표준. 추가 dll 의존 없음.

```cpp
#include <winhttp.h>
#pragma comment(lib, "winhttp.lib")

bool ConnectWS(const wchar_t* host, INTERNET_PORT port, const wchar_t* path) {
    // 1) HTTP session
    HINTERNET hSession = WinHttpOpen(L"WTG-CS-Client/1.0",
        WINHTTP_ACCESS_TYPE_DEFAULT_PROXY, nullptr, nullptr, 0);
    HINTERNET hConnect = WinHttpConnect(hSession, host, port, 0);

    // 2) Upgrade request
    HINTERNET hRequest = WinHttpOpenRequest(hConnect, L"GET", path,
        nullptr, nullptr, nullptr, 0);

    // 3) Set upgrade flag
    BOOL ok = WinHttpSetOption(hRequest,
        WINHTTP_OPTION_UPGRADE_TO_WEB_SOCKET, nullptr, 0);

    // 4) Send (인증 query 는 path 에 이미 포함됨)
    WinHttpSendRequest(hRequest, WINHTTP_NO_ADDITIONAL_HEADERS, 0,
        WINHTTP_NO_REQUEST_DATA, 0, 0, 0);
    WinHttpReceiveResponse(hRequest, nullptr);

    // 5) Complete upgrade
    HINTERNET hWebSocket = WinHttpWebSocketCompleteUpgrade(hRequest, NULL);
    WinHttpCloseHandle(hRequest);

    // 6) Send subscribe message
    const char* subMsg = "{\"type\":\"subscribe\",\"pairs\":[\"USD/KRW\",\"EUR/KRW\"]}";
    WinHttpWebSocketSend(hWebSocket,
        WINHTTP_WEB_SOCKET_UTF8_MESSAGE_BUFFER_TYPE,
        (PVOID)subMsg, (DWORD)strlen(subMsg));

    // 7) Receive loop
    BYTE buf[8192];
    DWORD recvLen;
    WINHTTP_WEB_SOCKET_BUFFER_TYPE bufType;
    while (WinHttpWebSocketReceive(hWebSocket, buf, sizeof(buf),
                                    &recvLen, &bufType) == NO_ERROR) {
        if (bufType == WINHTTP_WEB_SOCKET_CLOSE_BUFFER_TYPE) break;
        // buf 의 recvLen 바이트 = JSON envelope. 기존 parser 그대로 사용.
        // 예: ParseQuoteEnvelope(buf, recvLen);
    }

    WinHttpCloseHandle(hWebSocket);
    return true;
}
```

`path` 인자 예: `L"/v1/subscribe?x_wtg_user=alice"` (dev) 또는
`L"/v1/subscribe?access_token=eyJ..."` (운영).

운영 TLS (wss://) 사용 시 `WinHttpOpen` 에서 `WINHTTP_FLAG_SECURE` 추가 +
`WinHttpOpenRequest` 의 마지막 flag.

## 6. 검증 절차

### 6.1 CLI 도구로 먼저 확인

cs 코드 변경 전에 endpoint 자체 동작 확인. 예: `wscat` 또는 `websocat`:

```bash
brew install websocat       # macOS
websocat 'ws://10.0.0.10:8089/v1/subscribe?x_wtg_user=alice'
# stdin 으로 subscribe 보냄:
{"type":"subscribe","pairs":["USD/KRW","EUR/KRW"]}
# 시세 envelope 흘러옴 ✓
```

### 6.2 cs 측 통합

1. WinHTTP WebSocket connect 성공
2. subscribe message 전송 → `{"type":"subscribed",...}` 응답
3. envelope stream 수신 — 기존 parser 가 정상 동작 (entries 의 bid/ask 추출)
4. broker subscribe 와 동일 envelope 형식 검증 (msgtype/symbol/entries)
5. reconnect / network 끊김 시나리오 — close frame 받고 재연결

### 6.3 운영 전환 단계

| Phase | 작업 | 검증 |
|------|------|------|
| **P4-1**: dual subscribe | cs 가 broker + ws 양쪽 동시 subscribe (broker 가 primary, ws 가 shadow) | ws envelope 이 broker 와 일치 |
| **P4-2**: ws primary | cs 가 ws 를 primary 로 사용, broker 는 fallback | ws stable |
| **P4-3**: broker 끔 | cs 가 broker subscribe 코드 제거. ws 만 | 운영 안정성 |

P4-3 완료 시 → broker 시세 부하 영구 0 → mymqd SIGABRT 위험 제거.

## 7. 트러블슈팅

| 증상 | 원인 / 처치 |
|------|-------|
| `HTTP 401` Sec-WebSocket-Accept 없음 | 인증 query 누락 — `?x_wtg_user=` 또는 `?access_token=` 첨부 |
| `403 Forbidden` | mci-edge-price 의 `--allow-cidrs` 에 cs client IP 없음. 운영 IP 추가 |
| close code=1006 직후 종료 | CheckOrigin 거부 (운영 환경). `--dev` 모드 또는 origin 화이트리스트 확인 |
| envelope 의 `qty` 항상 0 | BEST tick 은 호가량 정보 없음. cs 가 qty 필수면 별도 협의 (호가량 추가 spec) |
| `feed` 가 "BEST" — cs 가 "SMB" / "KMB" 기대 | mci-price 가 다중시장 best 산정 후 합성. raw feed 별로 받아야 한다면 `--envelope-format=best` 시 source 별 정보 (단 schema 다름) |

## 8. 검증 완료 사례 (WTG 측)

테스트 스크립트 (`internal/edge/price/legacy_envelope_test.go`) + 실 e2e 검증:

```bash
# legacy 인스턴스 + load-gen 시세 흘림 → ws connect → envelope 확인
./build/bin/mci-edge-price --listen :8089 --envelope-format=legacy --dev &
./build/bin/load-gen --duration=4s --rate=20 --pairs=USDKRW,EURKRW &
websocat 'ws://localhost:8089/v1/subscribe?x_wtg_user=alice'
> {"type":"subscribe","pairs":["USD/KRW","EUR/KRW"]}
< {"type":"subscribed","pairs":["EUR/KRW","USD/KRW"]}
< {"ts":"2026-05-31T07:14:43.374Z","feed":"BEST","seq":...,"msgtype":"incremental","symbol":"EURKRW","entries":[{"type":"bid","px":1500.4,"qty":0},{"type":"ask","px":1500.7,"qty":0}]}
< {"ts":"2026-05-31T07:14:43.374Z","feed":"BEST","seq":...,"msgtype":"incremental","symbol":"USDKRW","entries":[{"type":"bid","px":1380.56,"qty":0},{"type":"ask","px":1380.84,"qty":0}]}
```

## 9. 일정 가이드 (예상)

| 단계 | 작업 | 담당 | 소요 |
|------|------|------|------|
| P1 | WTG envelope 호환 옵션 | WTG | **완료** (commit 3dc293f) |
| P2 | cs 측 WinHTTP WS 통합 | cs 팀 | 1~2주 |
| P3 | 통합 검증 (legacy envelope 확인) | 공동 | 2~3일 |
| P4-1 | cs dual subscribe (shadow ws) | cs / 운영 | 1주 |
| P4-2 | cs ws primary | cs / 운영 | 1주 |
| P4-3 | broker subscribe 제거 | 운영 | 1일 |

## 10. 운영 배포 시나리오

dev 환경 (`ws://localhost:8089`) 을 운영으로 옮길 때 점검 표.

### 10.1 TLS (wss://) 종단

cs client → mci-edge-price 사이를 TLS 로 보호. 사내망이라도 cs framework 의
보안 정책에 부합.

```bash
./build/bin/mci-edge-price \
    --listen :8089 \
    --upstream 127.0.0.1:50051 \
    --envelope-format=legacy \
    --tls-cert /etc/wtg/cert/edge-price.crt \
    --tls-key  /etc/wtg/cert/edge-price.key \
    --log-level=info
```

cs 측 URL: `wss://edge-price.example.com:8089/v1/subscribe?access_token=...`.

운영 TLS 권장:
- TLS 1.2+ (cert 발급 시 PEM 형식)
- LetsEncrypt 또는 사내 CA
- cert rotation — systemd unit + `--reload` (HUP) 또는 컨테이너 재기동

### 10.2 IP allowlist + rate limit

운영 cs client 의 출발 IP 범위로 제한 — 외부 노출 시 필수.

```bash
./build/bin/mci-edge-price \
    --listen :8089 \
    --envelope-format=legacy \
    --allow-cidrs 10.0.0.0/24,10.0.1.0/24 \
    --ip-rate 100 --ip-burst 200 \
    ...
```

- `--allow-cidrs`: 콤마 구분 CIDR. 비면 모두 허용 (dev 만).
- `--ip-rate`: IP 당 sustained tick/sec. 0 = 비활성.
- `--ip-burst`: 단기 burst 허용량 (token bucket size).

### 10.3 HA — 다중 인스턴스 + LB

WebSocket 은 long-lived TCP. LB 가 sticky session 또는 stateless connection
정책 가져야 함. nginx 예시:

```nginx
upstream edge_price_legacy {
    least_conn;
    server 10.0.10.1:8089 max_fails=3 fail_timeout=30s;
    server 10.0.10.2:8089 max_fails=3 fail_timeout=30s;
}

server {
    listen 443 ssl;
    server_name edge-price.example.com;

    ssl_certificate     /etc/nginx/cert/edge-price.crt;
    ssl_certificate_key /etc/nginx/cert/edge-price.key;

    location /v1/subscribe {
        proxy_pass http://edge_price_legacy;
        proxy_http_version 1.1;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection "upgrade";
        proxy_set_header X-Real-IP $remote_addr;
        proxy_read_timeout 3600s;     # 시세는 long-lived stream
        proxy_send_timeout 3600s;
    }
}
```

mci-edge-price 인스턴스 둘 다 같은 mci-price gRPC 를 upstream 으로. 각 인스턴스는
독립 subscriber_id (`--subscriber-id=edge-price-legacy-01` / `-02`) 로 grpc 등록
— mci-price 가 양쪽에 모두 fan-out.

### 10.4 monitoring + alert

#### 10.4.1 핵심 지표

| 지표 | endpoint | 임계 (alert) | 의미 |
|------|----------|------------|------|
| **e2e latency P99** | mci-price `/v1/price-stats` `.latency.bucket_*` | bucket_lt_100ms 비율 < 99% | broker 우회 path 의 지연 |
| **forwarder publish drop** | forwarder `:9091/stats` `.publish_errors_total` | rate > 0.1% of pub | grpc 재연결 또는 broker 끊김 |
| **grpc reconnect count** | forwarder logs (`PublishTick stream 재연결 OK total_reconnects=N`) | N 증가 (1시간 > 3) | mci-price 불안정 |
| **edge-price subscriber count** | mci-edge-price `/v1/edge-stats` `.registry.count` | 갑작스러운 0 | LB / 네트워크 단절 |
| **edge send dropped** | mci-edge-price `/v1/edge-stats` `.registry.dropped` | rate > 0 | slow consumer 격리 (운영 cs 의 처리 속도 ↓) |
| **mci-price clock skew** | mci-price `/v1/price-stats` `.latency.negative_count` | 증가 | forwarder ↔ mci-price NTP 동기 점검 |

#### 10.4.2 Prometheus / alert 예시

```yaml
# mci-edge-price 의 send_dropped 가 증가하면 cs client 가 느려진 것
- alert: EdgePriceSlowConsumer
  expr: rate(wtg_edge_price_registry_dropped[5m]) > 0
  for: 2m
  labels: { severity: warn }
  annotations:
    summary: "edge-price legacy 의 slow consumer drop 발생"

# forwarder 의 grpc 재연결이 잦으면 mci-price 가 흔들리는 것
- alert: ForwarderGRPCReconnectBurst
  expr: increase(quote_forwarder_grpc_reconnects[1h]) > 3
  for: 5m
  labels: { severity: warn }
  annotations:
    summary: "1시간 안에 grpc 재연결 3회 초과"

# latency P99 가 100ms 초과 — broker 우회 path 비정상
- alert: PriceLatencyP99High
  expr: |
    1 - (
      wtg_price_latency_bucket{le="100ms"}
      / wtg_price_latency_count
    ) > 0.01
  for: 5m
  labels: { severity: warn }
```

#### 10.4.3 운영 dashboard 권장 panel

- forwarder published / publish_errors / queue_drops (시계열)
- mci-price ticks/sec rate + latency avg/P99
- edge-price registry count + sent/dropped (시계열)
- forwarder grpc reconnect (누계)
- mci-price clock skew negative_count

#### 10.4.4 mci-admin UI 의 시세 통계 페이지

운영 monitoring 보조 — 사이드바 진단 도구 → 시세 통계:
- 6 카드 (received / matched / ticks / dropped / sub_drops / conflation symbols)
- Latency 카드 (avg/max + bucket 분포 + clock skew)
- BEST 산정 테이블 (symbol × source × spread)
- 2초 polling 자동 갱신

## 11. P4-1 envelope 일치 검증 — `quote-diff` 도구

WTG 가 제공하는 자동 비교 도구. cs 마이그레이션 단계의 confidence 확보용.

### 11.1 단독 검증 (legacy ↔ best 변환 정확도)

WTG 단독 — legacy envelope 변환이 best 와 값 일치하는지:

```bash
./build/bin/quote-diff \
    --source-a ws://localhost:8083/v1/subscribe \
    --source-b ws://localhost:8089/v1/subscribe \
    --pairs USD/KRW,EUR/KRW,JPY/KRW \
    --duration 10m \
    --user diff
```

`matched: ~100%`, `mismatched: 0`, `orphan: 0` 이면 변환 정확.

### 11.2 P4-1 dual run (cs ws + cs broker)

cs 가 양쪽 subscribe 한 envelope 을 동시 출력하면 동일 형식으로 비교 가능:
- cs ws output → ws relay (cs 가 직접 quote-diff 호환 ws 노출)
- cs broker output → ws relay
- quote-diff 가 두 ws 비교

또는 cs 가 양쪽 envelope 을 같은 파일에 dump → diff 스크립트 (jq + 비교).

### 11.3 종료 코드

- exit 0: mismatched = 0
- exit 1: mismatched > 0 (CI 자동 fail)

## 11-A. legacy 경로 필드 대사표 (broker subscribe → WTG legacy)

원본(mymq broker PRICE 구독)과 WTG legacy 인스턴스(`:8089`, `--envelope-format=legacy`)
출력의 **필드 단위 대조**. 컷오버 전 cs 파서 영향도 판정 근거.

🟢 동일 · 🟡 형식 동일/값 재산정 · 🔴 값·의미 상이

### A. Envelope 최상위 필드

| 필드 | 원본 (broker PRICE 구독) | WTG legacy | 동일성 | 비고 |
|---|---|---|---|---|
| `ts` | tick 시각 RFC3339(nano) UTC | 동일 형식, BEST tick `ts` | 🟡 | best 합성 시점으로 재산정 |
| `feed` | 실제 피드/시장 source 명 | `"BEST"` 고정 | 🔴 | 다중시장 best 합성 source |
| `seq` | 출처별 시퀀스 | WTG 자체 `seq_num` | 🔴 | dedup/누락탐지용, 절대값 무의미 |
| `msgtype` | `snapshot`/`incremental` | `"incremental"` 고정 | 🟡 | WTG 는 항상 incremental |
| `symbol` | `"USDKRW"` 외부표기 | 동일 | 🟢 | SymbolMap 표기 그대로 |
| `entries` | `[{type,px,qty}, …]` | `[{type,px,qty:0}, …]` | 🟡 | B 참조 |

### B. `entries[]` 원소

| 필드 | 원본 | WTG legacy | 동일성 | 비고 |
|---|---|---|---|---|
| `type` | `"bid"` / `"ask"` | `"bid"` / `"ask"` | 🟢 | 한쪽 호가만 있으면 그 entry 만. 순서 bid→ask |
| `px` | 해당 feed 호가 | BEST `bid`/`ask` | 🔴 | C 참조 |
| `qty` | 호가 수량 | `0` 고정 | 🔴 | BEST tick 은 수량 정보 없음 |

### C. 값(px) 산정 차이 — 핵심

| 항목 | 원본 | WTG legacy |
|---|---|---|
| 산정 | 구독하던 단일 feed 의 raw 호가 | 다중시장 best = `max(bid)` / `min(ask)` (cross 시 최신 ts fallback) |
| 마진 | 환경에 따라 | **미적용** — legacy 는 raw BEST tick 기반 (마진은 best `:8083` 경로에만) |
| 판정 | 단일 feed 였다면 값 상이 가능 / 예전에도 best 였다면 수렴 | |

### D. 필드 증감
- **없어짐**: 원본에 여러 `MDEntryType`(trade/open/volume 등) 또는 feed별 부가 필드가
  있었다면 → WTG legacy 는 bid/ask 2개 entry 로 축약.
- **추가**: 없음 (상위 스키마 동일).

### E. cs 파서 영향도 판정

| cs 파서가 쓰는 것 | 영향 |
|---|---|
| `entries[].type` + `px` 만 | 🟢 변경 0 |
| `qty` 사용 (수량 표시) | 🔴 항상 0 → 로직 확인 |
| `feed` 로 시장 구분 | 🔴 항상 BEST → 재검토 |
| `seq` 연속성 검사 | 🔴 채번 체계 상이 → dedup 확인 |
| `msgtype` snapshot/incremental 분기 | 🟡 항상 incremental |

### F. 검증 (컷오버 전 필수)
- `cmd/quote-diff` — 원본 vs best 두 ws source 필드 자동 비교 (§11)
- `cmd/quote-replay` — mds `.trc` 캡처 mds/WTG 동시 재생 → 출력 대사

## 12. 점검 체크리스트 (운영 배포 직전)

cs 가 P4-3 (broker subscribe 제거) 진입 전 마지막 확인:

- [ ] mci-edge-price legacy 인스턴스 TLS cert 유효 (`openssl x509 -in cert.pem -noout -dates`)
- [ ] `--allow-cidrs` 에 운영 cs 네트워크 포함
- [ ] LB sticky session 설정 또는 stateless 정책 확정
- [ ] Prometheus scrape target 등록 (mci-edge-price + mci-price + forwarder)
- [ ] alert rule 배포 (위 §10.4.2)
- [ ] quote-diff 24h 이상 mismatched=0 확인
- [ ] mci-price `/v1/price-stats.latency.negative_count == 0` (NTP 동기 OK)
- [ ] forwarder `quote_forwarder_grpc_reconnects` 추세 안정
- [ ] cs framework 측 reconnect 로직 검증 (서버 재기동 시 자동 복구)
- [ ] mci-edge-price-legacy 인스턴스 systemd unit 또는 컨테이너 manifest 영속화
- [ ] runbook — 장애 시 broker subscribe 일시 복귀 절차 (`WTG_FWD_PUBLISH_MODE=both`)
