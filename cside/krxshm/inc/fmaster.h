
#ifndef __fmaster_h__
#define __fmaster_h__

typedef struct 
{
	char    code[12];  /* 종목코드			       String	 12	                          */
	uint    infn    ;  /* 정보분배종목인덱스	       Int	     6	당일 시장별 종목 식별용으로 부여되는 일련번호 
		       		   - 시장(정보상품) : 유가시장(증권A,증권C), 코스닥시장(증권B), 코넥스시장(증권B), 
		       		     파생시장(파생A,파생B), 금현물시장(일반A), 배출권시장(일반A)                      */
	char    ksnm[40]; /* 종목약명	                String	40		                                      */
	char    esnm[40]; /* 종목영문약명	            String	40		                                      */
            
	char    focd    ; /* 선물옵션구분코드	        String	1	 0:해당없음 C:콜옵션 F:선물 P:풋옵션 */
	char    prid[11]; /* 상품ID	                    String	11	                                       */
	char    iscd[ 9]; /* 종목단축코드	            String	9	                                       */
	char    grid[ 3]; /* 장운영상품그룹ID	        String	3		                                      */
	YMD_T   ltdt    ; /* 상장일자	                String	8		                                      */
	YMD_T   dldt    ; /* 상장폐지일자	            String	8		                                      */
	char    stcd    ; /* 스프레드기준종목구분코드	String	1	F: 원월물(시간스프레드) N: 근월물(시간스프레드) 
		       													H: 고가물(가격스프레드) L: 저가물(가격스프레드) 
		       													C: 단기물(상품간스프레드) */
	char    paym    ; /* 최종결제방법코드	        String	1	C: 현금결제 D: 실물인수도결제 
		       													A: 현금+실물인수도결제 O: 해당없음 */
	char    plec    ; /* 가격제한확대적용방향코드	String	1	X: 미적용 F: 정방향 B: 역방향 T: 양방향 */
	int     fple    ; /* 가격제한최종단계		    Int	    3		 */
	char    fil1[ 1];
	double  upl1    ; /* 가격제한1단계상한가       Double	11		 */
	double  upl2    ; /* 가격제한2단계상한가       Double	11		 */
	double  upl3    ; /* 가격제한3단계상한가       Double	11		 */
	double  lpl1    ; /* 가격제한1단계하한가       Double	11		 */
	double  lpl2    ; /* 가격제한2단계하한가       Double	11		 */
	double  lpl3    ; /* 가격제한3단계하한가       Double	11		 */
	double  bprc    ; /* 기준가격	              Double	11		 */
	char    uaid[ 3]; /* 기초자산ID	              String	3	   	 */
	char    recd    ; /* 권리행사유형코드	      String	1	A: 미국형 E: 유럽형 Z: 기타 */
	char    sccd[ 2]; /* 스프레드구성코드	      String	2	
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
	char    fil2[ 2];
	char    spr1[12]; /* 스프레드구성종목코드1	    String	    12		 */
	char    spr2[12]; /* 스프레드구성종목코드2	    String	    12		 */
	YMD_T   tddt    ; /* 최종거래일자	            String	    8		 */
	YMD_T   lpdt    ; /* 최종결제일자	            String	    8		 */
	int     sndm    ; /* 결제월일련번호	            Int	        3		 */
	YMD_T   exdt    ; /* 만기일자	                String	    8		*/
	double  eprc    ; /* 행사가격	                FLOAT128	18		 */
	double  unit    ; /* 거래단위	                FLOAT128	22		1 
		     		  호가수량에 해당하는 거래대상(파생상품의 경우 기초자산) 자산의 수량. 지수의 Point, 주식의 주수,
		     		  채권 및 외화의 금액, 실물자산의 무게 등이 해당함. 파생상품의 경우 실물인수도시 그 기준이 됨 */
	double  mult    ; /* 거래승수	                FLOAT128	22		
		       		  약정대금 및 결제시 사용하는 계산승수. 호가제출시 표시하는 가격에 거래승수를 곱한 값이 1 
		       		  '거래단위'를 거래하기 위한 실제 가격이 됨. 거래단위 = 거래승수 × 가격표시단위 */
	char    bpac    ; /* 조정구분코드	            String	    1		
		       		  기초자산의 기준가격 조정이 정배수 조정(미결제조정)인지 비정배수 조정(거래승수조정)인지의 구분 
		       		  N: 정상 O: 미결제조정 C: 거래승수조정 */
	char    tplp    ; /* 시장조성구분코드	        String	1	
                          0:미시장조성종목 1:당일시장조성종목 2:과거시장조성종목 */
	char    ltcd    ; /* 상장유형코드	            String	1	
		       		  기상장 종목 또는 신규상장 종목의 유형 
                         1: 신규상장 2: 추가설정 3: 기존종목 4: 최초상장 5: 종목조정 6: 특별설정 */
	char    arcd[ 2]; /* 조정사유코드	            String	    2	
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
	char    fil3[ 3];
            
	double  aprc    ; /* 등가격	                    Double	   11	기초자산기준가격에 가장 가까운 행사가격 */
	double  uapr    ; /* 기초자산종가	            Double	    11		 */
	char    uacd[12]; /* 기초자산종목코드	        String	    12		 */
	int     rday    ; /* 잔존일수	                Int	        8		 */
	double  abpr    ; /* 조정기준가격	            FLOAT128	18		*/
            
	char    ppcd[ 2]; /* 기준가격구분코드	        String	    2		
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
	char    bptc    ; /* 매매용기준가격구분코드	String	       1	
		       			0:해당없음
		       			1:실세
		       			2:기세
		       			3:이론가
		       			4:대상자산종가 */
	char    bpcd[ 2]; /* 증거금기준가격구분코드	String	       2		
		       		   파생상품 종목의 증거금산출을 위한 기준가격으로 인정하는 가격으로서 조정이나 거래성립, 
		       			이론가 존재여부 등을 고려해서 결정함. '정산가격구분코드'와 동일한 코드 도메인 사용
		       			10:정산가없음
		       			11:당일종가 (실세)
		       			12:당일기세 (거래성립후기세)
		       			13:전일정산가(거래성립후 종가미형성)
		       			14:당일이론가(거래성립후 종가미형성)     */
	char    btcd    ; /* 협의대량매매대상여부	    String	       1		 
		       		  여부(Y:협의매매상품 N:대상아님) 
		       		  협의대량가능상품(CLASS) : 3년국채선물, 미국달러선물, 유로선물, 엔선물 */
	char    halt    ; /* 거래정지여부	            String	       1		여부(정상, 거래정지) */
	char    fil4[ 9];
	double  pcpr    ; /* 전일조정종가	            FLOAT128	  18		 */
	double  pdpc    ; /* 전일증거금기준가격	        FLOAT128	  23		 */
	double  setp    ; /* 정산이론가격	            FLOAT128	  16		*/
	double  btpr    ; /* 기준이론가격	            FLOAT128	  16		*/
	double  pspr    ; /* 전일정산가격	            FLOAT128	  18		*/
	double  fulp    ; /* 선물CIRCUIT_BREAKERS상한가	Double	  11	
		       		  CB 발동조건을 판단하기 위한 상한가격 (기준가대비 +/- 5 %가 CB 발동 조건인경우 
		       		  +5%를 계산한 가격) */
	double  fllp    ; /* 선물CIRCUIT_BREAKERS하한가	Double	  11	
		       		  CB 발동조건을 판단하기 위한 하한가격 (기준가대비 +/- 5 %가 CB 발동 조건인경우 
		       		  -5%를 계산한 가격) */
	double  dppr    ; /* 조회용행사가격	             FLOAT128	  18		 */
	char    atmc    ; /* ATM구분코드	                 String	       1		0:선물 1:ATM 2:ITM 3:OTM */
	char    ftdt    ; /* 최종거래일여부	             String	       1		                         */
	char    yprt    ; /* 전일종가구분코드	         String	1		
		       			1:실세
		       			2:기세
		       			3:거래무
		       			4:시가기준가종목의 기세 */
	char    fil5[ 5];
            
	double  dvsp    ; /* 배당락후배당가치	         FLOAT128	16		
		     			배당락후배당가치(익일) :
		     			- 배당락후미래가치(선물)
		     			- 배당락후현재가치(옵션) */
	double  yprc    ; /* 전일종가	                 Double	11		      */
	double  pdop    ; /* 이전일자시가	             Double	11		      */
	double  pdhp    ; /* 이전일자고가	             Double	11		      */
	double  pdlp    ; /* 이전일자저가	             Double	11		      */
	HMS_T   lttm    ; /* 최종체결시각	             String	9		      */
	YMD_T   ftdp    ; /* 최초거래일자	             String	8		      */
	char    sptc[ 2]; /* 정산가격구분코드	         String	2		
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
	char    fil6[ 2]; 
	double  dprt    ; /* 정산가격이론가격괴리율	     Double	  13	     */
	ulong   pdoi    ; /* 전일미결제약정수량	         Long	  12	     */
	double  pdba    ; /* 전일매도우선호가가격	     Double	  11	     */
	double  pdbb    ; /* 전일매수우선호가가격	     Double	  11	     */
	double  ipvl    ; /* 내재변동성	                 Double	  11	     */
	double  pkpr    ; /* 상장중최고가	             Double	  11	     */
	double  lppr    ; /* 상장중최저가	             Double	  11	     */
	double  pypr    ; /* 연중최고가	                 Double	  11	     */
	double  lypr    ; /* 연중최저가	                 Double	  11	     */
	YMD_T   pkdt    ; /* 상장중최고가일자	         String	   8	     */
	YMD_T   lodt    ; /* 상장중최저가일자	         String	   8	     */
	YMD_T   hydt    ; /* 연중최고가일자	             String	   8	     */
	YMD_T   lydt    ; /* 연중최저가일자	             String	   8	     */
	uint    nofy    ; /* 연간기준일수	             Int	   8	     */
	uint    ntdm    ; /* 월간거래일수	             Int	   8	     */
	uint    ntdy    ; /* 연간거래일수	             Int	   8	     */
	char    ulid[ 4]; /* 기초자산상품군ID	         String	   4		
		       		  증거금 산출시 오프셋 비율을 적용하는 기초자산상품군ID */

	ulong   ycnt    ; /* 전일체결건수	             Long	  15	     */
	ulong   yvol    ; /* 전일누적거래량	             Long	  12	     */
	ldouble yamt    ; /* 전일누적거래대금	         FLOAT128 22	     */
	ulong   ytvl    ; /* 전일총누적거래량	         Long	  15		 */
	ldouble ytam    ; /* 전일총누적거래대금	         FLOAT128 22	     */
	double  itrt    ; /* 금리	                     Double	  11		
		     		  주식파생상품 : 선형보간금리 FICC, 
		     		  이론가 미산출 상품(변동성지수선물, 코스피고배당50선물, 코스피배당성장50선물, 
		     		  유로스톡스50선물, 스프레드 종목) : CD금리(이론가에 사용되는 금리가 아님) */
	ulong   oivl    ; /* 주식선물미결제한도수량	     Long	  15		*/ 
	double  orag    ; /* 증거금OFFSET비율	         Double	  11	증거금 OFFSET 비율 (%) */
	int     lotc    ; /* 지정가호가취소조건코드	     Int	   5	1102	
		       			지정가호가취소조건코드. Bitwise 정의
		       			1: FAS (Fill And Stay)
		       			2: FOK (Fill Or Kill)
		      			4: FAK (Fill And Kill)
		      			8: GTS (Good for the Session)
		      			16: GTC (Good Till Cancel)
						32: GTD (Good Till Date) */
	int     mpoc    ; /* 시장가호가취소조건코드	      Int	   5	
		       			시장가호가취소조건코드. Bitwise 정의
		       			1: FAS (Fill And Stay)
		       			2: FOK (Fill Or Kill)
		       			4: FAK (Fill And Kill)
		       			8: GTS (Good for the Session)
		       			16: GTC (Good Till Cancel)
		      			32: GTD (Good Till Date) */
	int     copc    ; /* 조건부지정가호가취소조건코드	  Int	5	
		       			조건부지정가호가취소조건코드. Bitwise 정의
		       			1: FAS (Fill And Stay)
		     			2: FOK (Fill Or Kill)
		     			4: FAK (Fill And Kill)
		     			8: GTS (Good for the Session)
		     			16: GTC (Good Till Cancel)
						32: GTD (Good Till Date)  */
	int     bfoc    ; /* 최유리지정가호가취소조건코드	  Int	5	
		       			최유리지정가호가취소조건코드. Bitwise 정의
		       			1: FAS (Fill And Stay)
		       			2: FOK (Fill Or Kill)
		       			4: FAK (Fill And Kill)
		       			8: GTS (Good for the Session)
		       			16: GTC (Good Till Cancel)
		      			32: GTD (Good Till Date)   */
	char    efpi    ; /* EFP거래대상여부	        String	 1		EFP:기초자산조기인수도부거래 */
	char    flxi    ; /* FLEX거래대상여부	    String	 1		     */
	char    holy    ; /* 휴장여부	            String	  1		     */
	char    ldpr    ; /* 동적가격제한여부	    String	  1		     */
	char    umid[ 3]; /* 기초자산시장ID	        String	  3		기초자산이 거래되는 시장의 시장ID */
	char    fil7[ 9];

	ulong   efvo    ; /* EFP체결수량	            Long	 12		EFP:기초자산조기인수도부거래 */
	ldouble efva    ; /* EFP거래대금	            FLOAT128 22		EFP:기초자산조기인수도부거래 */
	double  uldp    ; /* 동적상한가간격	        Double	 11		동적상한가=직전 체결가격+동적상한가간격 */
	double  lldp    ; /* 동적하한가간격	        Double	 11		동적하한가=직전 체결가격+동적하한가간격(음수 가능) */
	ldouble ulvo    ; /* 상한수량	            FLOAT128	23		*/
	ldouble llvo    ; /* 하한수량	            FLOAT128	23		*/
	ldouble ulbt    ; /* 협의대량매매상한수량	FLOAT128	23		*/
	ldouble llbt    ; /* 협의대량매매하한수량	FLOAT128	23		*/
	char    bpid[11]; /* 기준상품ID	            String	    11		 */
	char    spid[11]; /* 부상품ID	            String	    11		 */
	char    stwk[ 2]; /* 결제주	                String	    2		결제주(W1,W2,W3,W4,W5) */
	uint    nibp    ; /* 기준상품종목수	        Int	        6		 */
	uint    nisp    ; /* 부상품종목수	        Int	        6		 */
	YMD_T   ssdt    ; /* 휴면지정일자	        String	    8		*/
	char    supn    ; /* 휴면여부	            String	    1		*/
	char    fil8[ 7];
            

	double dummy[134];
} FMAST_T;


#endif
