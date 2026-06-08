/*
 * wtgpush.c — POSIX socket + HTTP/1.1 minimal 구현.
 *
 * 설계 노트:
 *   · 호출마다 socket open / connect / send / recv / close — 연결 풀 없음.
 *     mymq AP 환경의 push 빈도 (수 회/초) 에선 충분.
 *     운영 push rate ↑ (수백/초) 환경은 wtgpush_pool.c 후속.
 *   · SO_SNDTIMEO / SO_RCVTIMEO 로 단계별 timeout — connect 는 select() 로 별도.
 *   · JSON body 는 호출자가 escape 책임 — 본 SDK 는 단순 sprintf 로 envelope 조립.
 *   · IPv4 only — IPv6 필요 시 getaddrinfo path 추가 (mymq 운영망은 IPv4).
 */

#include "wtgpush.h"

#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <errno.h>
#include <unistd.h>
#include <fcntl.h>
#include <sys/types.h>
#include <sys/socket.h>
#include <sys/time.h>
#include <netinet/in.h>
#include <netinet/tcp.h>
#include <arpa/inet.h>
#include <netdb.h>

#define WTGPUSH_DEFAULT_TIMEOUT_MS  5000
#define WTGPUSH_REQ_HEADER_MAX      1024
#define WTGPUSH_RESP_BUF            2048
#define WTGPUSH_USER_MAX            256
#define WTGPUSH_DATA_MAX            (64 * 1024)  /* 64KB body 한도 */

/* 내부 — DNS resolve (gethostbyname — IPv4). reentrant 위해 gethostbyname_r
 * 가 좋으나 AIX/Solaris 호환성 위해 gethostbyname 사용 후 결과 즉시 copy.
 * 멀티스레드에선 호스트 결과가 thread-local 이 아니므로 mymq AP 가 init 단계
 * 에서 1회 호출 권장. push 호출은 cli->host 의 IP 를 직접 줘도 무방. */
static int resolve_host(const char *host, struct in_addr *out_ip) {
	struct in_addr ip;
	if (inet_pton(AF_INET, host, &ip) == 1) {
		*out_ip = ip;
		return 0;
	}
	struct hostent *he = gethostbyname(host);
	if (he == NULL || he->h_addrtype != AF_INET || he->h_length == 0) {
		return -1;
	}
	memcpy(out_ip, he->h_addr_list[0], sizeof(struct in_addr));
	return 0;
}

/* 내부 — connect with timeout (non-blocking + select). */
static int connect_timeout(int sock, const struct sockaddr *addr, socklen_t alen,
                           int timeout_ms) {
	int flags = fcntl(sock, F_GETFL, 0);
	if (flags < 0) return -1;
	if (fcntl(sock, F_SETFL, flags | O_NONBLOCK) < 0) return -1;

	int rc = connect(sock, addr, alen);
	if (rc == 0) {
		/* 즉시 성공 — blocking 복원 후 return */
		fcntl(sock, F_SETFL, flags);
		return 0;
	}
	if (errno != EINPROGRESS) return -1;

	fd_set wfds;
	FD_ZERO(&wfds);
	FD_SET(sock, &wfds);
	struct timeval tv;
	tv.tv_sec  = timeout_ms / 1000;
	tv.tv_usec = (timeout_ms % 1000) * 1000;

	rc = select(sock + 1, NULL, &wfds, NULL, &tv);
	if (rc <= 0) {
		errno = (rc == 0) ? ETIMEDOUT : errno;
		return -1;
	}
	int sockerr = 0;
	socklen_t slen = sizeof(sockerr);
	if (getsockopt(sock, SOL_SOCKET, SO_ERROR, &sockerr, &slen) < 0) return -1;
	if (sockerr != 0) {
		errno = sockerr;
		return -1;
	}
	fcntl(sock, F_SETFL, flags);  /* blocking 복원 */
	return 0;
}

