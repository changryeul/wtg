
#ifndef __B601K_h__
#define __B601K_h__


/* IFMSRPD0023 B601K	일반채권, 국고채권 우선호가  */
typedef struct 
{
	char datc[ 2];  /* 데이터구분값		     String	 2	정보분배 데이터를 식별하는 구분 코드값    */
	char infc[ 3];  /* 정보구분값			 String	 3	정보분배에서 분배하는 정보의 구분 코드값  */
	char seqn[ 8];  /* 정보분배일련번호	     Int	     8	정보분배에서 부여하는 일련번호 
					   시세 : 종목별 보드별 부여 (※ 대용량 서비스에서 제공)
					   종목정보 :  정보구분값별 부여 기타 : 데이터구분값별 부여 */ 
	char bdid[ 2]; 	/* 보드ID	             String	2	*/
	char ssid[ 2]; 	/* 세션ID	             String	2	*/
	char code[12]; 	/* 종목코드	             String	12  */	
	char time[12]; 	/* 매매처리시각	         String	12	*/ 

	struct
	{
		char sprc[11]; 	/* 매도우선호가가격	Double	11	*/
		char bprc[11]; 	/* 매수우선호가가격	Double	11	*/
		char svol[15];  /* 매도우선호가잔량	Long	15	*/
		char bvol[15];  /* 매수우선호가잔량	Long	15	*/
		char syld[13];  /* 매도우선호가수익률	Double	13	*/
		char byld[13];  /* 매수우선호가수익률	Double	13	*/
	} hoga[5];

	char stvl[15];  /* 	채권매도호가총잔량					Long	15	*/
	char btvl[15];  /* 	채권매수호가총잔량					Long	15	*/	
	char endc[ 1];  /* 	정보분배메세지종료키워드(%HFF)		String	1   */

} B601K_T;



#endif





