/* sise read-shim drop-in 검증 — trn 이 쓰는 l_mfread/l_mbread 로 mci-price-krx SHM 읽기. */
#include <stdio.h>
#include "mfsise.h"
#include "mbsise.h"
int main(int argc, char **argv) {
	if (l_mfopen(SHM_READ)) {
		KBFUT_T *r = argc > 1 ? l_mfread(argv[1], NULL) : NULL;
		if (r) printf("fut  %.12s ePrc=%.2f yPrc=%.2f diff=%.2f sign=[%c]\n",
		              r->futCd, r->fsise.ePrc, r->fsise.yPrc, r->fsise.diff, r->fsise.sign ? r->fsise.sign : ' ');
		else printf("fut  %s NOT FOUND\n", argc > 1 ? argv[1] : "(none)");
	} else printf("l_mfopen fail (SHM 없음)\n");
	if (l_mbopen(SHM_READ)) {
		KBOND_T *b = argc > 2 ? l_mbread(argv[2]) : NULL;
		if (b) printf("bond %.12s ePrc=%.2f eYld=%.4f diff=%.2f\n",
		              b->bondCd, b->bsise.ePrc, b->bsise.eYld, b->bsise.diff);
		else printf("bond %s NOT FOUND\n", argc > 2 ? argv[2] : "(none)");
	} else printf("l_mbopen fail\n");
	return 0;
}
