
#ifndef __A001B_h__
#define __A001B_h__


/* IFMSBTD0016 A001B 채권 종목정보 */
typedef struct {
	char datc[ 2];  /* 데이터구분값		           String	 2	정보분배 데이터를 식별하는 구분 코드값    */
	char infc[ 3];  /* 정보구분값			       String	 3	정보분배에서 분배하는 정보의 구분 코드값  */
	char seqn[ 8];  /* 정보분배일련번호	           Int	     8	정보분배에서 부여하는 일련번호 
					   시세 : 종목별 보드별 부여 (※ 대용량 서비스에서 제공)
					   종목정보 :  정보구분값별 부여 기타 : 데이터구분값별 부여 */ 
	char nofi[ 6];  /* 정보구분총종목수	           Int	     6	정보구분값별 총 종목수                    */
	char bday[ 8];  /* 영업일자			           String	 8                                      	  */
	char code[12];  /* 종목코드			           String	 12	현선물 통합상품의 종목 코드(ISIN종목코드) */
	char infn[ 6];  /* 정보분배종목인덱스	       Int	     6	당일 시장별 종목 식별용으로 부여되는 일련번호 
					   - 시장(정보상품) : 유가시장(증권A,증권C), 코스닥시장(증권B), 코넥스시장(증권B), 
					     파생시장(파생A,파생B), 금현물시장(일반A), 배출권시장(일반A)                      */

	char typc[ 2];  /* 소매채권분류코드	           String	 2	
                         BA:금융채 CA:회사채 ET:지방채 EC:기타 GA:국채 MA:통안채                          */
	char ksnm[40];  /* 종목약명			           String	 40	                                    	  */
	char esnm[40];  /* 종목영문약명		           String	 40	                                    	  */
	char opid[ 3];  /* 장운영상품그룹ID	           String	 3	동일한 장운영(TSC, Trading Schedule Control) 
					    대상이 되는 상품들의 집합을 식별하기 위한 ID. 동일한 스케쥴로 제어되는 논리적인 단위 */
	char bltc[ 1];  /* 채권상장구분코드	           String	 1	
                        D:상장폐지 E:기타 I:미발행 N:비상장 Y:상장                                        */
	char ctcd[ 6];  /* 채권분류코드		           String	 6	                                          */
	char gtcd[ 1];  /* 채권보증구분코드	           String	 1	
                       1:보증 2:부분보증 3:담보부 4:무보증 5:정부보증 6:커버드본드                        */
	char cpcd[ 2];  /* 이자지급방법코드	           String	 2	
                       11:고정금리형-할인채 12:고정금리형-복리채 13:고정금리형-이표채 14:고정금리형-단리채 
                       15:고정금리형-복5단2 19:고정금리형-기타 21:변동금리형-이표채 22:변동금리형-복리채 
                       23:변동금리형-단리채 29:변동금리형-기타 */
	char ltdt[ 8];  /* 상장일자			           String	 8	파생상품(CLASS) 이나 종목의 상장일자      */
	char isdt[ 8];  /* 발행일자			           String	 8		                                      */
	char rddt[ 8];  /* 상환일자			           String	 8		                                      */
	char sldt[ 8];  /* 매출일				       String	 8		                                      */
	char isrt[13];  /* 채권발행율			       Double	 13	양수:할인 음수:할증                       */
	char cprt[14];  /* 표면이자율			       Double	 14	연간이자지급액/채권액면가액               */
	char mccp[ 4];  /* 이자지급계산월수	           Int	     4	0:불규칙, 0(이표채,복리채,그외 비표기)    */
	char cptc[ 1];  /* 이표지급방법코드	           String	 1	1:선급 2:후급                             */
	char intp[ 1];  /* 채권이자지급일기준구분코드  String	 1	1:발행일 2:상환일                         */
	char cpdt[ 1];  /* 이자월말구분코드	           String	 1	1:일자기준 2:말일기준                     */
	char dpcc[ 1];  /* 이자원단위미만처리코드	   String	 1	0:NO 1:절사 2:절상 3:반올림               */
	char pisc[ 1];  /* 채권선매출이자지급방법코드  String	 1	1:매출시 2:최초이자지급시 3:상환시 4:매출시(국고채 유형) */
	char isam[22];  /* 발행금액					   FLOAT128	 22			                                      */
	char ltam[22];  /* 상장금액					   FLOAT128	 22			                                      */
	char rram[13];  /* 만기상환비율	               Double	 13			                                      */
	char atcd[ 1];  /* 분할상환유형구분코드	       String	 1	1:원금균등(default) 2:원리금균등 3:불균등 */
	char nomg[ 4];  /* 거치개월수	               Int	     4			                                      */
	char noam[ 5];  /* 분할상환횟수	               Int	     5			                                      */			
	char halt[ 1];  /* 거래정지여부	               String	 1	거래정지 여부(정상, 거래정지)                 */
	char pcpd[ 8];  /* 전기이자지급일자	           String	 8			                                      */	
	char ncpd[ 8];  /* 차기이자지급일자	           String	 8			                                      */
	char pbss[ 1];  /* 영구채권만기구조여부	       String	 1	Y:영구채권만기구조 임 N:영구채권만기구조가 아님 */
	char sbtc[ 1];  /* 채권스트립구분코드	       String	 1	1:일반채권 2:원금분리 3:이자분리              */
	char bprc[11];  /* 기준가격	                   Double	 11			                                      */
	char ltrd[ 1];  /* 정리매매여부	               String	 1			                                      */	
	char ictc[ 1];  /* 투자유의채권구분코드	       String	 1	0: 해당없음 1: 지정예고 2: 지정               */
	char endc[ 1];  /* 정보분배메세지종료키워드	   String	 1	메세지의 마지막을 식별하는 문자 (%HFF)        */
} A001B_T;                                                     
                                                             
                                                             
#endif


