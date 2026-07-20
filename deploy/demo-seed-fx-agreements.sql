-- ===========================================================================
-- 데모/모의거래용 고객약정 시드 (엔진 Oracle 데이터)
--
-- 운영에서는 고객이 약정을 맺으면 EAI 로 TB_FXB_CSC001M(고객약정정보기본) 등이
-- 적재된다. 데모/모의거래 환경에는 EAI 가 없으므로 이 스크립트로 최소 세트를
-- 직접 심는다 — 고객목록조회(W1401) 등 매매 화면 조회까지 동작하도록.
--
-- 대상 스키마: trn 런타임 접속 계정 (DB2USR, 기본 FXPL). 아래 &&SCHEMA 로 지정.
-- 멱등: 마커 FX_SYS_LSMD_ID='DEMOSEED' 로 기존 데모행 삭제 후 재삽입.
--       데모 데이터 제거: 각 테이블에서 DELETE WHERE FX_SYS_LSMD_ID='DEMOSEED'.
--
-- **운영 DB 에는 절대 실행 금지.** 데모/개발 전용.
--
-- 실행:
--   sqlplus <user>/<pw>@<tns> @deploy/demo-seed-fx-agreements.sql
--   (스키마를 물으면 FXPL 입력, 또는 DEFINE SCHEMA=FXPL 로 무프롬프트)
--
-- 생성물 (10 고객):
--   CMC001F  부점 1건            (MNGM_BRCD=9999 데모부점)
--   CSC001M  고객약정 10건        (WBCS_RLNM_ALTR_NO 9000000001..9000000010)
--   CSC004M  사용자 10건          (FX_USER_NO demo01..demo10, LGN_ID 동일)
--   CSC005R  사용자-고객 관계 10건
--   CSC003M  결제계좌 20건        (고객당 원화1 + 외화1)
--
-- 검증: 고객목록조회(W1401S01)를 **관리부점 필터 없이**(mngmBrcd 공란) 호출하면
--       10건이 그대로 조회된다. 특정 부점코드로 필터하면 엔진 쿼리의
--       CHAR(6) = RTRIM(VARCHAR2) 비교가 non-blank-padded 라 '9999  ' != '9999'
--       로 0건이 된다(엔진측 기존 quirk — 전체조회/self-비교 경로는 정상).
--       데모 시연은 전체 조회로 진행할 것.
-- ===========================================================================

SET SERVEROUTPUT ON
SET DEFINE ON
-- 스키마 미지정 시 프롬프트 (운영 계정 오적용 방지 — 반드시 확인).
ACCEPT SCHEMA CHAR PROMPT '대상 스키마 (예: FXPL): ' DEFAULT 'FXPL'

DECLARE
    v_schema   VARCHAR2(30) := UPPER('&&SCHEMA');
    v_marker   CONSTANT VARCHAR2(30) := 'DEMOSEED';
    v_brcd     CONSTANT VARCHAR2(6)  := '9999';        -- 데모부점
    v_cnt      CONSTANT PLS_INTEGER  := 10;            -- 데모 고객 수
    v_cust     VARCHAR2(16);
    v_user     VARCHAR2(30);
    v_nm       VARCHAR2(40);

    -- 데모행 전체 삭제 (멱등) — 자식→부모 순서.
    PROCEDURE purge IS
    BEGIN
        FOR t IN (SELECT column_value tbl FROM TABLE(sys.odcivarchar2list(
                    'TB_FXB_CSC003M','TB_FXB_CSC005R','TB_FXB_CSC004M',
                    'TB_FXB_CSC001M','TB_FXB_CMC001F'))) LOOP
            EXECUTE IMMEDIATE 'DELETE FROM '||v_schema||'.'||t.tbl
                              ||' WHERE FX_SYS_LSMD_ID = :1' USING v_marker;
        END LOOP;
    END;
