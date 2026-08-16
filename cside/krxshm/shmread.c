/*
 * shmread — /dev/shm/mfsise(MFSISE_T)를 실제 sise 구조체로 매핑해 종목 시세를 읽는다.
 * mci-price-krx(Go)가 write 한 SHM 을 C 가 l_mfread 와 동일하게(bsearch by futCd) 읽어
 * byte-exact 호환을 증명. libmfsise 불요 — vendored 헤더의 MFSISE_T/KBFUT_T 그대로 사용.
 *
 * 빌드: cc -I cside/krxshm/inc shmread.c -o shmread
 * 사용: shmread <shm_path> <종목코드>
 */
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <fcntl.h>
#include <unistd.h>
#include <sys/mman.h>
#include <sys/stat.h>
#include "mfsise.h"

static int cmp_futCd(const void *a, const void *b) {
	return strncmp(((const KBFUT_T *)a)->futCd, ((const KBFUT_T *)b)->futCd, STD_CD_LEN);
}

int main(int argc, char **argv) {
	if (argc < 3) {
		fprintf(stderr, "usage: %s <shm_path> <code>\n", argv[0]);
		return 2;
	}
	int fd = open(argv[1], O_RDONLY);
	if (fd < 0) { perror("open"); return 1; }
	size_t sz = MFSISE_SZ;
	MFSISE_T *m = mmap(NULL, sz, PROT_READ, MAP_SHARED, fd, 0);
	if (m == MAP_FAILED) { perror("mmap"); return 1; }

	printf("useN=%d maxN=%d (mmap %zu B)\n", m->useN, m->maxN, sz);
	KBFUT_T key;
	memset(&key, 0, sizeof(key));
	strncpy(key.futCd, argv[2], STD_CD_LEN);
	KBFUT_T *r = (KBFUT_T *)bsearch(&key, m->kbfut, m->useN, sizeof(KBFUT_T), cmp_futCd);
	if (!r) { printf("NOT FOUND: %s\n", argv[2]); return 1; }

	FSISE_T *s = &r->fsise;
	printf("code=%.12s short=%.9s ePrc=%.2f bPrc=%.2f yPrc=%.2f diff=%.2f sPrc=%.2f rate=%.4f sign=[%c]\n",
	       r->futCd, r->shrtCd, s->ePrc, s->bPrc, s->yPrc, s->diff, s->sPrc, (double)s->rate, s->sign ? s->sign : ' ');
	return 0;
}
