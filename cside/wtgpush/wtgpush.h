/*
 * wtgpush.h — WTG mci-push 의 HTTP push endpoint 호출 C SDK.
 *
 * Phase 2.6 — 운영 svc (C, mymq AP) 가 broker publish 대신 mci-push 의
 *   POST /v1/internal/push 를 호출해 unsolicited 메시지를 발사.
 *
 * 설계:
 *   · 외부 의존 0 — POSIX socket + HTTP/1.1 minimal 구현
 *     (libcurl 등 별도 패키지 설치 불필요 — mymq 운영 환경 호환 보장)
 *   · TLS 없음 — Internal 망 + X-Push-Secret 인증 (Phase 2.5 결정)
 *   · thread-safe — 각 호출이 자체 socket open/close (connection pool 없음)
 *     · push rate 가 높으면 wtgpush_pool.c (후속) 사용
 *
 * 사용 예:
 *
 *   wtg_push_client_t cli;
 *   wtg_push_init(&cli, "mci-push.internal", 8081,
 *                 getenv("WTG_PUSH_SECRET"), 2000);
 *
 *   // user-targeted
 *   wtg_push_send(&cli, "dealer01", "{\"orderId\":123,\"status\":\"FILLED\"}");
 *
 *   // broadcast
 *   wtg_push_broadcast(&cli, "{\"market\":\"HALT\"}");
 */

#ifndef WTGPUSH_H
#define WTGPUSH_H

#ifdef __cplusplus
extern "C" {
#endif

/* 반환 코드 — 0 = 성공, < 0 = 실패. */
#define WTGPUSH_OK              0
#define WTGPUSH_E_INVALID      -1   /* 인자 NULL 또는 잘못 */
#define WTGPUSH_E_RESOLVE      -2   /* gethostbyname / getaddrinfo 실패 */
#define WTGPUSH_E_SOCKET       -3   /* socket() / setsockopt() 실패 */
#define WTGPUSH_E_CONNECT      -4   /* connect() 실패 (timeout 포함) */
#define WTGPUSH_E_SEND         -5   /* send() 실패 (broken pipe 등) */
#define WTGPUSH_E_RECV         -6   /* recv() 실패 또는 timeout */
#define WTGPUSH_E_PARSE        -7   /* HTTP 응답 파싱 실패 */
#define WTGPUSH_E_HTTP_4XX     -8   /* HTTP 4xx (auth 실패 등) */
#define WTGPUSH_E_HTTP_5XX     -9   /* HTTP 5xx (inject_full 등) */
#define WTGPUSH_E_OVERSIZE    -10   /* user / data 가 buffer 초과 */

/* 클라이언트 컨텍스트 — 한 번 init 후 여러 push 호출에 재사용. */
typedef struct {
	char host[256];         /* mci-push 호스트 (DNS 또는 IP) */
	int  port;              /* mci-push 포트 (8081 기본) */
	char secret[128];       /* X-Push-Secret (Phase 2.5 — secret-only 인증) */
	int  timeout_ms;        /* connect + send + recv 각 단계 timeout */
	int  last_http_status;  /* 마지막 호출의 HTTP status (디버깅용) */
	int  last_errno;        /* 마지막 호출의 errno (E_CONNECT 등에서 의미) */
} wtg_push_client_t;

/*
 * wtg_push_init — 클라이언트 초기화.
 *
 * @cli       NULL 아님 (자체 영역 — heap/stack 어디든 OK).
 * @host      mci-push 의 hostname 또는 IP (예: "mci-push.internal" / "10.0.3.20").
 * @port      mci-push 포트 (보통 8081).
 * @secret    --push-secret 값과 동일. NULL 또는 "" 면 헤더 미첨부 (dev only).
 * @timeout_ms  각 socket 단계 timeout (권장 2000 = 2초). 0 면 5000 default.
 *
 * 반환: WTGPUSH_OK 또는 WTGPUSH_E_INVALID.
 */
int wtg_push_init(wtg_push_client_t *cli,
                  const char *host, int port,
                  const char *secret, int timeout_ms);

/*
 * wtg_push_send — user 명시 push (특정 ws 사용자에게).
 *
 * @cli         init 된 클라이언트.
 * @user        대상 사용자 ID (LogonID — broker 의 그것과 동일). NULL/""불가
 *              (broadcast 는 wtg_push_broadcast 사용).
 * @json_data   JSON 문자열 (마지막 byte 가 '\0' 인 일반 C string).
 *              예: "{\"orderId\":123,\"price\":1.0850}".
 *              호출자가 escape / 유효성 보장. 본 SDK 는 그대로 envelope 의 data 에 넣는다.
 *
 * 반환: WTGPUSH_OK 또는 WTGPUSH_E_*.
 * 실패 시 cli->last_http_status 또는 cli->last_errno 참조.
 */
int wtg_push_send(wtg_push_client_t *cli,
                  const char *user, const char *json_data);

/*
 * wtg_push_broadcast — 전체 ws 사용자에게 broadcast (LogonID="").
 *
 * @cli         init 된 클라이언트.
 * @json_data   JSON 문자열. wtg_push_send 의 그것과 동일.
 *
 * 반환: WTGPUSH_OK 또는 WTGPUSH_E_*.
 */
int wtg_push_broadcast(wtg_push_client_t *cli, const char *json_data);

/*
 * wtg_push_strerror — 반환 코드의 사람이 읽을 수 있는 메시지.
 * (static 영역의 const char* — 호출자가 free 안 함, thread-safe).
 */
const char *wtg_push_strerror(int code);

#ifdef __cplusplus
}
#endif

#endif /* WTGPUSH_H */
