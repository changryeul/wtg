# 클라이언트 시세 구독 가이드 (web / HTS)

client(web, HTS/EMP 등)가 WTG 에서 시세를 **구독하고 수신**하는 방법. 실 endpoint /
envelope / 인증은 `internal/edge/price` 구현 기준이다.

## 1. 전체 그림

```
[client] ──WebSocket──▶ mci-edge-price (DMZ)
                          │  GET /v1/subscribe
                          └─ Internal: gRPC ◀── mci-price (BestConsumer + Profile 마진)
                                                (broker 미경유 — 시세 fan-out 부하 분리)
```

- 시세는 **WebSocket 단방향 push**. 연결하면 서버가 tick 을 계속 밀어준다.
- 클라이언트는 원하면 **control 메시지**로 통화쌍 필터만 조정한다(양방향은 이것뿐).
- HTS/EMP(레거시 C++)도 **동일 WS endpoint** 를 쓰되 envelope 포맷만 legacy 로 맞춘다.

| 포트 | 인스턴스 | envelope | 대상 |
|---|---|---|---|
| `:8083` | `mci-edge-price` | best (기본) | 신규 web client |
| `:8089` | `mci-edge-price-legacy` (`--envelope-format=legacy`) | legacy(entries) | 기존 HTS/EMP (파서 무변경) |

## 2. 인증 — 먼저 JWT 를 받는다

WebSocket 은 브라우저에서 커스텀 헤더를 못 넣으므로 **토큰을 쿼리로** 전달한다.
edge-price 미들웨어(`BearerFromQuery`/`UserFromQuery`)가 쿼리를 헤더로 변환한다.

```
운영:   wss://<host>:8083/v1/subscribe?access_token=<JWT>
개발:   ws://<host>:8083/v1/subscribe?x_wtg_user=alice01     (DevMode)
```

JWT 는 로그인으로 발급받는다 (edge-api 경유):

```
POST https://<host>:8090/v1/login   {"usid":"alice01","passwd":"****", ...}
 → { "access_token":"<JWT>", "refresh_token":"...", ... }
```

로그인 시 **Site/Tier → Profile** 이 결정되어 JWT 에 박히고, 클라이언트는 자기
Profile 의 **마진 적용 시세만** 수신한다(위변조 불가). `?profile=` 쿼리는 dev
도구 전용 fallback 이며 운영에서는 JWT 의 Profile 이 우선한다.

## 3. 수신 envelope

### (A) 기본 "best" quote — 신규 web client 표준
Profile 라우팅 + 마진 적용된 합성(BEST) 시세.

```json
{
  "type": "quote",
  "pair": "USD/KRW",
  "channel": "WEB", "site": "BRANCH", "tier": "VIP", "tenor": "SPT",
  "bid": 1385.20, "ask": 1385.60,
  "ts_unix_nano": 1720900000000000000,
  "raw_bid": 1385.30, "raw_ask": 1385.50,
  "v": 42,
  "quote_id": "q-abc123", "valid_until_unix_nano": 1720900002000000000
}
```

부가로 서버가 보내는 제어/알림 프레임:

```json
{"type":"subscribed", "pairs":["USD/KRW","EUR/USD"]}   // 필터 상태 echo (pairs:null = 전체)
{"type":"error", "code":"bad_request", "message":"..."} // 잘못된 control (연결은 유지)
```

### (B) legacy envelope — HTS/EMP 파서 무변경
`--envelope-format=legacy` 포트(:8089)에서. cs 가 broker subscribe 로 받던 형식 그대로.

```json
{ "ts":"2026-07-15T09:00:00.123Z", "feed":"BEST", "seq":1024,
  "msgtype":"incremental", "symbol":"USDKRW", "entries":[ ... ] }
```

> 핵심: legacy 를 쓰면 transport(mymq TCP → ws)만 교체하고 **파서 코드는 그대로**.

## 4. 프로토콜 규칙

- **방향**: 서버→클라이언트 단방향 시세 push. 클라이언트→서버는 control 메시지만.
- **필터**: 연결 직후 아무것도 안 보내면 **전체(all) 수신**. 아래로 한정/해제한다.
  ```json
  {"type":"subscribe",   "pairs":["USD/KRW","EUR/USD"]}   // 필터 설정/추가
  {"type":"unsubscribe", "pairs":["EUR/USD"]}             // 제거 (빈 셋 되면 all 복귀)
  ```
  서버는 처리 후 `{"type":"subscribed","pairs":[...]}` 로 현재 상태를 echo.
