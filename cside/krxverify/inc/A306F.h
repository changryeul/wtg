
#ifndef __A306F_h__
#define __A306F_h__


/* IFMSRPD0036 A306F	파생 체결 */
typedef struct
{
	char datc[ 2];  /* 데이터구분값		     String	   2	 */
	char infc[ 3];  /* 정보구분값			 String	   3	 */
	char seqn[ 8];  /* 정보분배일련번호	     Int	   8	 
					   시세 : 종목별 보드별 부여 (※ 대용량 서비스에서 제공)
					   종목정보 :  정보구분값별 부여 기타 : 데이터구분값별 부여 */ 
	char bdid[ 2]; /* 보드ID	             String	   2	*/
	char ssid[ 2]; /* 세션ID	             String	   2	*/
	char code[12]; /* 종목코드	             String	   12  */	
	char infn[ 6]; /* 정보분배종목인덱스	 Int	   6	당일 시장별 종목 식별용으로 부여되는 일련번호 
					   - 시장(정보상품) : 유가시장(증권A,증권C), 코스닥시장(증권B), 코넥스시장(증권B), 
					     파생시장(파생A,파생B), 금현물시장(일반A), 배출권시장(일반A)                      */
	char time[12]; /* 매매처리시각	         String	    12	*/ 

	char cprc[ 9]; /* 체결가격		         Double	    9	56	*/
	char cvol[ 9]; /* 거래량		         Int	    9	65	*/
	char nprc[ 9]; /* 근월물체결가격	     Double	    9	74 선물스프레드 구성종목 중 근월물의 체결가격 */ 
	char fprc[ 9]; /* 원월물체결가격	     Double	    9	83 선물스프레드 구성종목 중 원월물의 체결가격 */	
	char oprc[ 9]; /* 시가                   Double	    9	92	*/
	char hprc[ 9]; /* 고가                   Double	    9	101 */	
	char lprc[ 9]; /* 저가                   Double	    9	110	*/
	char pprc[ 9]; /* 직전가격		         Double	    9	119	*/
	char tvol[12]; /* 누적거래량		     Long	    12	131	*/
	char tamt[22]; /* 누적거래대금		     FLOAT128	22	153	*/
	char ftcd[ 1]; /* 최종매도매수구분코드	 String	    1	154	space : 단일가체결 0 해당없음 1 매도 2 매수" */
	char uldp[ 9]; /* 동적상한가	         Double	    9	163	*/
	char lldp[ 9]; /* 동적하한가	         Double	    9	172	*/
	char endc[ 1]; /* 정보분배메세지종료키워드(%HFF)	String	1   */
} A306F_T;

#endif




