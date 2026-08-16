
#ifndef __bmaster_h__
#define __bmaster_h__

typedef struct 
{ 
	char      code[12];    /* 표준종목코드	Not Null	CHARACTER(12) */
	char      fil1[ 4];
    char      ksnm[40];    /* 한글종목약어명		    CHARACTER(40) */
    char      esnm[40];    /* 영문종목약어명		    CHARACTER(40) */
	char      issr[ 5];    /* 채권발행기관코드		CHARACTER(5)  */
	char      isyn    ;    /* 채권발행상장구분		CHARACTER(1)  */
	char      ctcd[ 6];    /* 채권분류코드		    CHARACTER(6)  */
	char      itcd[ 2];    /* 채권이자지급방식구분		CHARACTER(2) */
	char      fil2[ 2];
	YMD_T     bday    ;    /* 적용년월일	Not Null	CHARACTER(8)  */
	YMD_T     ltdt    ;    /* 상장년월일		CHARACTER(8)  */
	YMD_T     isdt    ;    /* 발행년월일		CHARACTER(8)  */
	YMD_T     rddt    ;    /* 상환년월일		CHARACTER(8)  */
	YMD_T     sldt    ;    /* 매출년월일		CHARACTER(8)  */
	YMD_T     fidt    ;    /* 최초이자지급년월일		CHARACTER(8)  */
	YMD_T     prdt    ;    /* 직전이자지급년월일	Not Null	CHARACTER(8) */
	YMD_T     pydt    ;    /* 차기이자지급년월일	Not Null	CHARACTER(8) */
	double    cprt    ;    /* 표면이율	Not Null	DECIMAL(9,5)  */
	int       mccp    ;    /* 이자지급계산월수	Not Null	DECIMAL(3)  */
	char      cptc    ;    /* 채권이자선후급구분		CHARACTER(1)  */
	char      dpcc    ;    /* 채권이자원미만처리구분		CHARACTER(1) */
	char      prcd    ;    /* 물가연동여부		CHARACTER(1)  */
	char      fil3[ 1];
	char      prid[10];    /* 가격단위규칙ID	Not Null	CHARACTER(10) */
	char      fil4[ 6];
	double    isix    ;    /* 발행일참조지수	Not Null	DECIMAL(9,5) */
	double    ulpr    ;    /* 채권상한가	Not Null	DECIMAL(9,2) */
	double    llpr    ;    /* 채권하한가	Not Null	DECIMAL(9,2) */
	double    eprc    ;    /* 채권종가	Not Null	DECIMAL(9,2) */
	double    eyld    ;    /* 종가수익률	Not Null	DECIMAL(22,9) */
	YMD_T     epdt    ;    /* 종가형성년월일	Not Null	CHARACTER(8) */
	YMD_T     stdt    ;    /* 결제년월일	Not Null	CHARACTER(8)  */
	ldouble   prrt    ;    /* 물가연동률	Not Null	DECIMAL(15,8) */
	double    pyld    ;    /* 민간평가수익률	Not Null	DECIMAL(9,5) */
	char      mtcf[ 2];    /* KRX원화채권만기구분	Not Null	CHARACTER(2) */
	char      kbtp    ;    /* KRX원화채권종목구분	Not Null	CHARACTER(1) */
	char      fil5[13];
	double    spr1    ;    /* 지표스프래드1차가격	Not Null	DECIMAL(9,2) */
	double    spr2    ;    /* 지표스프래드2차가격	Not Null	DECIMAL(9,2) */
	double    spr3    ;    /* 지표스프래드3차가격	Not Null	DECIMAL(9,2) */
	ldouble   prix    ;    /* 물가연동계수	Not Null	DECIMAL(15,9) */
	ldouble   prip    ;    /* 물가추정지수가격		DECIMAL(20,8) */
	char      txcd    ;    /* 과세여부	Not Null	CHARACTER(1) */
	char      strp    ;    /* 스트립시장조성여부	Not Null	CHARACTER(1) */
	char      fil6[ 6]; 
	double    bprc    ;    /* 기준가         */
	ulong     seqn    ;    /* 종목일련번호   */

	double   dummy[18]; 

} BMAST_T;


#endif
