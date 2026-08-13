
#ifndef __A301K_h__
#define __A301K_h__

/* IFMSRPD0027 A301K	채권 체결 */
typedef struct
{ 
	//XHDR  hdr;
	char datc[ 2];  /* 데이터구분값		           String	 2	정보분배 데이터를 식별하는 구분 코드값    */
	char infc[ 3];  /* 정보구분값			       String	 3	정보분배에서 분배하는 정보의 구분 코드값  */
	char seqn[ 8];  /* 정보분배일련번호	           Int	     8	정보분배에서 부여하는 일련번호 
					   시세 : 종목별 보드별 부여 (※ 대용량 서비스에서 제공)
					   종목정보 :  정보구분값별 부여 기타 : 데이터구분값별 부여 */ 
	//XBTRI tri; 
	char bdid[ 2]; 	/* 보드ID	             String	2	*/
	char ssid[ 2]; 	/* 세션ID	             String	2	*/
	char code[12]; 	/* 종목코드	             String	12  */	
	char time[12]; 	/* 매매처리시각	         String	12	*/ 

	char cprc[11];  /* 체결가격	             Double	11	*/
	char cvol[10];  /* 거래량	             Long	10	*/
	char cday[ 8];  /* 거래일자	             String	8	*/
	char camt[22];  /* 거래대금	             FLOAT128	22 */	
	char tyld[13];  /* 체결수익률	         Double	13	*/
	char oprc[11];  /* 시가	                 Double	11	*/
	char hprc[11];  /* 고가	                 Double	11	*/
	char lprc[11];  /* 저가	                 Double	11	*/
	char oyld[13];  /* 시가수익률            Double	13	*/
	char hyld[13];  /* 고가수익률            Double	13	*/
	char lyld[13];  /* 저가수익률	         Double	13	*/
	char tvol[15];  /* 채권누적체결수량	     Long	15	*/
	char tamt[22];  /* 누적거래대금	         FLOAT128	22 */
	char sday[ 8];  /* 결제일자	             String	8	*/
	char endc[ 1];  /* 정보분배메세지종료키워드	String	1*/

} A301K_T;


#endif



