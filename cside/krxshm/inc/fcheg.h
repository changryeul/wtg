
#ifndef __fcheg_h__
#define __fcheg_h__


typedef struct
{	
	HMS_T      time;   /* 체결시간 hhmissuuuuuu   */   
	float      cRat;   /* 직전대비 등락률   */
	char       bsCd;   /* 최종매도매수구분코드 space' ':단일가체결 0:해당없음 1:매도 2:매수 */
	char       sign;   /* 직전대비 부호 +(상승)/-(하락)/' '(보합) */ 
	char   fill[ 2];
	double     cPrc;   /* 체결가                  */
	ulong      cVol;   /* 거래량                  */
	ldouble    cAmt;   /* 거래대금                */
	double     nPrc;   /* 근월물체결가격          */
	double     fPrc;   /* 원월물체결가격          */
	double     oPrc;   /* 시가                    */
	double     hPrc;   /* 고가                    */
	double     lPrc;   /* 저가                    */
	double     pPrc;   /* 직전가격                */ 
	double     cDif;   /* 직전대비          */
} FCHEG_U;

#define MAX_FCHEG	360
typedef struct
{
	ulong        xSeq;   	/* KRX체결 일련번호  */ 
	ulong        tVol;   	/* 누적 체결수량     */
	ldouble      tAmt;   	/* 누적 거래대금     */ 
	double       cPrc;      /* 현재가            */
	double      ulPrc;      /* 동적상한가        */
	double      llPrc;      /* 동적하한가        */ 
	double       pPrc;      /* 직전가            */
	double       cDif;      /* 직전대비          */
	float        cRat;      /* 직전대비 등락률   */
	ushort       useN;      /* 현재 체결슬롯 사용 갯수       */
	ushort       cIdx;      /* 현재 사용슬롯 인덱스 0 ~ (MAX_FCHEG-1) */ 
	char         sign;      /* 직전대비 부호 +(상승)/-(하락)/' '(보합) */ 
	char     fill[ 7]; 
	FCHEG_U  cheg[MAX_FCHEG];   /* 채결 데이터           */
} FCHEG_T;

/* for data manager ap */
#define IDX_NFC(cheg) ((cheg->useN<=0)? 0:((cheg->cIdx<(MAX_FCHEG-1))? cheg->cIdx+1:0)) 
#define USE_NFC(useN) ((useN<=0)? 1:(useN>=MAX_FCHEG)? MAX_FCHEG:useN+1) 

/* for data consumer ap */
#define IDX_RFC(useN,cIdx) ((useN<MAX_FCHEG)? 0:(cIdx<(MAX_FCHEG-1))? cIdx+1:0)  
#define IDX_NFX(cidx) ((cidx<(MAX_FCHEG-1))? cidx+1:0)

#endif
