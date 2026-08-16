
#ifndef __fmtick_h__
#define __fmtick_h__

#define MAX_FMTICK   (12*60) 
typedef struct 
{ 
	HMS_T               time;   /* 시간 hhmi      */ 
	float               rate;   /* 등락률         */              
	char                sign;   /* 대비부호 +(상승)/-(하락)/' '(보합) */ 
	char             fill[3]; 
	double              diff;   /* 대비           */
	double              oPrc;	/* 시가           */
	double              hPrc;	/* 고가           */
	double              lPrc;	/* 저가           */
	double              ePrc;	/* 종가           */ 
	double              pPrc;   /* 직전가격       */ 
	ulong               cVol;   /* 거래량         */
	ldouble             cAmt;   /* 거래대금       */
} FMTICK_T; 


#endif
