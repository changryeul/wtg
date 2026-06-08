/*
 * sample.c — wtgpush SDK 사용 예 (PoC).
 *
 * 빌드:
 *   make
 *   ./sample <host> <port> <secret> <user> '<json>'
 *
 * 예:
 *   ./sample 127.0.0.1 8081 mysecret dealer01 '{"orderId":123,"price":1.0850}'
 *   ./sample 127.0.0.1 8081 mysecret ""       '{"market":"HALT"}'   # broadcast
 */

#include "wtgpush.h"

#include <stdio.h>
#include <stdlib.h>
#include <string.h>

int main(int argc, char *argv[]) {
	if (argc != 6) {
		fprintf(stderr,
		    "usage: %s <host> <port> <secret> <user (\"\" for broadcast)> '<json-data>'\n",
		    argv[0]);
		return 2;
	}
	const char *host   = argv[1];
	int         port   = atoi(argv[2]);
	const char *secret = argv[3];
	const char *user   = argv[4];
	const char *data   = argv[5];

	wtg_push_client_t cli;
	int rc = wtg_push_init(&cli, host, port, secret, 2000);
	if (rc != WTGPUSH_OK) {
		fprintf(stderr, "init: %s\n", wtg_push_strerror(rc));
		return 1;
	}

	if (user == NULL || user[0] == 0) {
		rc = wtg_push_broadcast(&cli, data);
		fprintf(stderr, "broadcast → %d (%s) http=%d errno=%d\n",
		        rc, wtg_push_strerror(rc), cli.last_http_status, cli.last_errno);
	} else {
		rc = wtg_push_send(&cli, user, data);
		fprintf(stderr, "send(%s) → %d (%s) http=%d errno=%d\n",
		        user, rc, wtg_push_strerror(rc), cli.last_http_status, cli.last_errno);
	}

	return (rc == WTGPUSH_OK) ? 0 : 1;
}
