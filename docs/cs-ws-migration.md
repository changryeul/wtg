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