- **keepalive**: 서버가 주기적으로 ws ping 을 보낸다. 브라우저/WinHTTP 는 pong 자동
  응답. pong 이 timeout(`WsPongTimeout`) 넘으면 서버가 연결을 끊는다.
- **재연결**: 클라이언트 책임. 끊기면 backoff 후 재연결하고 필터를 다시 보낸다.
- **TLS**: 운영은 `wss://`.

## 5. Web 클라이언트 (JavaScript) — 자동재연결 + 재구독 포함

```javascript
class QuoteClient {
  constructor({ host, getToken, pairs, onQuote }) {
    this.host = host;            // "host:8083"
    this.getToken = getToken;    // async () => JWT (만료 시 refresh 포함)
    this.pairs = pairs || null;  // null = 전체 수신
    this.onQuote = onQuote;
    this.backoff = 1000;         // 재연결 backoff (ms), 최대 30s
    this.stopped = false;
  }

  async connect() {
    const token = await this.getToken();
    const ws = new WebSocket(`wss://${this.host}/v1/subscribe?access_token=${token}`);
    this.ws = ws;

    ws.onopen = () => {
      this.backoff = 1000;                     // 성공 시 backoff 리셋
      if (this.pairs) this.setPairs(this.pairs); // 재연결 시 필터 복원
    };

    ws.onmessage = (ev) => {
      const msg = JSON.parse(ev.data);
      switch (msg.type) {
        case "quote":       this.onQuote(msg); break;
        case "subscribed":  /* 현재 구독 상태 msg.pairs */ break;
        case "error":       console.warn("서버 오류:", msg.code, msg.message); break;
      }
    };

    ws.onclose = () => {
      if (this.stopped) return;
      setTimeout(() => this.connect(), this.backoff);
      this.backoff = Math.min(this.backoff * 2, 30000); // 지수 backoff
    };
    ws.onerror = () => ws.close();
  }

  setPairs(pairs) {                            // 통화쌍 필터 변경
    this.pairs = pairs;
    if (this.ws?.readyState === WebSocket.OPEN)
      this.ws.send(JSON.stringify({ type: "subscribe", pairs }));
  }
  unsubscribe(pairs) {
    if (this.ws?.readyState === WebSocket.OPEN)
      this.ws.send(JSON.stringify({ type: "unsubscribe", pairs }));
  }
  stop() { this.stopped = true; this.ws?.close(); }
}

// 사용
const qc = new QuoteClient({
  host: "fx.example.com:8083",
  getToken: async () => localStorage.getItem("wtg_access_token"),
  pairs: ["USD/KRW", "EUR/USD"],
  onQuote: (q) => render(q.pair, q.bid, q.ask, q.ts_unix_nano),
});
qc.connect();
```

## 6. HTS / EMP (레거시 C++, WinHTTP WebSocket)

기존 broker subscribe 를 걷어내고 WinHTTP 로 WS 연결. envelope 은 legacy 포트
(:8089)로 받아 **기존 파서를 그대로** 쓴다.

```c
#include <windows.h>
#include <winhttp.h>
#pragma comment(lib, "winhttp.lib")

// 기존 broker subscribe 파서 재사용 — envelope 이 동일하므로 시그니처 그대로.
extern void parse_legacy_envelope(const char *json, int len);

static HINTERNET g_ws = NULL;

