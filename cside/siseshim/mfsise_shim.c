/*
 * libmfsise read-shim — 파생 시세 SHM(/dev/shm/mfsise) 읽기 전용 클라이언트.
 * 벤더 sise 피드 대신 mci-price-krx(Go)가 SHM 을 채우므로, trn/mon AP 는 이 shim 의
 * l_mfopen/l_mfread 로 무수정 read. 원 mfsise.h(l_mfopen/l_mfread/l_mfclose) 시그니처
 * 동일 — drop-in. SLOG/emsg(comsise) 미사용 self-contained.
 *
 * 빌드: cc -I<inc> -c mfsise_shim.c ; ar rcs libmfsise.a mfsise_shim.o
 */
#define _GNU_SOURCE
#include <stdio.h>
#include <string.h>
#include <stdlib.h>
#include <fcntl.h>
#include <unistd.h>
#include <sys/mman.h>
#include "mfsise.h"

MFSISE_T *mfsise = NULL;

static int cmp_futCd(const void *a, const void *b) {
	return strncmp(((const KBFUT_T *)a)->futCd, ((const KBFUT_T *)b)->futCd, STD_CD_LEN);
}

MFSISE_T *l_mfopen(SHM_MODE mode) {
	if (mfsise) return mfsise;
	int fd = shm_open(SHM_MFSISE, mode == SHM_RDWR ? O_RDWR : O_RDONLY, 0666);
	if (fd < 0) return NULL;
	int prot = mode == SHM_RDWR ? (PROT_READ | PROT_WRITE) : PROT_READ;
	void *p = mmap(NULL, MFSISE_SZ, prot, MAP_SHARED, fd, 0);
	close(fd);
	if (p == MAP_FAILED) return NULL;
	mfsise = (MFSISE_T *)p;
	return mfsise;
}

KBFUT_T *l_mfread(const char *code, const char *scode) {
	if (mfsise == NULL || mfsise->useN <= 0 || (code == NULL && scode == NULL)) return NULL;
	KBFUT_T key;
	memset(&key, 0, sizeof(key));
	if (code != NULL) {
		strncpy(key.futCd, code, STD_CD_LEN);
		return (KBFUT_T *)bsearch(&key, mfsise->kbfut, mfsise->useN, sizeof(KBFUT_T), cmp_futCd);
	}
	/* 단축코드는 SHM 이 futCd 정렬이라 bsearch 불가 → 선형 스캔 (안전) */
	for (int i = 0; i < mfsise->useN; i++) {
		if (strncmp(mfsise->kbfut[i].shrtCd, scode, SHT_CD_LEN) == 0) return &mfsise->kbfut[i];
	}
	return NULL;
}

void l_mfclose(void) {
	if (mfsise) {
		munmap(mfsise, MFSISE_SZ);
		mfsise = NULL;
	}
}