/* 내부 — send all (partial send 보정). */
static int send_all(int sock, const char *buf, size_t len) {
	size_t off = 0;
	while (off < len) {
		ssize_t n = send(sock, buf + off, len - off, 0);
		if (n < 0) {
			if (errno == EINTR) continue;
			return -1;
		}
		off += (size_t)n;
	}
	return 0;
}

/* 내부 — HTTP 응답 status line 파싱 (간단). buf 는 NULL-terminated.
 * "HTTP/1.1 200 OK\r\n..." 형식. status code 만 추출. */
static int parse_http_status(const char *buf) {
	if (strncmp(buf, "HTTP/1.", 7) != 0) return -1;
	const char *p = strchr(buf, ' ');
	if (p == NULL) return -1;
	return atoi(p + 1);
}

/* 내부 — 핵심 push 호출. user="" 면 broadcast. */
static int do_push(wtg_push_client_t *cli, const char *user, const char *json_data) {
	if (cli == NULL || cli->host[0] == 0 || cli->port <= 0) return WTGPUSH_E_INVALID;
	if (json_data == NULL) json_data = "null";

	size_t user_len = (user != NULL) ? strlen(user) : 0;
	size_t data_len = strlen(json_data);
	if (user_len >= WTGPUSH_USER_MAX) return WTGPUSH_E_OVERSIZE;
	if (data_len >= WTGPUSH_DATA_MAX) return WTGPUSH_E_OVERSIZE;

	/* 1. JSON envelope 조립 — { "user": "...", "data": <raw> }.
	 *    data 는 호출자 책임의 raw JSON — 따옴표 자체 escape 안 함. */
	char *body = (char *)malloc(WTGPUSH_DATA_MAX + WTGPUSH_USER_MAX + 64);
	if (body == NULL) return WTGPUSH_E_INVALID;
	int body_len;
	if (user_len > 0) {
		body_len = snprintf(body, WTGPUSH_DATA_MAX + WTGPUSH_USER_MAX + 64,
		                    "{\"user\":\"%s\",\"data\":%s}", user, json_data);
	} else {
		body_len = snprintf(body, WTGPUSH_DATA_MAX + WTGPUSH_USER_MAX + 64,
		                    "{\"data\":%s}", json_data);
	}
	if (body_len < 0) { free(body); return WTGPUSH_E_INVALID; }

	/* 2. HTTP 요청 헤더 조립. */
	char header[WTGPUSH_REQ_HEADER_MAX];
	int hlen = snprintf(header, sizeof(header),
	    "POST /v1/internal/push HTTP/1.1\r\n"
	    "Host: %s:%d\r\n"
	    "Content-Type: application/json\r\n"
	    "Content-Length: %d\r\n"
	    "Connection: close\r\n"
	    "%s%s%s"
	    "\r\n",
	    cli->host, cli->port, body_len,
	    (cli->secret[0] ? "X-Push-Secret: " : ""),
	    (cli->secret[0] ? cli->secret      : ""),
	    (cli->secret[0] ? "\r\n"           : ""));
	if (hlen < 0 || hlen >= (int)sizeof(header)) {
		free(body);
		return WTGPUSH_E_OVERSIZE;
	}

	/* 3. socket + connect. */
	struct in_addr ip;
	if (resolve_host(cli->host, &ip) < 0) {
		cli->last_errno = errno;
		free(body);
		return WTGPUSH_E_RESOLVE;
	}
	int sock = socket(AF_INET, SOCK_STREAM, 0);
	if (sock < 0) {
		cli->last_errno = errno;
		free(body);
		return WTGPUSH_E_SOCKET;
	}
	int one = 1;
	(void)setsockopt(sock, IPPROTO_TCP, TCP_NODELAY, &one, sizeof(one));

	int to_ms = (cli->timeout_ms > 0) ? cli->timeout_ms : WTGPUSH_DEFAULT_TIMEOUT_MS;
	struct timeval tv;
	tv.tv_sec  = to_ms / 1000;
	tv.tv_usec = (to_ms % 1000) * 1000;
	(void)setsockopt(sock, SOL_SOCKET, SO_SNDTIMEO, &tv, sizeof(tv));
	(void)setsockopt(sock, SOL_SOCKET, SO_RCVTIMEO, &tv, sizeof(tv));

	struct sockaddr_in addr;
	memset(&addr, 0, sizeof(addr));
	addr.sin_family = AF_INET;
	addr.sin_port   = htons((uint16_t)cli->port);
	addr.sin_addr   = ip;

	if (connect_timeout(sock, (struct sockaddr *)&addr, sizeof(addr), to_ms) < 0) {
		cli->last_errno = errno;
		close(sock);
		free(body);
		return WTGPUSH_E_CONNECT;
	}

	/* 4. 헤더 + body 송신. */
	if (send_all(sock, header, (size_t)hlen) < 0 ||
	    send_all(sock, body,   (size_t)body_len) < 0) {
		cli->last_errno = errno;
		close(sock);
		free(body);
		return WTGPUSH_E_SEND;
	}
	free(body);

	/* 5. 응답 수신 — status line 만 필요. 첫 chunk 만 읽고 close. */
	char resp[WTGPUSH_RESP_BUF];
	ssize_t n = recv(sock, resp, sizeof(resp) - 1, 0);
	close(sock);
	if (n <= 0) {
		cli->last_errno = errno;
		return WTGPUSH_E_RECV;
	}
	resp[n] = 0;

	int status = parse_http_status(resp);
	if (status < 0) return WTGPUSH_E_PARSE;
	cli->last_http_status = status;
	if (status >= 500) return WTGPUSH_E_HTTP_5XX;
	if (status >= 400) return WTGPUSH_E_HTTP_4XX;
	if (status != 200) return WTGPUSH_E_PARSE;
	return WTGPUSH_OK;
}

