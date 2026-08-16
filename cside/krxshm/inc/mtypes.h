
#ifndef __mtypes_h__
#define __mtypes_h__

#include <stdio.h>
#include <limits.h> 
#include <unistd.h>
#include <stdlib.h>
#include <stdarg.h>
#include <sys/types.h>
#include <sys/time.h>
#include <string.h>
#include <ctype.h>
#include <time.h> 
#include <sys/mman.h>
#include <sys/stat.h>
#include <fcntl.h>
#include <errno.h>
#include <semaphore.h>

extern int errno;
extern char emsg[1024];

#ifndef ushort
typedef unsigned short ushort ;
#endif
#ifndef uint
typedef unsigned int uint;
#endif
#ifndef ulong
typedef unsigned long ulong ;
#endif
#ifndef llong
typedef long long llong ;
#endif
#ifndef ldouble
typedef long double ldouble;
#endif

#define STD_CD_LEN  12
#define SHT_CD_LEN  9

typedef enum
{
	SHM_READ,   /* READ MODE      */ 
	SHM_RDWR    /* READ & WRITE MODE */
} SHM_MODE;

typedef struct 
{
	char     hh;      /* 시  0-23 */
	char     mi;      /* 분  0-59 */
	char     ss;      /* 초  0-59 */
	char     tp;      /* 24시간제 여부 0:24 1:12 */
	uint     ms;      /* 마이크로초  */
} HMS_T; /* size:8 */ 

typedef struct 
{
	short     yy;      /* 년  yyyy */
	char      mm;      /* 월  mm   */
	char      dd;      /* 일  dd   */ 
} YMD_T; /* size:4 */

typedef struct 
{
	short     yy;      /* 년  yyyy */
	char      mm;      /* 월  mm   */
	char      dd;      /* 일  dd   */ 
	short     yd;      /* day in the year 0~365 */
	char      wd;      /* day of the week 0~6 */
	char      sv;      /* daylight saving time */
} YMDE_T; /* size:8 */

typedef struct 
{
	short     yy;      /* 년  yyyy */
	char      mm;      /* 월  mm   */
	char      dd;      /* 일  dd   */ 
	char      hh;      /* 시  0-23 */
	char      mi;      /* 분  0-59 */
	char      ss;      /* 초  0-59 */
	char      tp;      /* 24시간제 여부 0:24 1:12 */
	uint      ms;      /* 마이크로초   */
	short     yd;      /* day in the year 0~365 */
	char      wd;      /* day of the week 0~6 */
	char      sv;      /* daylight saving time */
} DTM_T; /* size:16 */


char colord(double val, double base);
char colorl(ulong val, ulong base);
void l_getCurYMD(YMD_T *ymd);
void l_getCurHMS(HMS_T *hms);
void l_getCurDTM(DTM_T *dtm);
void l_str2ymd(char *strDate, YMD_T *ymd);
char* l_ymd2str(YMD_T *ymd, char *strDate, int buflen);
HMS_T* l_str2hms(char *strTime, HMS_T *hms, char msFlg);
char* l_hms2str(HMS_T *hms, char *strTime, int buflen, char msFlg);
DTM_T* l_str2dtm(char *strDateTm, DTM_T *dtm, char msFlg) ;
char* l_dtm2str(DTM_T *dtm, char *strDateTm, int buflen, char msFlg);
int l_s2i(char *s, uint l) ;
uint l_s2ui(char *s, uint l) ;
long l_s2l(char *s, uint l);
ulong l_s2ul(char *s, uint l);
llong l_s2ll(char *s, uint l);
float l_s2f(char *s, uint l);
double l_s2d(char *s, uint l);
ldouble l_s2ld(char *s, uint l);
int l_trimr(char *s, int l);
int l_triml(char *s, int l);
char* l_trima(char *s, int l);





#endif
