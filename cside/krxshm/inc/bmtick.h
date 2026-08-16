
#ifndef __bmtick_h__
#define __bmtick_h__

#define MAX_BMTICK   (12*60) 
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
	double              oYld;	/* 시가수익률     */
	double              hYld;	/* 고가수익률     */
	double              lYld;	/* 저가수익률     */
	double              eYld;	/* 종가수익률     */ 
	double              pPrc;   /* 직전가            */
	ulong               cVol;   /* 거래량         */
	ldouble             cAmt;   /* 거래대금       */
} BMTICK_T;


#endif