int quote_connect(const wchar_t *host, int port, const wchar_t *jwt) {
    HINTERNET hSession = WinHttpOpen(L"HTS/1.0",
        WINHTTP_ACCESS_TYPE_DEFAULT_PROXY, NULL, NULL, 0);
    HINTERNET hConnect = WinHttpConnect(hSession, host, (INTERNET_PORT)port, 0);

    // 토큰은 쿼리로 (WS 헤더 제약). 운영은 access_token, dev 는 x_wtg_user.
    wchar_t path[512];
    swprintf(path, 512, L"/v1/subscribe?access_token=%s", jwt);

    HINTERNET hReq = WinHttpOpenRequest(hConnect, L"GET", path,
        NULL, WINHTTP_NO_REFERER, WINHTTP_DEFAULT_ACCEPT_TYPES,
        WINHTTP_FLAG_SECURE);                       // wss = SECURE

    WinHttpSetOption(hReq, WINHTTP_OPTION_UPGRADE_TO_WEB_SOCKET, NULL, 0);
    if (!WinHttpSendRequest(hReq, WINHTTP_NO_ADDITIONAL_HEADERS, 0,
                            NULL, 0, 0, 0)) return -1;
    if (!WinHttpReceiveResponse(hReq, NULL)) return -1;

    g_ws = WinHttpWebSocketCompleteUpgrade(hReq, 0);
    WinHttpCloseHandle(hReq);                        // upgrade 후 request 핸들 해제
    return g_ws ? 0 : -1;
}

// (선택) 통화쌍 필터. 안 보내면 전체 수신.
void quote_subscribe(const char *pairs_json) {       // 예: {"type":"subscribe","pairs":["USD/KRW"]}
    WinHttpWebSocketSend(g_ws, WINHTTP_WEB_SOCKET_UTF8_MESSAGE_BUFFER_TYPE,
                         (PVOID)pairs_json, (DWORD)strlen(pairs_json));
}

// 수신 루프 (전용 스레드 권장) — envelope 은 기존과 동일 → 기존 파서 그대로.
void quote_recv_loop(void) {
    char buf[16384];
    for (;;) {
        DWORD n = 0; WINHTTP_WEB_SOCKET_BUFFER_TYPE bt;
        DWORD rc = WinHttpWebSocketReceive(g_ws, buf, sizeof(buf)-1, &n, &bt);
        if (rc != NO_ERROR) break;                   // 오류 → 재연결 로직으로
        if (bt == WINHTTP_WEB_SOCKET_CLOSE_BUFFER_TYPE) break;
        // UTF8 조각(FRAGMENT)일 수 있으니 message 완성까지 누적 필요 시 처리.
        buf[n] = '\0';
        parse_legacy_envelope(buf, (int)n);          // {ts,feed,seq,msgtype,symbol,entries}
    }
    WinHttpWebSocketClose(g_ws, WINHTTP_WEB_SOCKET_SUCCESS_CLOSE_STATUS, NULL, 0);
    // 끊기면: 재로그인(JWT 갱신)→quote_connect 재호출→quote_subscribe 재전송
}
```

주의:
- **ping/pong**: WinHTTP 가 protocol ping 을 자동 응답한다(별도 코드 불필요).
- **fragment**: 큰 메시지는 `WINHTTP_WEB_SOCKET_UTF8_MESSAGE_FRAGMENT_BUFFER_TYPE`
  로 조각나 올 수 있으니, message 타입이 될 때까지 누적한 뒤 파싱.
- **재연결/JWT 만료**: 수신 오류/close 시 재로그인으로 JWT 갱신 후 재연결·재구독.

전환 운영 세팅(legacy 포트 기동 등)은 `docs/cs-ws-migration.md` 참조.

## 7. 요점

| 항목 | 값 |
|---|---|
| endpoint | `GET /v1/subscribe` (ws upgrade) — best :8083 / legacy :8089 |
| 인증 | `?access_token=<JWT>`(운영) 또는 `?x_wtg_user=<id>`(dev). Profile 은 JWT 에서 결정 |
| 방향 | 서버→클라이언트 단방향 push. 클라이언트는 subscribe/unsubscribe control 만 |
| 필터 | 미전송 시 전체, `{"type":"subscribe","pairs":[...]}` 로 한정 |
| envelope | best(`type:quote`) / legacy(`entries`) |
| keepalive | 서버 주기 ping, 클라이언트 pong 자동. 재연결은 클라이언트 책임 |
| TLS | 운영 `wss://` |

## 관련 문서
- `docs/cs-ws-migration.md` — HTS/EMP(cs framework) broker → WS 전환 단계별
- `docs/customer-connections.md` — login→JWT→ws→fan-out 3 트랙 전체 가이드
- `docs/cooker-quote-schema.md` — 시세 producer → broker → mci-price envelope(v1)
- `docs/margin-policy.md` — Profile 별 bid/ask 마진 정책
