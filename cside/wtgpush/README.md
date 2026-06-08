# wtgpush — WTG mci-push HTTP push C SDK

WTG Phase 2.6 — 운영 svc (C, mymq AP) 가 broker publish 대신 mci-push 의
`POST /v1/internal/push` 를 호출해 unsolicited 메시지를 발사하는 SDK.

## 설계 결정

- **외부 의존 0** — POSIX socket + HTTP/1.1 minimal 구현
- **TLS 없음** — Internal 망 전제, X-Push-Secret 만 (Phase 2.5 결정)
- **연결 풀 없음** — 호출마다 socket open/close (단순함 우선)
- 호출 빈도 ↑ (수백/초) 환경은 `wtgpush_pool.c` 후속

## 빌드

```bash
cd cside/wtgpush
make            # libwtgpush.a + sample
make clean
```

플랫폼별:
- Linux / macOS / *BSD — 기본 그대로
- Solaris — `-lsocket -lnsl` 자동 추가
- AIX / HPUX — 기본 socket lib 으로 충분

## 사용

```c
#include "wtgpush.h"

wtg_push_client_t cli;
wtg_push_init(&cli, "mci-push.internal", 8081,
              getenv("WTG_PUSH_SECRET"), 2000);

// user-targeted
int rc = wtg_push_send(&cli, "dealer01",
                       "{\"orderId\":123,\"status\":\"FILLED\"}");
if (rc != WTGPUSH_OK) {
    fprintf(stderr, "push 실패: %s (http=%d)\n",
            wtg_push_strerror(rc), cli.last_http_status);
}

// broadcast (전체 ws 사용자)
wtg_push_broadcast(&cli, "{\"market\":\"HALT\"}");
```

## mymq AP 통합 (참조)

`/Users/winwaysystems/mywork/mymq/src/http/alert.c` 같은 기존 모듈에 추가하는 경우:

```makefile
# mymq AP Makefile 에 추가
WTGPUSH_DIR = $(MYMQ_ROOT)/../wtg/cside/wtgpush
CFLAGS  += -I$(WTGPUSH_DIR)
LDFLAGS += -L$(WTGPUSH_DIR) -lwtgpush
```

또는 obj 직접 link:

```makefile
LIBS += $(WTGPUSH_DIR)/libwtgpush.a
```

## 통합 test

mci-push 띄운 상태에서:

```bash
# 1. mci-push 띄움 (별 터미널)
./build/bin/mci-push --listen=:8081 --push-secret=mysecret --dev

# 2. sample 으로 user push
./sample 127.0.0.1 8081 mysecret dealer01 '{"price":1.0850}'
# 출력: send(dealer01) → 0 (OK) http=200 errno=0

# 3. broadcast
./sample 127.0.0.1 8081 mysecret "" '{"market":"HALT"}'
# 출력: broadcast → 0 (OK) http=200 errno=0

# 4. 인증 실패 (잘못된 secret)
./sample 127.0.0.1 8081 wrong dealer01 '{"x":1}'
# 출력: send(dealer01) → -8 (HTTP 4xx ...) http=401 errno=0
```

## 마이그레이션 (broker publish → HTTP push)

기존 mymq AP 가 broker 로 publish 하던 코드:

```c
// 기존 — broker 로 unsolicited publish
PublishRequest pub = { .exchange = "PUSH", .routing_key = "user:dealer01", ... };
mq_publish(&pub);
```

→ HTTP push 로 전환:

```c
// 신규 — mci-push 의 HTTP endpoint 호출
wtg_push_send(&cli, "dealer01", json_data);
```

기존 broker path 와 병행 (dual-write) 가능 — 운영 안정 확인 후 broker path 제거 (Phase 2.7).

## 반환 코드

| 코드 | 의미 |
|---|---|
| `WTGPUSH_OK` (0) | 성공 (HTTP 200) |
| `WTGPUSH_E_INVALID` (-1) | 인자 NULL / 잘못 |
| `WTGPUSH_E_RESOLVE` (-2) | DNS resolve 실패 |
| `WTGPUSH_E_SOCKET` (-3) | socket() / setsockopt() 실패 |
| `WTGPUSH_E_CONNECT` (-4) | connect() 실패 / timeout |
| `WTGPUSH_E_SEND` (-5) | send() 실패 (broken pipe 등) |
| `WTGPUSH_E_RECV` (-6) | recv() 실패 / timeout |
| `WTGPUSH_E_PARSE` (-7) | HTTP 응답 파싱 실패 |
| `WTGPUSH_E_HTTP_4XX` (-8) | HTTP 4xx (auth 실패 등) |
| `WTGPUSH_E_HTTP_5XX` (-9) | HTTP 5xx (inject_full 등) |
| `WTGPUSH_E_OVERSIZE` (-10) | user / data buffer 초과 |

`WTGPUSH_E_HTTP_*` 의 경우 `cli->last_http_status` 로 정확한 코드 확인. 다른 에러는 `cli->last_errno` 가 의미 가짐.

## thread safety

- `wtg_push_init` — 단일 thread 가 init (한 번만).
- `wtg_push_send` / `wtg_push_broadcast` — **각 thread 가 자체 `wtg_push_client_t` 사용 권장**.
  공유 cli 를 여러 thread 가 동시 호출하면 `last_http_status` / `last_errno` 가 race.
  발사 자체 (socket open/close) 는 reentrant.
- `gethostbyname` 은 thread-unsafe (legacy) — init 시 한 번 호출되면 OK.
  IP 주소를 cli->host 에 직접 넣으면 호출 시점에 DNS resolve 불필요.

## 관련 문서

- `docs/push-monitoring.md` — Prometheus / Grafana 가시화
- `docs/push-secret-rotation.md` — 운영 secret 관리
- `pkg/push/client.go` — Go 측 동등 SDK (multi-instance / mTLS 옵션 포함)
- `internal/push/http_push.go` — mci-push 측 핸들러 (wire 호환 검증)
