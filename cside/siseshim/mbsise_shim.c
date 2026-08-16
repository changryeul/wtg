/* libmbsise read-shim — 채권 시세 SHM(/dev/shm/mbsise) 읽기 전용. l_mbopen/l_mbread/l_mbclose. */
#define _GNU_SOURCE
#include <string.h>
#include <stdlib.h>
#include <fcntl.h>
#include <unistd.h>
#include <sys/mman.h>
#include "mbsise.h"

MBSISE_T *mbsise = NULL;

static int cmp_bondCd(const void *a, const void *b) {
	return strncmp(((const KBOND_T *)a)->bondCd, ((const KBOND_T *)b)->bondCd, STD_CD_LEN);
}

MBSISE_T *l_mbopen(SHM_MODE mode) {
	if (mbsise) return mbsise;
	int fd = shm_open(SHM_MBSISE, mode == SHM_RDWR ? O_RDWR : O_RDONLY, 0666);
	if (fd < 0) return NULL;
	int prot = mode == SHM_RDWR ? (PROT_READ | PROT_WRITE) : PROT_READ;
	void *p = mmap(NULL, MBSISE_SZ, prot, MAP_SHARED, fd, 0);
	close(fd);
	if (p == MAP_FAILED) return NULL;
	mbsise = (MBSISE_T *)p;
	return mbsise;
}

KBOND_T *l_mbread(const char *code) {
	if (mbsise == NULL || mbsise->useN <= 0 || code == NULL) return NULL;
	KBOND_T key;
	memset(&key, 0, sizeof(key));
	strncpy(key.bondCd, code, STD_CD_LEN);
	return (KBOND_T *)bsearch(&key, mbsise->bond, mbsise->useN, sizeof(KBOND_T), cmp_bondCd);
}

void l_mbclose(void) {
	if (mbsise) { munmap(mbsise, MBSISE_SZ); mbsise = NULL; }
}