/* ─── 공개 API ─── */

int wtg_push_init(wtg_push_client_t *cli,
                  const char *host, int port,
                  const char *secret, int timeout_ms) {
	if (cli == NULL || host == NULL || port <= 0 || port > 65535) {
		return WTGPUSH_E_INVALID;
	}
	if (strlen(host) >= sizeof(cli->host)) return WTGPUSH_E_INVALID;
	memset(cli, 0, sizeof(*cli));
	strncpy(cli->host, host, sizeof(cli->host) - 1);
	cli->port = port;
	if (secret != NULL && secret[0] != 0) {
		if (strlen(secret) >= sizeof(cli->secret)) return WTGPUSH_E_INVALID;
		strncpy(cli->secret, secret, sizeof(cli->secret) - 1);
	}
	cli->timeout_ms = (timeout_ms > 0) ? timeout_ms : WTGPUSH_DEFAULT_TIMEOUT_MS;
	return WTGPUSH_OK;
}

int wtg_push_send(wtg_push_client_t *cli, const char *user, const char *json_data) {
	if (user == NULL || user[0] == 0) return WTGPUSH_E_INVALID;
	return do_push(cli, user, json_data);
}

int wtg_push_broadcast(wtg_push_client_t *cli, const char *json_data) {
	return do_push(cli, "", json_data);
}

const char *wtg_push_strerror(int code) {
	switch (code) {
	case WTGPUSH_OK:           return "OK";
	case WTGPUSH_E_INVALID:    return "invalid argument";
	case WTGPUSH_E_RESOLVE:    return "DNS resolve failed";
	case WTGPUSH_E_SOCKET:     return "socket() / setsockopt() failed";
	case WTGPUSH_E_CONNECT:    return "connect() failed or timed out";
	case WTGPUSH_E_SEND:       return "send() failed (broken pipe etc.)";
	case WTGPUSH_E_RECV:       return "recv() failed or timed out";
	case WTGPUSH_E_PARSE:      return "HTTP response parse failed";
	case WTGPUSH_E_HTTP_4XX:   return "HTTP 4xx (auth failed etc.) — check last_http_status";
	case WTGPUSH_E_HTTP_5XX:   return "HTTP 5xx (inject_full etc.) — check last_http_status";
	case WTGPUSH_E_OVERSIZE:   return "request too large (header/user/data exceed buffer)";
	default:                   return "unknown error";
	}
}
