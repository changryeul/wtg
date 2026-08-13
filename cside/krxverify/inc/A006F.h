
#ifndef __A006F_h__
#define __A006F_h__


/* IFMSBTD0022 A006F	파생 종목정보 */

typedef struct
{
	char datc[ 2];  /* 데이터구분값		           String	 2	*/
	char infc[ 3];  /* 정보구분값			       String	 3	*/
	char seqn[ 8];  /* 정보분배일련번호	           Int	     8	
					   시세 : 종목별 보드별 부여 (※ 대용량 서비스에서 제공)
					   종목정보 :  정보구분값별 부여 기타 : 데이터구분값별 부여 */ 
	char nofi[ 6];  /* 정보구분총종목수	           Int	     6	                          */
	char bday[ 8];  /* 영업일자			           String	 8                            */
	char code[12];  /* 종목코드			           String	 12	                          */
	char infn[ 6];  /* 정보분배종목인덱스	       Int	     6	당일 시장별 종목 식별용으로 부여되는 일련번호 
					   - 시장(정보상품) : 유가시장(증권A,증권C), 코스닥시장(증권B), 코넥스시장(증권B), 
					     파생시장(파생A,파생B), 금현물시장(일반A), 배출권시장(일반A)                      */

	char focd[ 1]; /* 선물옵션구분코드	        String	1	46	0:해당없음 C:콜옵션 F:선물 P:풋옵션 */
	char prid[11]; /* 상품ID	                String	11	57	                                      */
	char iscd[ 9]; /* 종목단축코드	            String	9	66	                                      */
	char klnm[80]; /* 종목명	                String	80	146	                                      */
	char ksnm[40]; /* 종목약명	                String	40	186	                                      */
	char elnm[80]; /* 종목영문명	            String	80	266	                                      */
	char esnm[40]; /* 종목영문약명	            String	40	306	                                      */
	char grid[ 3]; /* 장운영상품그룹ID	        String	3	309	                                      */
	char ltdt[ 8]; /* 상장일자	                String	8	317	                                      */
	char dldt[ 8]; /* 상장폐지일자	            String	8	325	                                      */
	char stcd[ 1]; /* 스프레드기준종목구분코드	String	1	326	F: 원월물(시간스프레드) N: 근월물(시간스프레드) 
																H: 고가물(가격스프레드) L: 저가물(가격스프레드) 
																C: 단기물(상품간스프레드) */
	char paym[ 1]; /* 최종결제방법코드	        String	1	327	C: 현금결제 D: 실물인수도결제 
																A: 현금+실물인수도결제 O: 해당없음 */
	char plec[ 1]; /* 가격제한확대적용방향코드	String	1	328	X: 미적용 F: 정방향 B: 역방향 T: 양방향 */
	char fple[ 3]; /* 가격제한최종단계		    Int	    3	331	 */
	char upl1[11]; /* 가격제한1단계상한가       Double	11	342	 */
	char upl2[11]; /* 가격제한2단계상한가       Double	11	353	 */
	char upl3[11]; /* 가격제한3단계상한가       Double	11	364	 */
	char lpl1[11]; /* 가격제한1단계하한가       Double	11	375	 */
	char lpl2[11]; /* 가격제한2단계하한가       Double	11	386	 */
	char lpl3[11]; /* 가격제한3단계하한가       Double	11	397	 */
	char bprc[11]; /* 기준가격	                Double	11	408	 */
	char uaid[ 3]; /* 기초자산ID	            String	3	411	 */
	char recd[ 1]; /* 권리행사유형코드	        String	1	412	 A: 미국형 E: 유럽형 Z: 기타 */
	char sccd[ 2]; /* 스프레드구성코드	        String	2	414	
					  Combination 호가를 대체할 스프레드물의 종목구성. 시간스프레드와 가격스프레드별로 다르게 정의
						** 코드값 **
						1. 시간스프레드
						   - T1: 최근월물+2째월물
							  > T2, T3, T4, …
						2. 가격스프레드
						   2.1 D1: ATM+아래 첫 가격물
								> D2, D3, D4
						   2.2 U01: ATM+위 첫 가격물
								> U2, U3, U4
						3. 상품간스프레드
						   - C1: 상품간 최근월물
							 C2: 상품간 2째월물 , C3, C4, …     */
	char spr1[12]; /* 스프레드구성종목코드1	    String	    12	426	 */
	char spr2[12]; /* 스프레드구성종목코드2	    String	    12	438	 */
	char tddt[ 8]; /* 최종거래일자	            String	    8	446	 */
	char lpdt[ 8]; /* 최종결제일자	            String	    8	454	 */
	char sndm[ 3]; /* 결제월일련번호	        Int	        3	457	 */
	char exdt[ 8]; /* 만기일자	                String	    8	465	*/
	char eprc[18]; /* 행사가격	                FLOAT128	18	483	 */
	char bpac[ 1]; /* 조정구분코드	            String	    1	484	
					  기초자산의 기준가격 조정이 정배수 조정(미결제조정)인지 비정배수 조정(거래승수조정)인지의 구분 
					  N: 정상 O: 미결제조정 C: 거래승수조정 */
	char unit[22]; /* 거래단위	                FLOAT128	22	506	1 
					  호가수량에 해당하는 거래대상(파생상품의 경우 기초자산) 자산의 수량. 지수의 Point, 주식의 주수,
					  채권 및 외화의 금액, 실물자산의 무게 등이 해당함. 파생상품의 경우 실물인수도시 그 기준이 됨 */
	char mult[22]; /* 거래승수	                FLOAT128	22	528	
					  약정대금 및 결제시 사용하는 계산승수. 호가제출시 표시하는 가격에 거래승수를 곱한 값이 1 
					  '거래단위'를 거래하기 위한 실제 가격이 됨. 거래단위 = 거래승수 × 가격표시단위 */
	char tplp[ 1]; /* 시장조성구분코드	        String	1	529	
                       0:미시장조성종목 1:당일시장조성종목 2:과거시장조성종목 */
	char ltcd[ 1]; /* 상장유형코드	            String	1	530	
					  기상장 종목 또는 신규상장 종목의 유형 
                      1: 신규상장 2: 추가설정 3: 기존종목 4: 최초상장 5: 종목조정 6: 특별설정 */
	char aprc[11]; /* 등가격	                Double	   11	541	기초자산기준가격에 가장 가까운 행사가격 */
	char arcd[ 2]; /* 조정사유코드	            String	    2	543	
					  10:유상증자(구주주 배정) 11:유상증자(제3자 배정) 12:유상증자(공모 증자)   
					  13:유상증자(DR)          14:기타 유상증자        15:상장폐지(종목별) 
					  16:주식교환              17:스톡옵션 행사        18:이익소각 
					  20:무상증자              21:주식배당             31:국내 CB 전환 
					  32:해외 CB 전환          34:신주인수권 행사      35:국내 BW 행사 
					  36:해외 BW 행사          41:DR 분할              42:DR 병합 
					  43:원주의 DR 전환        44:DR의 원주 전환       50:합병 
					  60:상호변경              61:주식의 종류 변경     70:액면 병합 
					  71:액면 분할             72:감자(주식병합)       73:회사분할(종속) 
					  80:회사분할로 인한 재상장 81:상장폐지후 신청에 의한 재상장 98:설립 99:기타 */
	char uacd[12]; /* 기초자산종목코드	        String	    12	555	 */
	char uapr[11]; /* 기초자산종가	            Double	    11	566	 */
	char rday[ 8]; /* 잔존일수	                Int	        8	574	 */
	char abpr[18]; /* 조정기준가격	            FLOAT128	18	592	*/
	char ppcd[ 2]; /* 기준가격구분코드	        String	    2	594	
					  파생상품 종목의 익일 매매체결 기준가격으로 인정하는 가격으로서 조정이나 거래성립, 이론가 
					   존재여부 등을 고려해서 결정함. 코드값 10번대는 선묾상품, 20번대는 옵션상품에 해당함 
						11:전일정산가
						12:전일기준가(거래성립전 종가미형성)
						13:당일이론가(거래성립전 종가미형성)
						14:전일기세(거래성립전 기세형성)
						15:당일이론가(거래성립전 기세형성)
						16:조정된 전일정산가
						17:조정된 전일기준가(거래성립전 종가미형성)
						18:조정된 전일기세(거래성립전 기세형성)
						19:전일 대상자산 종가(이론가없는 상품)
						21:전일증거금기준가
						22:전일기준가(거래성립전 종가미형성)
						23:당일이론가(거래성립전 종가미형성)
						24:전일기세(거래성립전 기세형성)
						25:조정된 전일증거금기준가
						26:조정된 전일기준가(거래성립전 종가미형성)
						27:조정된 전일기세(거래성립전 기세형성) */
	char bptc[ 1]; /* 매매용기준가격구분코드	String	       1	595	
						0:해당없음
						1:실세
						2:기세
						3:이론가
						4:대상자산종가 */
	char pcpr[18]; /* 전일조정종가	            FLOAT128	  18	613	 */
	char btcd[ 1]; /* 협의대량매매대상여부	    String	       1	614	 
					  여부(Y:협의매매상품 N:대상아님) 
					  협의대량가능상품(CLASS) : 3년국채선물, 미국달러선물, 유로선물, 엔선물 */
	char pdpc[23]; /* 전일증거금기준가격	    FLOAT128	  23	637	 */
	char bpcd[ 2]; /* 증거금기준가격구분코드	String	       2	639	
					   파생상품 종목의 증거금산출을 위한 기준가격으로 인정하는 가격으로서 조정이나 거래성립, 
						이론가 존재여부 등을 고려해서 결정함. '정산가격구분코드'와 동일한 코드 도메인 사용
						10:정산가없음
						11:당일종가 (실세)
						12:당일기세 (거래성립후기세)
						13:전일정산가(거래성립후 종가미형성)
						14:당일이론가(거래성립후 종가미형성)     */
	char setp[16]; /* 정산이론가격	            FLOAT128	  16	655	*/
	char btpr[16]; /* 기준이론가격	            FLOAT128	  16	671	*/
	char pspr[18]; /* 전일정산가격	            FLOAT128	  18	689	*/
	char halt[ 1]; /* 거래정지여부	            String	       1	690	여부(정상, 거래정지) */
	char fulp[11]; /* 선물CIRCUIT_BREAKERS상한가	Double	  11	701	
					  CB 발동조건을 판단하기 위한 상한가격 (기준가대비 +/- 5 %가 CB 발동 조건인경우 
					  +5%를 계산한 가격) */
	char fllp[11]; /* 선물CIRCUIT_BREAKERS하한가	Double	  11	712	
					  CB 발동조건을 판단하기 위한 하한가격 (기준가대비 +/- 5 %가 CB 발동 조건인경우 
					  -5%를 계산한 가격) */
	char dppr[18]; /* 조회용행사가격	         FLOAT128	  18	730	 */
	char atmc[ 1]; /* ATM구분코드	             String	       1	731	0:선물 1:ATM 2:ITM 3:OTM */
	char ftdt[ 1]; /* 최종거래일여부	         String	       1	732	                         */
	char dvsp[16]; /* 배당락후배당가치	         FLOAT128	16	748	
						배당락후배당가치(익일) :
						- 배당락후미래가치(선물)
						- 배당락후현재가치(옵션) */
	char yprc[11]; /* 전일종가	                 Double	11	759	      */
	char yprt[ 1]; /* 전일종가구분코드	         String	1	760	
						1:실세
						2:기세
						3:거래무
						4:시가기준가종목의 기세 */
	char pdop[11]; /* 이전일자시가	             Double	11	771	      */
	char pdhp[11]; /* 이전일자고가	             Double	11	782	      */
	char pdlp[11]; /* 이전일자저가	             Double	11	793	      */
	char ftdp[ 8]; /* 최초거래일자	             String	8	801	      */
	char lttm[ 9]; /* 최종체결시각	             String	9	810	      */
	char sptc[ 2]; /* 정산가격구분코드	         String	2	812	
						파생상품 종목의 당일 정산가격으로 인정하는 가격으로서 조정이나 거래성립, 
						 이론가 존재여부 등을 고려해서 결정함
						10:정산가없음
						11:당일종가 (실세)
						12:당일기세 (거래성립후기세)
						13:전일정산가(거래성립후 종가미형성)
						14:당일이론가(거래성립후 종가미형성)
						15:스프레드분 종가
						16:조정된 전일 정산가
						17:대상자산 종가
						18:정산기준가격
						40: 최우선매수호가(장종료시 양방호가가 있고 정산기준가격이 최우선매수호가와 같거나 
							최우선매수호가를 하방으로 초과하는 경우)
						41: 최우선매도호가(장종료시 양방호가가 있고 정산기준가격이 최우선매도호가와 같거나 
							최우선매도호가를 상방으로 초과하는 경우) */
	char dprt[13]; /* 정산가격이론가격괴리율	 Double	  13	825	     */
	char pdoi[12]; /* 전일미결제약정수량	     Long	  12	837	     */
	char pdba[11]; /* 전일매도우선호가가격	     Double	  11	848	     */
	char pdbb[11]; /* 전일매수우선호가가격	     Double	  11	859	     */
	char ipvl[11]; /* 내재변동성	             Double	  11	870	     */
	char pkpr[11]; /* 상장중최고가	             Double	  11	881	     */
	char lppr[11]; /* 상장중최저가	             Double	  11	892	     */
	char pypr[11]; /* 연중최고가	             Double	  11	903	     */
	char lypr[11]; /* 연중최저가	             Double	  11	914	     */
	char pkdt[ 8]; /* 상장중최고가일자	         String	   8	922	     */
	char lodt[ 8]; /* 상장중최저가일자	         String	   8	930	     */
	char hydt[ 8]; /* 연중최고가일자	         String	   8	938	     */
	char lydt[ 8]; /* 연중최저가일자	         String	   8	946	     */
	char nofy[ 8]; /* 연간기준일수	             Int	   8	954	     */
	char ntdm[ 8]; /* 월간거래일수	             Int	   8	962	     */
	char ntdy[ 8]; /* 연간거래일수	             Int	   8	970	     */
	char ycnt[15]; /* 전일체결건수	             Long	  15	985	     */
	char yvol[12]; /* 전일누적거래량	         Long	  12	997	     */
	char yamt[22]; /* 전일누적거래대금	         FLOAT128 22	1019     */
	char ytvl[15]; /* 전일총누적거래량	         Long	  15	1034	 */
	char ytam[22]; /* 전일총누적거래대금	     FLOAT128 22	1056     */
	char itrt[11]; /* 금리	                     Double	  11	1067	
					  주식파생상품 : 선형보간금리 FICC, 
					  이론가 미산출 상품(변동성지수선물, 코스피고배당50선물, 코스피배당성장50선물, 
					  유로스톡스50선물, 스프레드 종목) : CD금리(이론가에 사용되는 금리가 아님) */
	char oivl[15]; /* 주식선물미결제한도수량	 Long	  15	1082	*/ 
	char ulid[ 4]; /* 기초자산상품군ID	         String	   4	1086	
					  증거금 산출시 오프셋 비율을 적용하는 기초자산상품군ID */
	char orag[11]; /* 증거금OFFSET비율	         Double	  11	1097	증거금 OFFSET 비율 (%) */
	char lotc[ 5]; /* 지정가호가취소조건코드	 Int	   5	1102	
						지정가호가취소조건코드. Bitwise 정의
						1: FAS (Fill And Stay)
						2: FOK (Fill Or Kill)
						4: FAK (Fill And Kill)
						8: GTS (Good for the Session)
						16: GTC (Good Till Cancel)
						32: GTD (Good Till Date) */
	char mpoc[ 5]; /* 시장가호가취소조건코드	      Int	   5	1107	
						시장가호가취소조건코드. Bitwise 정의
						1: FAS (Fill And Stay)
						2: FOK (Fill Or Kill)
						4: FAK (Fill And Kill)
						8: GTS (Good for the Session)
						16: GTC (Good Till Cancel)
						32: GTD (Good Till Date) */
	char copc[ 5]; /* 조건부지정가호가취소조건코드	  Int	5	1112	
						조건부지정가호가취소조건코드. Bitwise 정의
						1: FAS (Fill And Stay)
						2: FOK (Fill Or Kill)
						4: FAK (Fill And Kill)
						8: GTS (Good for the Session)
						16: GTC (Good Till Cancel)
						32: GTD (Good Till Date)  */
	char bfoc[ 5]; /* 최유리지정가호가취소조건코드	  Int	5	1117	
						최유리지정가호가취소조건코드. Bitwise 정의
						1: FAS (Fill And Stay)
						2: FOK (Fill Or Kill)
						4: FAK (Fill And Kill)
						8: GTS (Good for the Session)
						16: GTC (Good Till Cancel)
						32: GTD (Good Till Date)   */
	char efpi[ 1]; /* EFP거래대상여부	    String	 1	1118	EFP:기초자산조기인수도부거래 */
	char flxi[ 1]; /* FLEX거래대상여부	    String	 1	1119	     */
	char efvo[12]; /* EFP체결수량	        Long	 12	1131	EFP:기초자산조기인수도부거래 */
	char efva[22]; /* EFP거래대금	        FLOAT128 22	1153	EFP:기초자산조기인수도부거래 */
	char holy[ 1]; /* 휴장여부	            String	  1	1154	     */
	char ldpr[ 1]; /* 동적가격제한여부	    String	  1	1155	     */
	char uldp[11]; /* 동적상한가간격	    Double	 11	1166	동적상한가=직전 체결가격+동적상한가간격 */
	char lldp[11]; /* 동적하한가간격	    Double	 11	1177	동적하한가=직전 체결가격+동적하한가간격(음수 가능) */
	char umid[ 3]; /* 기초자산시장ID	    String	  3	1180	기초자산이 거래되는 시장의 시장ID */
	char ulvo[23]; /* 상한수량	            FLOAT128	23	1203	*/
	char llvo[23]; /* 하한수량	            FLOAT128	23	1226	*/
	char ulbt[23]; /* 협의대량매매상한수량	FLOAT128	23	1249	*/
	char llbt[23]; /* 협의대량매매하한수량	FLOAT128	23	1272	*/
	char bpid[11]; /* 기준상품ID	        String	    11	1283	 */
	char spid[11]; /* 부상품ID	            String	    11	1294	 */
	char nibp[ 6]; /* 기준상품종목수	    Int	        6	1300	 */
	char nisp[ 6]; /* 부상품종목수	        Int	        6	1306	 */
	char stwk[ 2]; /* 결제주	            String	    2	1308	결제주(W1,W2,W3,W4,W5) */
	char supn[ 1]; /* 휴면여부	            String	    1	1309	*/
	char ssdt[ 8]; /* 휴면지정일자	        String	    8	1317	*/
	char endc[ 1]; /* 정보분배메세지종료키워드	String	1	1318	메세지의 마지막을 식별하는 문자 (%HFF)  */
} A006F_T;


#endif






