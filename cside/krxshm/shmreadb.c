/*
 * shmreadb — /dev/shm/mbsise(MBSISE_T, 채권)를 실제 sise 구조체로 읽는다.
 * mci-price-krx(Go)가 write 한 채권 SHM 을 C 가 l_mbread 와 동일하게(bsearch by bondCd)
 * 읽어 byte-exact 호환 증명. libmbsise 불요.
 * 빌드: cc -I cside/krxshm/inc shmreadb.c -o shmreadb   사용: shmreadb <shm_path> <code>
 */
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <fcntl.h>
#include <unistd.h>
#include <sys/mman.h>
#include "mbsise.h"

static int cmp_bondCd(const void *a, const void *b) {
	return strncmp(((const KBOND_T *)a)->bondCd, ((const KBOND_T *)b)->bondCd, STD_CD_LEN);
}

int main(int argc, char **argv) {
	if (argc < 3) {
		fprintf(stderr, "usage: %s <shm_path> <code>\n", argv[0]);
		return 2;
	}
	int fd = open(argv[1], O_RDONLY);
	if (fd < 0) { perror("open"); return 1; }
	size_t sz = MBSISE_SZ;
	MBSISE_T *m = mmap(NULL, sz, PROT_READ, MAP_SHARED, fd, 0);
	if (m == MAP_FAILED) { perror("mmap"); return 1; }

	printf("useN=%d maxN=%d (mmap %zu B)\n", m->useN, m->maxN, sz);
	KBOND_T key;
	memset(&key, 0, sizeof(key));
	strncpy(key.bondCd, argv[2], STD_CD_LEN);
	KBOND_T *r = (KBOND_T *)bsearch(&key, m->bond, m->useN, sizeof(KBOND_T), cmp_bondCd);
	if (!r) { printf("NOT FOUND: %s\n", argv[2]); return 1; }

	BSISE_T *s = &r->bsise;
	BHOGA_T *h = &r->bhoga;
	printf("code=%.12s ePrc=%.2f bPrc=%.2f eYld=%.4f diff=%.2f yPrc=%.2f rate=%.4f sign=[%c]\n",
	       r->bondCd, s->ePrc, s->bPrc, s->eYld, s->diff, s->yPrc, (double)s->rate, s->sign ? s->sign : ' ');
	printf("  hoga: stVol=%lu btVol=%lu ask0=%.2f/%lu(yld %.4f) bid0=%.2f/%lu(yld %.4f)\n",
	       h->stVol, h->btVol, h->shoga[0].prc, h->shoga[0].vol, h->shoga[0].yld,
	       h->bhoga[0].prc, h->bhoga[0].vol, h->bhoga[0].yld);
	return 0;
}
