
#ifndef __bhoga_h__
#define __bhoga_h__

#define N_BHOGA 5

typedef struct 
{
	double   prc ;   /* 호가        */
	ulong    vol ;   /* 호가잔량    */
	double   yld ;   /* 호가수익률  */
} BHOGA_U;

typedef struct 
{
	ulong         xSeq;  /* 정보분배 일련번호 */ 
	HMS_T         time;  /* 호가시간          */

	ulong        stVol;  /* 매도호가 총잔량   */
	ulong        btVol;  /* 매수호가 총잔량   */

	double     sMinPrc;  /* 매도우선호가최소  */
	double     sMaxPrc;  /* 매도우선호가최대  */
	double     bMinPrc;  /* 매수우선호가최소  */
	double     bMaxPrc;  /* 매수우선호가최대  */

	BHOGA_U    shoga[N_BHOGA]; /* 매도호가 DESC  */
	BHOGA_U    bhoga[N_BHOGA]; /* 매수호가 DESC  */
} BHOGA_T;


#endif
