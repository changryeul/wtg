/*
 * krxverify oracle — KRX 원 TR 을 "실제 sise 구조체(A306F_T 등)"로 캐스팅해
 * 각 필드를 l_s2d(=trim+atof) 로 파싱, 정규 CSV 로 출력한다. WTG Go 디코더가
 * 동일 바이트를 읽은 결과(cmd/krx-verify decode)와 diff 하면 오프셋/파싱을
 * C 구조체 레이아웃 기준으로 런타임 대조하게 된다.
 *
 * 입력: length-prefixed(4B BE) 원 TR 레코드 스트림 파일.
 * 빌드: cc -I<sise inc> oracle.c -o oracle  (sise .h 는 순수 char[] — 무의존)
 */
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <ctype.h>

#include "A306F.h"
#include "A301K.h"
#include "B606F.h"
#include "B601K.h"
#include "H306F.h"
#include "A006F.h"
#include "A001B.h"

/* trim 후 double (C sise l_s2d 와 동형: 선행/후행 공백 제거 후 atof). */
static double s2d(const char *p, int n) {
	char buf[64];
	int i = 0, j;
	if (n >= (int)sizeof(buf)) n = sizeof(buf) - 1;
	for (j = 0; j < n; j++) buf[j] = p[j];
	buf[n] = 0;
	/* 좌우 trim */
	char *s = buf;
	while (*s == ' ') s++;
	int e = strlen(s);
	while (e > 0 && (s[e-1] == ' ' || s[e-1] == '\n')) s[--e] = 0;
	if (s[0] == 0) return 0.0;
	return atof(s);
	(void)i;
}
static long long s2l(const char *p, int n) {
	char buf[64];
	int j;
	if (n >= (int)sizeof(buf)) n = sizeof(buf) - 1;
	for (j = 0; j < n; j++) buf[j] = p[j];
	buf[n] = 0;
	char *s = buf;
	while (*s == ' ') s++;
	int e = strlen(s);
	while (e > 0 && s[e-1] == ' ') s[--e] = 0;
	if (s[0] == 0) return 0;
	return atoll(s);
}
/* trim 문자열 → 정적 버퍼 (CSV 값용). 한 printf 안에서 여러 번 호출되므로
 * 링 버퍼로 aliasing 을 피한다 (단일 static 이면 마지막 값이 앞을 덮어씀). */
static const char *s2s(const char *p, int n) {
	static char ring[16][128];
	static int rr = 0;
	char *buf = ring[rr++ & 15];
	int j;
	if (n >= 128) n = 127;
	for (j = 0; j < n; j++) buf[j] = p[j];
	buf[n] = 0;
	char *s = buf;
	while (*s == ' ') s++;
	int e = strlen(s);
	while (e > 0 && s[e-1] == ' ') s[--e] = 0;
	return s;
}

static void emit_A306F(const char *b) {
	const A306F_T *t = (const A306F_T *)b;
	printf("A306F,%s,last=%.4f,open=%.4f,high=%.4f,low=%.4f,near=%.4f,far=%.4f,pprc=%.4f,"
	       "cvol=%lld,tvol=%lld,tamt=%.4f,uplim=%.4f,dnlim=%.4f,bs=%s\n",
	       s2s(t->code, 12),
	       s2d(t->cprc, 9), s2d(t->oprc, 9), s2d(t->hprc, 9), s2d(t->lprc, 9),
	       s2d(t->nprc, 9), s2d(t->fprc, 9), s2d(t->pprc, 9),
	       s2l(t->cvol, 9), s2l(t->tvol, 12), s2d(t->tamt, 22),
	       s2d(t->uldp, 9), s2d(t->lldp, 9), s2s(t->ftcd, 1));
}

static void emit_A301K(const char *b) {
	const A301K_T *t = (const A301K_T *)b;
	printf("A301K,%s,last=%.4f,yld=%.6f,cvol=%lld,camt=%.4f,open=%.4f,high=%.4f,low=%.4f,"
	       "oyld=%.6f,hyld=%.6f,lyld=%.6f,tvol=%lld,tamt=%.4f\n",
	       s2s(t->code, 12),
	       s2d(t->cprc, 11), s2d(t->tyld, 13), s2l(t->cvol, 10), s2d(t->camt, 22),
	       s2d(t->oprc, 11), s2d(t->hprc, 11), s2d(t->lprc, 11),
	       s2d(t->oyld, 13), s2d(t->hyld, 13), s2d(t->lyld, 13),
	       s2l(t->tvol, 15), s2d(t->tamt, 22));
}

