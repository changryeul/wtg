
#ifndef __mbsise_h__
#define __mbsise_h__

#include "mtypes.h"  
#include "bmaster.h" 
#include "bcheg.h"  
#include "bhoga.h" 
#include "bmtick.h"  


/* 국채 시세 메모리 */
typedef struct 
{
	double              bPrc;   /* 기준가격,기준가액       */
	double              oPrc;	/* 시가                    */
	double              hPrc;	/* 고가                    */
	double              lPrc;	/* 저가                    */
	double              ePrc;   /* 종가(현재가)            */
	double              oYld;	/* 시가 수익률             */
	double              hYld;	/* 고가 수익률             */
	double              lYld;	/* 저가 수익률             */ 
	double              eYld;   /* 종가(현재가)수익률      */
	ulong               aVol;   /* 누적 거래량             */
	ldouble             aAmt;   /* 누적 거래금액           */
	double              yPrc;   /* 전일종가                */
	double              yYld;   /* 전일종가 수익률         */
	double              diff;   /* 전일대비                */		
	float               rate;   /* 전일대비 등락률         */
	char                sign;   /* 전일대비 부호 +(상승)/-(하락)/' '(보합) */ 
	char                ePcd;   /* 종가구분코드(마감) 1:실세 2:기세 3:거래무 4:시가기준가종목의 기세 */
	char                halt;   /* 거래정지 여부(정상, 거래정지)      */ 
	char               batOk;   /* 종목배치 수신여부 미수신(0)/수신완료(1) */
	YMD_T              batDt;   /* 종목배치 수신일         */
	char               clsOk;   /* 종가 수신여부     미수신(0)/수신완료(1) */ 
	char             evId[3];   /* 보드이벤트ID             */
	HMS_T              evsTm;   /* 보드이벤트 시작시간      */
	HMS_T              eveTm;   /* 보드이벤트 종료시간      */ 

	char            fill[40]; 
} BSISE_T;

typedef struct 
{
	char   bondCd[STD_CD_LEN];    /* 표준종목코드      */
	char            fill[  4];  
    char             ksNm[40];    /* 한글종목약어명	   */
    char             esNm[40];    /* 영문종목약어명	   */

	BMAST_T             bmast ;   /* 채권 마스터       */
	BSISE_T             bsise ;   /* 채권 시세         */
    BHOGA_T             bhoga ;   /* 채권 호가         */
    BCHEG_T             bcheg ;   /* 채권 체결         */
    BMTICK_T     bmtick[MAX_BMTICK];   /* 채권 1분봉   */
} KBOND_T; 
#define BMINX(hms) MIDX(hms,mbsise->oTime)

typedef struct 
{
	YMD_T       cDate;			/* 생성일자                */
	YMD_T       bday ;			/* 영업일자                */
	HMS_T       cTime;			/* 생성시간                */
	HMS_T       oTime;          /* 장시작 시간             */
	HMS_T       eTime;          /* 장종료 시간             */
	int         maxN ;			/* 최대갯수                */
	int         useN ;			/* 사용갯수                */
	HMS_T       evsTm;			/* 장운영 이벤트 발생 시간 */
	HMS_T       eveTm;			/* 장운영 이벤트 해제 시간 */
	char      evid[3];		    /* 장운영 이벤트코드       */ 

	char     fill[69];
	KBOND_T  bond[ 1];          /* 채권시세                */ 
} MBSISE_T;


#define SHM_MBSISE  "/mbsise"
#define F_MBSISE    SISE_DAT_PATH SHM_MBSISE ".dat" 
#define MAX_BITEM   500
#define MBSISE_SZ   (sizeof(MBSISE_T) + (sizeof(KBOND_T) * MAX_BITEM))


extern MBSISE_T* mbsise ;

extern char* l_trima(char*, int);
void 	shm_mbsync(const void *, size_t );
MBSISE_T*  l_mbopen(SHM_MODE );
KBOND_T*   l_mbread( const char* );
int        l_mbadd( const KBOND_T* );
int        l_mbdelete( const char* );
void       l_mbclose();


#endif

