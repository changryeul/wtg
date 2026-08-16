
#ifndef __fhoga_h__
#define __fhoga_h__

#define N_FHOGA 5

/* B606F */
typedef struct 
{
	double   prc ;   /* 호가        */
	ulong    vol ;   /* 호가잔량    */
	ulong    cnt ;   /* 주문건수    */
} FHOGA_U;

typedef struct 
{
	ulong         xSeq;  /* 정보분배 일련번호 */ 
	HMS_T         time;  /* 호가시간          */

	ulong        stVol;  /* 매도호가 총잔량   */
	ulong        btVol;  /* 매수호가 총잔량   */

	ulong        saCnt;  /* 매도호가 유효건수 */
	ulong        baCnt;  /* 매수호가 유효건수 */

	double       exPrc;  /* 예상체결가        */
	ulong        exVol;  /* 예상체결수량      */

	FHOGA_U     shoga[N_FHOGA]; /* 매도호가 DESC  */
	FHOGA_U     bhoga[N_FHOGA]; /* 매수호가 DESC  */
} FHOGA_T;

#endif
