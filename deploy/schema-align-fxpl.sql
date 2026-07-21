-- ===========================================================================
-- FXPL 엔진 스키마 정본(table.sql) 정렬 — 드리프트 4개 테이블만 타겟
--
-- 배경: 배포된 주문 코드(WTR005 등)는 신 스키마 컬럼을 조회하는데 RDS 의 일부
-- 테이블이 구 스키마로 남아 ORA-00904 (예: CMG039M.FX_PDCD 없음)로 주문이 막힘.
-- 드리프트 감사 결과 103개 중 4개만 어긋남 (99개 정상). 그 4개만 정렬한다.
--
-- 대상 스키마: FXPL (trn 런타임 계정). **공유 dev RDS — 실행 전 확인.**
-- 안전장치: SQLERROR 즉시 중단 + CMG039M 은 백업 테이블(BAK_*) 보존.
--
-- 실행: sqlplus <admin>/<pw>@//<rds>/ORCL @deploy/schema-align-fxpl.sql
-- ===========================================================================
WHENEVER SQLERROR EXIT SQL.SQLCODE
SET DEFINE OFF
SET SERVEROUTPUT ON

PROMPT ==== 1) CMC004M: SMSS_TGT_DCD 컬럼 추가 (nullable) ====
ALTER TABLE FXPL.TB_FXB_CMC004M ADD (SMSS_TGT_DCD CHAR(1));

PROMPT ==== 2) CMG016M: WBCS_RLNM_ALTR_NO_FND_NO 추가 + backfill + NOT NULL ====
-- 기존 10,897행 보존. 펀드번호 미분화 고객은 _FND_NO = 실명대체번호로 backfill.
-- 정본 PK(_FND_NO 포함)는 기존 무-PK 테이블에 dup 위험이 있어 추가하지 않음
-- (컬럼 존재 + NOT NULL 로 코드의 ORA-00904 만 해소).
ALTER TABLE FXPL.TB_FXB_CMG016M ADD (WBCS_RLNM_ALTR_NO_FND_NO VARCHAR2(30));
UPDATE FXPL.TB_FXB_CMG016M SET WBCS_RLNM_ALTR_NO_FND_NO = WBCS_RLNM_ALTR_NO
 WHERE WBCS_RLNM_ALTR_NO_FND_NO IS NULL;
ALTER TABLE FXPL.TB_FXB_CMG016M MODIFY (WBCS_RLNM_ALTR_NO_FND_NO NOT NULL);

PROMPT ==== 3) CMG020R: WBCS_RLNM_ALTR_NO_FND_NO 추가 + backfill + NOT NULL ====
ALTER TABLE FXPL.TB_FXB_CMG020R ADD (WBCS_RLNM_ALTR_NO_FND_NO VARCHAR2(30));
UPDATE FXPL.TB_FXB_CMG020R SET WBCS_RLNM_ALTR_NO_FND_NO = WBCS_RLNM_ALTR_NO
 WHERE WBCS_RLNM_ALTR_NO_FND_NO IS NULL;
ALTER TABLE FXPL.TB_FXB_CMG020R MODIFY (WBCS_RLNM_ALTR_NO_FND_NO NOT NULL);

PROMPT ==== 4) CMG039M: 백업 -> 재생성(신 스키마) -> 구데이터 변환적재 ====
-- 백업 (이미 있으면 재실행 위해 drop)
BEGIN EXECUTE IMMEDIATE 'DROP TABLE FXPL.BAK_CMG039M_20260721'; EXCEPTION WHEN OTHERS THEN NULL; END;
/
CREATE TABLE FXPL.BAK_CMG039M_20260721 AS SELECT * FROM FXPL.TB_FXB_CMG039M;

DROP TABLE FXPL.TB_FXB_CMG039M CASCADE CONSTRAINTS;

