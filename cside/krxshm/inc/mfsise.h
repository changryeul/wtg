
#ifndef __mfsise_h__
#define __mfsise_h__


#include "mtypes.h"  
#include "fmaster.h"  
#include "fcheg.h"  
#include "fhoga.h" 
#include "fmtick.h"  

/* 국채선물 시세 메모리 */
typedef struct 
{
	double           bPrc;   /* 기준가격,기준가액       */
	double           oPrc;	 /* 시가                    */
	double           hPrc;	 /* 고가                    */
	double           lPrc;	 /* 저가                    */
	double           ePrc;   /* 종가(현재가)            */
	ulong            aVol;   /* 누적 거래량             */
	ldouble          aAmt;   /* 누적 거래금액           */
	double           yPrc;   /* 전일종가                */
	double           diff;   /* 전일대비                */		

	double           sPrc;   /* 정산가격                */ 
	double           lsPr;   /* 최종결제가격            */
	float            rate;   /* 전일대비 등락률         */
	char          sPcd[2];   /* 정산가격구분코드
							   10:정산가없음      11:당일종가 (실세)    12:당일기세 (거래성립후기세) 
							   13:전일정산가(거래성립후 종가미형성)     14:당일이론가(거래성립후 종가미형성) 
							   15:스프레드분 종가 16:조정된 전일 정산가 17:대상자산 종가     18:정산기준가격    
							   40: 최우선매수호가(장종료시 양방호가가 있고 정산기준가격이 최우선매수호가와 같거나 
								   최우선매수호가를 하방으로 초과하는 경우) 
							   41: 최우선매도호가(장종료시 양방호가가 있고 정산기준가격이 최우선매도호가와 같거나 
								   최우선매도호가를 상방으로 초과하는 경우)  */
	char             lsPc;   /* 최종결제가격구분코드
							   1: 기초자산 종가 2: 산출식에 의해 계산된 값 3: 최종결제가격 없음 
							   4: 특별결제지수(SQ) 5: 최근일의 기초자산 종가(주식만 해당) 
							   6: 최근일의 기초자산 조정종가(주식만 해당) */

	char             sign;   /* 전일대비 부호 +(상승)/-(하락)/' '(보합) */ 


	char             ePcd;   /* 종가구분코드(마감) 1:실세 2:기세 3:거래무 4:시가기준가종목의 기세 */
	char             halt;   /* 거래정지 여부(정상, 거래정지)      */

	char            batOk;   /* 종목배치 수신여부 미수신(0)/수신완료(1) */
	char            clsOk;   /* 종가 수신여부     미수신(0)/수신완료(1) */
	YMD_T           batDt;   /* 종목배치 수신일          */

	HMS_T           evsTm;   /* 보드이벤트 시작시간      */
	HMS_T           eveTm;   /* 보드이벤트 종료시간      */ 
	char          evId[3];   /* 보드이벤트ID             */
	char         fill1[5];

	YMD_T            ltDt;   /* 최종거래일자             */
	YMD_T            lsDt;   /* 최종결제일자             */ 

	char        fill2[80]; 
} FSISE_T;

typedef struct 
{
	char    futCd[STD_CD_LEN];     /* 표준종목코드       */
	char          shrtCd[  9];     /* 종목단축코드		*/
	char          baseCd[ 12];     /* 기초자산종목코드   */
	char            fill[  7];  
    char            ksNm[ 40];     /* 종목약어명         */
    char            esNm[ 40];     /* 영문종목약어명     */

	FMAST_T             fmast ;   /* 국채선물 마스터    */
	FSISE_T             fsise ;   /* 국채선물 시세      */
    FHOGA_T             fhoga ;   /* 국채선물 호가      */
    FCHEG_T             fcheg ;   /* 국채선물 체결      */
    FMTICK_T   fmtick[MAX_FMTICK];   /* 국채선물 1분봉     */
} KBFUT_T; 
#define FMINX(hms) MIDX(hms,mfsise->oTime)
#define FMNINX(hms) MIDX(hms,mfnsise->oTime)

typedef struct 
{
	YMD_T       cDate;			/* 생성일자 */
	YMD_T       bday ;			/* 영업일자 */
	HMS_T       cTime;			/* 생성시간 */
	HMS_T       oTime;          /* 장시작 시간 */
	HMS_T       eTime;          /* 장종료 시간 */
	int         maxN ;			/* 최대갯수    */
	int         useN ;			/* 사용갯수    */

	HMS_T       evsTm;			/* 장운영 이벤트 발생 시간 */
	HMS_T       eveTm;			/* 장운영 이벤트 해제 시간 */
	char      evid[3];		    /* 장운영 이벤트코드    */

	char    fill[ 69];
	KBFUT_T kbfut[ 1];          /* 국채선물시세           */ 
} MFSISE_T;


#define SHM_MFSISE  "/mfsise"
#define SHM_MFNSISE  "/mfnsise"
#define F_MFSISE    SISE_DAT_PATH SHM_MFSISE ".dat" 
#define F_MFNSISE   SISE_DAT_PATH SHM_MFNSISE ".dat" 
#define MAX_FITEM   400
#define MFSISE_SZ   (sizeof(MFSISE_T) + (sizeof(KBFUT_T) * MAX_FITEM))
#define MFNSISE_SZ   (sizeof(MFSISE_T) + (sizeof(KBFUT_T) * MAX_FITEM))


extern MFSISE_T* mfsise ;
extern MFSISE_T* mfnsise ;

void shm_mfsync(const void *, size_t );
MFSISE_T* l_mfopen(SHM_MODE );
KBFUT_T*  l_mfread(const char* code, const char* scode);
int       l_mfadd(const KBFUT_T* );
int       l_mfdelete(const char* );
void      l_mfclose();

extern MFSISE_T* mfnsise ;

void shm_mfnsync(const void *, size_t );
MFSISE_T* l_mfnopen(SHM_MODE );
KBFUT_T*  l_mfnread(const char* code, const char* scode);
int       l_mfnadd(const KBFUT_T* );
int       l_mfndelete(const char* );
void      l_mfnclose();
#endif
