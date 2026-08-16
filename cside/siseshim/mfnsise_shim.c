/* libmfnsise read-shim — 야간 파생 시세 SHM(/dev/shm/mfnsise, MFSISE_T 동형) 읽기 전용. */
#define _GNU_SOURCE
#include <string.h>
#include <stdlib.h>
#include <fcntl.h>
#include <unistd.h>
#include <sys/mman.h>
#include "mfsise.h"

MFSISE_T *mfnsise = NULL;

static int cmp_futCd(const void *a, const void *b) {
	return strncmp(((const KBFUT_T *)a)->futCd, ((const KBFUT_T *)b)->futCd, STD_CD_LEN);
}

MFSISE_T *l_mfnopen(SHM_MODE mode) {
	if (mfnsise) return mfnsise;
	int fd = shm_open(SHM_MFNSISE, mode == SHM_RDWR ? O_RDWR : O_RDONLY, 0666);
	if (fd < 0) return NULL;
	int prot = mode == SHM_RDWR ? (PROT_READ | PROT_WRITE) : PROT_READ;
	void *p = mmap(NULL, MFNSISE_SZ, prot, MAP_SHARED, fd, 0);
	close(fd);
	if (p == MAP_FAILED) return NULL;
	mfnsise = (MFSISE_T *)p;
	return mfnsise;
}

KBFUT_T *l_mfnread(const char *code, const char *scode) {
	if (mfnsise == NULL || mfnsise->useN <= 0 || (code == NULL && scode == NULL)) return NULL;
	KBFUT_T key;
	memset(&key, 0, sizeof(key));
	if (code != NULL) {
		strncpy(key.futCd, code, STD_CD_LEN);
		return (KBFUT_T *)bsearch(&key, mfnsise->kbfut, mfnsise->useN, sizeof(KBFUT_T), cmp_futCd);
	}
	for (int i = 0; i < mfnsise->useN; i++)
		if (strncmp(mfnsise->kbfut[i].shrtCd, scode, SHT_CD_LEN) == 0) return &mfnsise->kbfut[i];
	return NULL;
}

void l_mfnclose(void) {
	if (mfnsise) { munmap(mfnsise, MFNSISE_SZ); mfnsise = NULL; }
}