CREATE TABLE FXPL.TB_FXB_CMG039M
(
    CRCD            CHAR(3)     NOT NULL,
    FX_PDCD         CHAR(3)     NOT NULL,
    PDF_TNR_CLSF_ID VARCHAR2(3) NOT NULL,
    APSR_YMD        VARCHAR2(8) NOT NULL,
    FNAP_YMD        VARCHAR2(8) NOT NULL,
    LMT_STTG_HMS    CHAR(6),
    LMT_FNSH_HMS    CHAR(6),
    MKT_STTG_HMS    CHAR(6),
    MKT_FNSH_HMS    CHAR(6),
    USE_YN          CHAR(1) DEFAULT 'Y' NOT NULL,
    FX_SYS_LSMD_ID  VARCHAR2(30) DEFAULT 'SYSTEM' NOT NULL,
    FX_SYS_LSMD_TS  DATE DEFAULT SYSDATE NOT NULL
);
ALTER TABLE FXPL.TB_FXB_CMG039M
  ADD CONSTRAINT PK_FXB_CMG039M PRIMARY KEY (CRCD, FX_PDCD, PDF_TNR_CLSF_ID, APSR_YMD);

-- 구 스키마(CRNC_PAIR_ID/STTG_HMS/FNSH_HMS) -> 신 스키마 변환.
--   CRCD    : 코드의 CASE 규칙 (USD/KRW->USD, USD/xxx->xxx, else 앞통화)
--   FX_PDCD : 구 PDF_TNR_CLSF_ID 가 FWD/MAR 면 그대로, 그 외(SPT/TOD/TOM)는 SPT
--   PDF_TNR : SPT계열은 테너 유지, FWD/MAR 은 'ALL'
--   LMT/MKT : 구 단일창(STTG/FNSH)을 지정가/시장가 양쪽에 동일 적용
-- PK 중복(여러 페어가 같은 CRCD 로 접힘) 은 GROUP BY + MIN 으로 흡수.
INSERT INTO FXPL.TB_FXB_CMG039M
  (CRCD, FX_PDCD, PDF_TNR_CLSF_ID, APSR_YMD, FNAP_YMD,
   LMT_STTG_HMS, LMT_FNSH_HMS, MKT_STTG_HMS, MKT_FNSH_HMS,
   USE_YN, FX_SYS_LSMD_ID, FX_SYS_LSMD_TS)
SELECT crcd, fxpdcd, tnr, apsr,
       MIN(fnap), MIN(sttg), MIN(fnsh), MIN(sttg), MIN(fnsh),
       'Y', 'SCHEMAFIX', SYSDATE
  FROM (
    SELECT
      CASE WHEN CRNC_PAIR_ID = 'USD/KRW'        THEN 'USD'
           WHEN SUBSTR(CRNC_PAIR_ID,1,3) = 'USD' THEN SUBSTR(CRNC_PAIR_ID,5,3)
           ELSE SUBSTR(CRNC_PAIR_ID,1,3) END          crcd,
      CASE WHEN PDF_TNR_CLSF_ID = 'FWD' THEN 'FWD'
           WHEN PDF_TNR_CLSF_ID = 'MAR' THEN 'MAR'
           ELSE 'SPT' END                             fxpdcd,
      CASE WHEN PDF_TNR_CLSF_ID IN ('FWD','MAR') THEN 'ALL'
           ELSE PDF_TNR_CLSF_ID END                   tnr,
      APSR_YMD apsr, FNAP_YMD fnap, STTG_HMS sttg, FNSH_HMS fnsh
      FROM FXPL.BAK_CMG039M_20260721
  )
 GROUP BY crcd, fxpdcd, tnr, apsr;

COMMIT;

PROMPT ==== 검증 ====
SELECT 'CMG039M rows' k, COUNT(*) v FROM FXPL.TB_FXB_CMG039M
UNION ALL SELECT 'USD/SPT/SPT (주문경로)', COUNT(*) FROM FXPL.TB_FXB_CMG039M
  WHERE CRCD='USD' AND FX_PDCD='SPT' AND PDF_TNR_CLSF_ID='SPT'
UNION ALL SELECT 'CMC004M has SMSS_TGT_DCD', COUNT(*) FROM all_tab_columns
  WHERE owner='FXPL' AND table_name='TB_FXB_CMC004M' AND column_name='SMSS_TGT_DCD'
UNION ALL SELECT 'CMG016M has _FND_NO', COUNT(*) FROM all_tab_columns
  WHERE owner='FXPL' AND table_name='TB_FXB_CMG016M' AND column_name='WBCS_RLNM_ALTR_NO_FND_NO'
UNION ALL SELECT 'CMG020R has _FND_NO', COUNT(*) FROM all_tab_columns
  WHERE owner='FXPL' AND table_name='TB_FXB_CMG020R' AND column_name='WBCS_RLNM_ALTR_NO_FND_NO';

PROMPT ==== 완료 ====
EXIT
