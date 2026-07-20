# 데모/모의거래용 고객약정 시드 런북

EC2(엔진 연동) 환경에서 **모의거래·시연**을 위해 고객약정 데이터를 엔진 Oracle 에
직접 심는 절차. 운영에서는 고객이 약정을 맺으면 EAI 로 `TB_FXB_CSC001M`(고객약정
정보기본) 등이 적재되지만, 데모/개발 환경엔 EAI 가 없으므로 이 스크립트로 최소
세트를 넣는다.

스크립트: `deploy/demo-seed-fx-agreements.sql`

## 무엇을 넣나 (10 고객 풀세트)

| 테이블 | 내용 | 건수 |
|-------|------|-----|
| `TB_FXB_CMC001F` | 부점정보 (데모부점 `9999`) | 1 |
| `TB_FXB_CSC001M` | 고객약정 (`9000000001`~`9000000010`) | 10 |
| `TB_FXB_CSC004M` | 사용자 (`demo01`~`demo10`, LGN_ID 동일) | 10 |
| `TB_FXB_CSC005R` | 사용자-고객 관계 | 10 |
| `TB_FXB_CSC003M` | 결제계좌 (고객당 원화1+외화1) | 20 |

W1401(고객목록조회)이 CSC001M↔CMC001F 를 INNER 조인하므로 부점이 반드시 있어야
고객이 화면에 뜬다. 계좌(CSC003M)·코드(CMC003D)는 OUTER 라 목록엔 선택적이지만
모의거래 결제 경로 대비해 함께 넣는다.

## 대상 스키마 — 반드시 trn 런타임 계정

trn 프로세스는 `DB2USR` 계정(기본 **FXPL**)으로 접속한다. 실행 중인 W1101 등의
`/proc/<pid>/environ` 에서 확인:

```bash
PID=$(pgrep -f "W1101 -edom" | head -1)
sudo cat /proc/$PID/environ | tr '\0' '\n' | grep -E 'DB2USR|DB2NAM'
```

시드는 이 스키마(FXPL)에 넣어야 trn 이 읽는다. (고객 실데이터가 FXBAPP 에만
있고 FXPL 이 비어 로그인/조회가 안 되던 것도 이 스키마 불일치가 원인 —
운영 전환 시 trn 이 fxpl/fxbapp 어느 쪽을 정본으로 볼지 정리 필요.)

## 실행

```bash
# EC2 에서 (rocky), 스크립트 업로드 후
export ORACLE_HOME=/opt/oracle/product/19c/dbhome_1
export PATH=$ORACLE_HOME/bin:$PATH LD_LIBRARY_PATH=$ORACLE_HOME/lib
export NLS_LANG=KOREAN_KOREA.AL32UTF8

echo "FXPL" | sqlplus -S <admin>/<pw>@//<rds-host>:1521/ORCL \
  @deploy/demo-seed-fx-agreements.sql
```

멱등이다 — 마커 `FX_SYS_LSMD_ID='DEMOSEED'` 로 기존 데모행을 지우고 다시 넣으므로
반복 실행 안전. 끝에 테이블별 건수 요약이 출력된다 (부점1/고객10/사용자10/관계10/계좌20).

## 검증 — 매매 화면 조회

skip-cert 로그인으로 JWT 받아 W1401 고객목록조회 (**관리부점 필터 없이**):

```bash
JWT=$(curl -s -XPOST http://127.0.0.1:8080/v1/login \
  -H 'Content-Type: application/json' -d '{"data":{"lgnId":"demo01"}}' \
  | python3 -c 'import sys,json;print(json.load(sys.stdin)["access_token"])')

curl -s -XPOST http://127.0.0.1:8080/v1/tx \
  -H "Authorization: Bearer $JWT" -H 'Content-Type: application/json' \
  -d '{"exchange":"dom","routing_key":"W1401S01","data":{"prGb":"1"}}'
```

`grid01_cnt=10` + `rcod=91003 조회가 완료되었습니다` 면 정상.

### 주의 — 관리부점 필터는 비워라

`mngmBrcd` 를 특정 부점으로 채워 호출하면 **0건**이 나온다. 엔진 쿼리가
`CSC001M.MNGM_BRCD = NVL(RTRIM(:vMngmBrcd), ...)` 인데 `RTRIM` 이 VARCHAR2 를
반환해 `CHAR(6)` 컬럼과 non-blank-padded 비교(`'9999  ' != '9999'`)가 되기 때문.
전체 조회(공란, self-비교)는 정상. 엔진측 기존 quirk 이며 데모 시연은 전체
조회로 진행한다.

## 데모 데이터 제거

```sql
DELETE FROM FXPL.TB_FXB_CSC003M WHERE FX_SYS_LSMD_ID='DEMOSEED';
DELETE FROM FXPL.TB_FXB_CSC005R WHERE FX_SYS_LSMD_ID='DEMOSEED';
DELETE FROM FXPL.TB_FXB_CSC004M WHERE FX_SYS_LSMD_ID='DEMOSEED';
DELETE FROM FXPL.TB_FXB_CSC001M WHERE FX_SYS_LSMD_ID='DEMOSEED';
DELETE FROM FXPL.TB_FXB_CMC001F WHERE FX_SYS_LSMD_ID='DEMOSEED';
COMMIT;
```

## 로그인 연계

`--login-skip-cert` 모드(인증서 미적용 과도기)에서는 `data.lgnId` 로 로그인하며,
그 사용자가 CSC004M 에 미리 있어야 한다 — 이 시드가 `demo01`~`demo10` 을 넣으므로
바로 로그인 가능. 인증/로그인 상세는 `docs/auth.md` §3.1.