static void emit_B606F(const char *b) {
	const B606F_T *t = (const B606F_T *)b;
	printf("B606F,%s,askTot=%lld,bidTot=%lld,askCnt=%lld,bidCnt=%lld,exp=%.4f,expVol=%lld",
	       s2s(t->code, 12), s2l(t->stvl, 9), s2l(t->btvl, 9),
	       s2l(t->apvc, 5), s2l(t->bpvc, 5), s2d(t->etpr, 9), s2l(t->etvl, 9));
	for (int i = 0; i < 5; i++)
		printf(",ask%d=%.4f/%lld/%lld", i, s2d(t->hoga[i].sprc, 9),
		       s2l(t->hoga[i].svol, 9), s2l(t->hoga[i].scnt, 5));
	for (int i = 0; i < 5; i++)
		printf(",bid%d=%.4f/%lld/%lld", i, s2d(t->hoga[i].bprc, 9),
		       s2l(t->hoga[i].bvol, 9), s2l(t->hoga[i].bcnt, 5));
	printf("\n");
}

static void emit_B601K(const char *b) {
	const B601K_T *t = (const B601K_T *)b;
	printf("B601K,%s,askTot=%lld,bidTot=%lld", s2s(t->code, 12),
	       s2l(t->stvl, 15), s2l(t->btvl, 15));
	for (int i = 0; i < 5; i++)
		printf(",ask%d=%.4f/%lld/%.6f", i, s2d(t->hoga[i].sprc, 11),
		       s2l(t->hoga[i].svol, 15), s2d(t->hoga[i].syld, 13));
	for (int i = 0; i < 5; i++)
		printf(",bid%d=%.4f/%lld/%.6f", i, s2d(t->hoga[i].bprc, 11),
		       s2l(t->hoga[i].bvol, 15), s2d(t->hoga[i].byld, 13));
	printf("\n");
}

static void emit_H306F(const char *b) {
	const H306F_T *t = (const H306F_T *)b;
	printf("H306F,%s,settle=%.4f,settleCd=%s,final=%.4f,finalCd=%s\n",
	       s2s(t->code, 12), s2d(t->sprc, 18), s2s(t->spcd, 2),
	       s2d(t->lspr, 8), s2s(t->lspc, 1));
}

static void emit_A006F(const char *b) {
	const A006F_T *t = (const A006F_T *)b;
	printf("A006F,%s,base=%.4f,prev=%.4f,uplim=%.4f,dnlim=%.4f,strike=%.4f,mult=%.4f,unit=%.4f,"
	       "prevOI=%lld,iv=%.4f,focd=%s,recd=%s,atmc=%s,halt=%s,uacd=%s\n",
	       s2s(t->code, 12), s2d(t->bprc, 11), s2d(t->yprc, 11),
	       s2d(t->upl1, 11), s2d(t->lpl1, 11), s2d(t->eprc, 18),
	       s2d(t->mult, 22), s2d(t->unit, 22), s2l(t->pdoi, 12), s2d(t->ipvl, 11),
	       s2s(t->focd, 1), s2s(t->recd, 1), s2s(t->atmc, 1), s2s(t->halt, 1),
	       s2s(t->uacd, 12));
}

static void emit_A001B(const char *b) {
	const A001B_T *t = (const A001B_T *)b;
	printf("A001B,%s,base=%.4f,coupon=%.6f,issueRate=%.6f\n",
	       s2s(t->code, 12), s2d(t->bprc, 11), s2d(t->cprt, 14), s2d(t->isrt, 13));
}

int main(int argc, char **argv) {
	if (argc < 2) {
		fprintf(stderr, "usage: %s <capture.dat>\n", argv[0]);
		return 2;
	}
	FILE *fp = fopen(argv[1], "rb");
	if (!fp) { perror("fopen"); return 1; }

	unsigned char lp[4];
	static char buf[70000];
	while (fread(lp, 1, 4, fp) == 4) {
		unsigned int n = (lp[0] << 24) | (lp[1] << 16) | (lp[2] << 8) | lp[3];
		if (n == 0 || n > sizeof(buf)) break;
		if (fread(buf, 1, n, fp) != n) break;
		if (n < 5) continue;
		if      (!memcmp(buf, "A306F", 5)) emit_A306F(buf);
		else if (!memcmp(buf, "A301K", 5)) emit_A301K(buf);
		else if (!memcmp(buf, "B606F", 5)) emit_B606F(buf);
		else if (!memcmp(buf, "B601K", 5)) emit_B601K(buf);
		else if (!memcmp(buf, "H306F", 5)) emit_H306F(buf);
		else if (!memcmp(buf, "A006F", 5)) emit_A006F(buf);
		else if (!memcmp(buf, "A001B", 5)) emit_A001B(buf);
	}
	fclose(fp);
	return 0;
}