BEGIN
    purge;

    -- 부점 (W1401 은 CSC001M.MNGM_BRCD = CMC001F.MNGM_BRCD INNER 조인 —
    -- 부점이 없으면 고객이 목록에서 사라진다).
    EXECUTE IMMEDIATE 'INSERT INTO '||v_schema||'.TB_FXB_CMC001F '
      ||'(MNGM_BRCD, MNGM_BRM, BRNC_DCD, USE_YN, FX_SYS_LSMD_ID, FX_SYS_LSMD_TS) '
      ||'VALUES (:1, :2, :3, ''Y'', :4, SYSDATE)'
      USING v_brcd, '데모부점', '0', v_marker;

    FOR i IN 1 .. v_cnt LOOP
        v_cust := '90000000' || LPAD(i, 2, '0');   -- 9000000001 .. 9000000010
        v_user := 'demo'      || LPAD(i, 2, '0');   -- demo01 .. demo10
        v_nm   := '데모고객'  || LPAD(i, 2, '0');

        -- 고객약정정보기본
        EXECUTE IMMEDIATE 'INSERT INTO '||v_schema||'.TB_FXB_CSC001M '
          ||'(WBCS_RLNM_ALTR_NO, CUS_ABBR_NM, CIF_NO, MNGM_BRCD, '
          ||' FX_CUS_STTS_DCD, CUS_CLCD, CIF_DCD, FWEX_TRN_RNG_DCD, TRLT_STTS_DCD, '
          ||' CUS_JNNG_YMD, RISK_AGRM_YN, EDPS_CSN, DRTR_CUS_YN, AGN_TRN_CUS_YN, '
          ||' FX_SYS_LSMD_ID, FX_SYS_LSMD_TS) '
          ||'VALUES (:1, :2, :3, :4, ''1'', ''320'', ''31'', ''3'', ''1'', '
          ||' ''20250101'', ''Y'', 0, ''N'', ''N'', :5, SYSDATE)'
          USING v_cust, v_nm, v_cust, v_brcd, v_marker;

        -- 사용자정보기본 (로그인 대상 — skip-cert 모드는 이 행이 미리 있어야 함).
        -- 동적 SQL 은 placeholder 를 위치로 바인딩하므로 :n 재사용 없이 매 자리 바인드.
        EXECUTE IMMEDIATE 'INSERT INTO '||v_schema||'.TB_FXB_CSC004M '
          ||'(FX_USER_NO, FX_USER_NM, FX_USER_ANM_NM, LGN_ID, '
          ||' SCRE_ACS_ATHR_DCD, FX_USER_DCD, USER_LVL_DCD, USER_STTS_DCD, '
          ||' SECU_MDIA_DCD, ACN_PWD_SECU_LVL, FX_SYS_LSMD_ID, FX_SYS_LSMD_TS) '
          ||'VALUES (:1, :2, :3, :4, ''XX'', ''TR'', ''XX'', ''1'', '
          ||' ''0'', ''1'', :5, SYSDATE)'
          USING v_user, v_nm, v_user, v_user, v_marker;

        -- 사용자-고객번호 관계 (매매 시 로그인 사용자 ↔ 고객 매핑)
        EXECUTE IMMEDIATE 'INSERT INTO '||v_schema||'.TB_FXB_CSC005R '
          ||'(WBCS_RLNM_ALTR_NO, FX_USER_NO, FX_SYS_LSMD_ID, FX_SYS_LSMD_TS) '
          ||'VALUES (:1, :2, :3, SYSDATE)'
          USING v_cust, v_user, v_marker;

        -- 결제계좌: 원화(1) + 외화(2). W1401 은 OUTER 조인 (없어도 목록엔 나옴)
        -- 이지만 모의거래 결제 경로 대비해 채운다.
        EXECUTE IMMEDIATE 'INSERT INTO '||v_schema||'.TB_FXB_CSC003M '
          ||'(WBCS_RLNM_ALTR_NO, FX_SLACT_DCD, FX_STLM_ACN, FX_SLACT_YN, '
          ||' STLM_ACSS_DCD, TRN_USE_YN, HOST_ACNT_EXSN_YN, CUS_ACNT_BAL, '
          ||' FX_SYS_LSMD_ID, FX_SYS_LSMD_TS) '
          ||'VALUES (:1, ''1'', :2, ''Y'', ''1'', ''Y'', ''Y'', 1000000000, :3, SYSDATE)'
          USING v_cust, 'DEMO'||LPAD(i,2,'0')||'WON', v_marker;

        EXECUTE IMMEDIATE 'INSERT INTO '||v_schema||'.TB_FXB_CSC003M '
          ||'(WBCS_RLNM_ALTR_NO, FX_SLACT_DCD, FX_STLM_ACN, FX_SLACT_YN, '
          ||' STLM_ACSS_DCD, TRN_USE_YN, HOST_ACNT_EXSN_YN, CUS_ACNT_BAL, '
          ||' FX_SYS_LSMD_ID, FX_SYS_LSMD_TS) '
          ||'VALUES (:1, ''2'', :2, ''Y'', ''1'', ''Y'', ''Y'', 1000000, :3, SYSDATE)'
          USING v_cust, 'DEMO'||LPAD(i,2,'0')||'FCY', v_marker;
    END LOOP;

    COMMIT;
    DBMS_OUTPUT.PUT_LINE('DEMO SEED 완료: schema='||v_schema
        ||' 부점=1 고객='||v_cnt||' 사용자='||v_cnt||' 계좌='||(v_cnt*2));
END;
/

-- 검증 요약
SELECT 'CMC001F' t, COUNT(*) c FROM &&SCHEMA..TB_FXB_CMC001F WHERE FX_SYS_LSMD_ID='DEMOSEED'
UNION ALL SELECT 'CSC001M', COUNT(*) FROM &&SCHEMA..TB_FXB_CSC001M WHERE FX_SYS_LSMD_ID='DEMOSEED'
UNION ALL SELECT 'CSC004M', COUNT(*) FROM &&SCHEMA..TB_FXB_CSC004M WHERE FX_SYS_LSMD_ID='DEMOSEED'
UNION ALL SELECT 'CSC005R', COUNT(*) FROM &&SCHEMA..TB_FXB_CSC005R WHERE FX_SYS_LSMD_ID='DEMOSEED'
UNION ALL SELECT 'CSC003M', COUNT(*) FROM &&SCHEMA..TB_FXB_CSC003M WHERE FX_SYS_LSMD_ID='DEMOSEED';

UNDEFINE SCHEMA
