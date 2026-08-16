
#ifndef __bcheg_h__
#define __bcheg_h__


typedef struct
{	
	HMS_T      time;      /* 체결시간 hhmissuuuuuu   */   
	YMD_T      dTrd;      /* 거래일자          */
	YMD_T      dSet;      /* 결제일자                */
	double     cPrc;      /* 체결가                  */
	ulong      cVol;      /* 거래량                  */
	ldouble    cAmt;      /* 거래대금                */
	double     cYld;      /* 체결수익률              */	
	double     oPrc;      /* 시가                    */
	double     hPrc;      /* 고가                    */
	double     lPrc;      /* 저가                    */
	double     oYld;      /* 시가수익률              */
	double     hYld;      /* 고가수익률              */
	double     lYld;      /* 저가수익률              */

	double     cDif;      /* 직전대비          */
	float      cRat;      /* 직전대비 등락률   */
	char       sign;      /* 직전대비 부호 +(상승)/-(하락)/' '(보합) */ 
	char   fill[ 3];
	
} BCHEG_U;

#define MAX_BCHEG	360

typedef struct
{
	ulong        xSeq;   	/* KRX체결 일련번호  */ 
	ulong        tVol;   	/* 누적체결수량      */
	ldouble      tAmt;   	/* 누적거래대금      */ 
	double       pPrc;      /* 직전가            */
	double       cPrc;      /* 현재가            */
	double       cYld;      /* 현재가수익률      */
	double       cDif;      /* 직전대비          */
	float        cRat;      /* 직전대비 등락률   */
	ushort       useN;      /* 현재 체결슬롯 사용 갯수       */
	ushort       cIdx;      /* 현재 사용슬롯 인덱스 0 ~ (MAX_BCHEG-1) */ 
	char         sign;      /* 직전대비 부호 +(상승)/-(하락)/' '(보합) */ 
	char      fill[7];
	
	BCHEG_U  cheg[MAX_BCHEG];   /* 체결 데이터           */
} BCHEG_T;

/* for data manager ap */
#define IDX_NBC(cheg) ((cheg->useN<=0)? 0:((cheg->cIdx<(MAX_BCHEG-1))? cheg->cIdx+1:0)) 
#define USE_NBC(useN) ((useN<=0)? 1:(useN>=MAX_BCHEG)? MAX_BCHEG:useN+1) 

/* for data consumer ap */
#define IDX_RBC(useN,cIdx) ((useN<MAX_BCHEG)? 0:(cIdx<(MAX_BCHEG-1))? cIdx+1:0)  
#define IDX_NBX(cidx) ((cidx<(MAX_BCHEG-1))? cidx+1:0)


#endif
