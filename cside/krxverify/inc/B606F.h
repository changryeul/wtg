
#ifndef __B606F_h__
#define __B606F_h__

/* IFMSRPD0034 B606F	파생 우선호가 (우선호가 5단계) */
typedef struct
{
	//XHDR  hdr;
	char datc[ 2];  /* 데이터구분값		     String	     2	  */
	char infc[ 3];  /* 정보구분값			 String	     3	  */
	char seqn[ 8];  /* 정보분배일련번호	     Int	     8	 
					   시세 : 종목별 보드별 부여 (※ 대용량 서비스에서 제공)
					   종목정보 :  정보구분값별 부여 기타 : 데이터구분값별 부여 */ 
	// XTRF trf;
	char bdid[ 2]; /* 보드ID	             String	   2	*/
	char ssid[ 2]; /* 세션ID	             String	   2	*/
	char code[12]; /* 종목코드	             String	   12  */	
	char infn[ 6]; /* 정보분배종목인덱스	 Int	   6	당일 시장별 종목 식별용으로 부여되는 일련번호 
					   - 시장(정보상품) : 유가시장(증권A,증권C), 코스닥시장(증권B), 코넥스시장(증권B), 
					     파생시장(파생A,파생B), 금현물시장(일반A), 배출권시장(일반A)                      */
	char time[12]; /* 매매처리시각	         String	    12	*/ 

	struct 
	{ 
		char sprc[ 9]; /* 매도우선호가		Double	9		*/
		char bprc[ 9]; /* 매수우선호가		Double	9		*/
		char svol[ 9]; /* 매도우선호가잔량		Int	9		*/
		char bvol[ 9]; /* 매수우선호가잔량		Int	9		*/
		char scnt[ 5]; /* 매도우선호가주문건수	Int	5		*/
		char bcnt[ 5]; /* 매수우선호가주문건수	Int	5		*/
	} hoga[5]; 

	char stvl[ 9]; /* 매도호가총잔량		Int	9		*/
	char btvl[ 9]; /* 매수호가총잔량		Int	9		*/
	char apvc[ 5]; /* 매도호가유효건수		Int	5		*/
	char bpvc[ 5]; /* 매수호가유효건수		Int	5		*/
	char etpr[ 9]; /* 예상체결가	        Double	9	*/
	char etvl[ 9]; /* 예상체결수량		    Int	    9	*/
	char endc[ 1];  /* 	정보분배메세지종료키워드(%HFF)		String	1   */

} B606F_T;



#endif




