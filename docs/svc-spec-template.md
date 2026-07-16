			# 서비스 명세 템플릿 (svc spec)

매매 TR 서비스 1개를 **① I/O 전문(`.h`) + ② 구현(`.pc`) + ③ 사용 DB + ④ 로직**
4단으로 기술한다. `.h` 는 WTG `pkg/svcio` 파서가 읽어 `/v1/tx` 고정폭 전문 자동조립 +
OpenAPI 생성에 쓰는 **기계 판독 원본**이고, 본 명세는 그 위에 구현/DB/로직을 얹은
**사람 판독 문서**다. `.h` 는 절대 이 문서로 대체하지 말고 원본을 그대로 유지한다.

- I/O 헤더: `win/src/inc/trn/WnnnnSnn.h` (파일명 = TR 코드)
- 구현: `win/src/trn/Wnnnn/WnnnnSnn.pc`
- `.h` 작성 규칙: `pkg/svcio` 파서가 `_I`/`_O` 접미사로 Input/Output 판별, `char[크기]`
  → string(maxLength), `int/long/short`→integer, `double/float`→number,
  `*_cnt` 필드 뒤 `orec[1]` → 가변 grid 자동 인식. (상세는 §부록)

---

## 작성 순서 (신규 TR)

1. `.pc` 에서 테이블 추출 → §4
   ```bash
   f=win/src/trn/Wnnnn/WnnnnSnn.pc
   command grep -noE "TB_[A-Z0-9_]+" "$f" | awk -F: '{print $2}' | sort | uniq -c | sort -rn   # 테이블 빈도
   command grep -nE "TB_[A-Z0-9_]+ +[A-Z]* *--" "$f"                                            # 별칭+한글명
   command grep -niE "insert into|update |delete from" "$f"                                      # DML 여부(조회 vs 등록)
   ```
2. `.pc` 에서 흐름 추출 → §3, §5
   ```bash
   command grep -nE "^int WnnnnSnn" "$f"                          # 진입/서브 함수
   command grep -niE "EXEC SQL (DECLARE|OPEN|FETCH|CLOSE)" "$f"   # 커서 구간
   command grep -niE "l_dbcommit|l_dbrollback|RET_SUCCESS" "$f"   # 트랜잭션 지점
   ```
3. `commit/rollback` 지점과 DML 여부 명시 → §5
4. `.h` 의 `_I`/`_O` 를 §2 표로 옮김 (파서용 `.h` 원본은 그대로 둔다)

---

# 서비스 명세 — `<CODE>` (`<이름>`)

## 1. 개요

| 항목 | 값 |
|---|---|
| 서비스 코드 | `<WnnnnSnn>` |
| 이름 | `<한글 이름 — .pc 상단 Components 주석>` |
| 유형 | `<조회 / 신규 / 정정 / 취소 / 등록>` |
| 구현 | `win/src/trn/<Wnnnn>/<WnnnnSnn>.pc` (`<라인수>` lines) |
| I/O 헤더 | `win/src/inc/trn/<WnnnnSnn>.h` |
| 라우팅 alias | `<alias>` → `POST /v1/tx {"alias":"<alias>","data":{…}}` |
| 채널 | `<진입부 comhdr_i->ctyp → chnl 매핑 요약>` |

## 2. I/O 전문 (`.h`)

**Input `<CODE>_I`**

| 필드 | 크기 | 설명 |
|---|---|---|
| `<field>` | `<n>` | `<한글 설명>` |
| … | | |

**Output `<CODE>_O`**

| 필드 | 크기 | 설명 |
|---|---|---|
| `<grid_cnt>` | `<n>` | 그리드 건수 |
| `orec[]` | (가변 grid) | `<주요 칸 나열: field1, field2, …>` |

> `.h` 원본을 그대로 옮긴다. `orec[1]` + 앞의 `*_cnt` 는 파서가 가변 배열로 자동 인식.

## 3. 구현 흐름 (`.pc`)

```
<CODE>()                        ← 진입 (dispatch)
  ├─ gethdr / getmid / getmod   COMHDR 파싱 + input(mid) + output(mod) 버퍼 준비
  ├─ switch(comhdr_i->ctyp)     매체코드 → comhdr_i->chnl 매핑
  ├─ retc = <CODE>_Fn(...)      ← 실제 로직 (조회/등록)
  └─ retc==RET_SUCCESS ? l_dbcommit() : l_dbrollback()   + fnSetMsg(e_msg)

<CODE>_Fn()                     ← 로직
  ├─ memset(mod, <CODE>_O_SZ) + 입력필드 바인딩(zsedit/memcpy)
  ├─ <SELECT: EXEC SQL OPEN 커서 → FETCH 루프 → grid_cnt 세팅 → CLOSE>
  │  <또는 등록: EXEC SQL INSERT/UPDATE/DELETE>
  └─ retc 반환
```

## 4. 사용 DB 테이블

| 테이블 | 한글명 | 별칭 | 접근 | 용도 |
|---|---|---|---|---|
| `TB_FXB_xxxxxx` | `<한글명>` | `<A>` | `SELECT/INSERT/UPDATE/DELETE` | `<용도>` |
| … | | | | |

*DML 요약: `<조회 전용(SELECT-only) / TRGnnn 에 INSERT + …>`*

## 5. 로직 / 필터 규칙

- **커서/DML**: `<커서명 + 조인 테이블 요약, 또는 DML 문 요약>`
- **동적 필터**: `<입력값 있을 때만 거는 조건들 — inqStYmd~inqEdYmd, 코드 필터 등.
  DECODE(:param,…)+DUAL UNION ALL '미입력=전체' 패턴 여부>`
- **트랜잭션**: `<RET_SUCCESS→commit / 실패→rollback>`
- **에러**: `e_msg`(E_MSG) → `fnSetMsg` 로 COMHDR 출력. 응답의 `<ercd 필드>` 에 노출.
  → WTG `/v1/tx` 는 `errn`≠0 이어도 전문 있으면 200, `errn` 본문 동봉(레거시 COMHDR 규약).

---

# 부록 A. 작성 예시 — `W3500S01` (주문내역조회)

## 1. 개요

| 항목 | 값 |
|---|---|
| 서비스 코드 | `W3500S01` |
| 이름 | 주문내역조회 |
| 유형 | 조회 (SELECT-only) |
| 구현 | `win/src/trn/W3500/W3500S01.pc` (1,135 lines) |
| I/O 헤더 | `win/src/inc/trn/W3500S01.h` |
| 라우팅 alias | `W3500S01` → `POST /v1/tx {"alias":"W3500S01","data":{…}}` |
| 채널 | 진입부 `comhdr_i->ctyp` → `chnl` 매핑 (HTS/EMP/WTS/API…) |

## 2. I/O 전문 (`.h`)

**Input `W3500S01_I`** (주요 필드)

| 필드 | 크기 | 설명 |
|---|---|---|
| `wbcsRlnmAltrNo` | 16 | 전행고객실명대체번호 |
| `inqStYmd` / `inqEdYmd` | 8 / 8 | 조회 시작/종료일자 |
| `fxPdcd` | 3 | FX상품코드 (SPT/FWD/SWP/SPM/FWM) |
| `crncPairId` | 7 | 통화페어ID (USD/KRW) |
| `ordnSttsDcd` | 1 | 주문상태 (0접수전/1접수/2부분체결/3완료/9거부) |
| `cnttsYn` | 1 | 체결여부 (관리자용) |

**Output `W3500S01_O`** — `grid01_cnt`(6) + `orec[1]` 가변 grid (약 60칸:
`ordnNo`, `crncPairId`, `byselDcd`, `fxOrdnPrc`, `ercdConvCon` …)

## 3. 구현 흐름 (`.pc`)

```
W3500S01()                      ← 진입 (dispatch)
  ├─ gethdr / getmid / getmod   COMHDR 파싱 + input(mid) + output(mod) 버퍼 준비
  ├─ switch(comhdr_i->ctyp)     매체코드 → comhdr_i->chnl 매핑
  ├─ retc = W3500S01_F1(...)    ← 실제 조회 로직
  └─ retc==RET_SUCCESS ? l_dbcommit() : l_dbrollback()   + fnSetMsg(e_msg)

W3500S01_F1()                   ← 조회
  ├─ memset(mod, W3500S01_O_SZ) + 입력필드 memset/zsedit 바인딩
  ├─ EXEC SQL OPEN  W3500S01_C1
  ├─ FETCH 루프 → orec[i] 채움, i 증가
  ├─ sprintf(grid01_cnt, "%d", i)
  └─ EXEC SQL CLOSE W3500S01_C1
```

## 4. 사용 DB 테이블

| 테이블 | 한글명 | 별칭 | 접근 | 용도 |
|---|---|---|---|---|
| `TB_FXB_TRG001L` | TRD주문내역기본 | A | SELECT | **주 조회 대상** |
| `TB_FXB_TRG002L` | TRD주문내역상세 | D=NEAR, E=FAR | SELECT | swap 2-leg 조인 |
| `TB_FXB_TRG003L` | TRD주문내역(존재체크) | I | SELECT | `EXISTS` 서브쿼리 |
| `TB_FXB_CSC001M` | 고객약정정보기본 | B | SELECT | 고객명 조인 |
| `TB_FXB_CSC004M` | 사용자정보기본 | T1 | SELECT | 관리자 권한 필터 |
| `TB_FXB_CSC011M` | 사용자펀드기본 | Z | SELECT | 펀드(고객ID) 매핑 |
| `TB_FXB_CMC001F` | 부점정보인터페이스 | C | SELECT | 관리부점명 |
| `TB_FXB_CMC003D` | 코드정보 | F~L (7회) | SELECT | 코드→명 decode |
| `TB_FXB_CMG004M` | 통화페어기본 | M | SELECT | 통화페어명 |

*DML 요약: 조회 전용(SELECT-only) — INSERT/UPDATE/DELETE 없음.*

## 5. 로직 / 필터 규칙

- **커서** `W3500S01_C1` — TRG001L 기준 8개 테이블 조인 + CMC003D 7회 self-join 으로
  코드명 decode.
- **동적 필터** (입력값 있을 때만): `inqStYmd~inqEdYmd`, `fxPdcd`, `crncPairId`,
  `ordnSttsDcd`, `cnttsYn`(`CNTT_STTS_DCD='6'`=체결), `ordnNo`, `mngmBrcd` 등.
  `DECODE(:param,…)+DUAL UNION ALL` 패턴으로 "미입력=전체" 구현.
- **트랜잭션**: 조회지만 관행적으로 `RET_SUCCESS→commit / 실패→rollback`.
- **에러**: `e_msg`(E_MSG) → `fnSetMsg` 로 COMHDR 출력. 응답의 `ercdConvId`/
  `ercdConvCon` 에 변환된 오류 노출.

---

# 부록 B. `.h` 작성 규칙 (svcio 파서)

| 항목 | 규칙 |
|---|---|
| 입력/출력 구분 | `typedef struct { … } <CODE>_I;` = Input, `<CODE>_O;` = Output. 접미사로 판별하고 코드도 추출. |
| 필드 1칸 | `char 이름 [크기]; // 한글주석` — 주석이 OpenAPI 필드 설명. |
| 타입 매핑 | `char`→string(maxLength=크기), `int/long/short/unsigned`→integer, `double/float`→number |
| 그리드(반복) | 인라인 `struct { … } orec[N];` 또는 외부 `_R` typedef 참조. `*_cnt`/`*_count` 필드 뒤 `orec[1]` → **가변 배열**(N행) 자동 재분류. 명시적 `orec[]` 도 가변. |
| 주석 인코딩 | UTF-8 저장(파서가 CP949 도 자동 변환). |
| 무시 | `#include`, `#define …_SZ`, 상단 라이선스 주석, 전역 `char pname[10]` 등. |

**최소 `.h` 골격**

```c
#ifndef __W9100S01__H__
#define __W9100S01__H__

typedef struct {                        // Input
    char    acntNo          [ 20];  //  계좌번호
    char    inqStYmd        [  8];  //  조회시작일자
    int     pageNo;                 //  페이지번호
} W9100S01_I;

typedef struct {                        // Output
    char    acntNm          [ 50];  //  계좌명
    char    grid01_cnt      [  6];  //  그리드건수 (뒤 orec[1] → 가변 grid)
    struct {
        char    trnYmd      [  8];  //  거래일자
        char    trnAmt      [ 23];  //  거래금액
    } orec[1];
} W9100S01_O;

#endif
```

## 작성 후 확인

1. `.h` 저장 → mci-admin 재기동(또는 svc-io reload)
2. 관리자 콘솔 **서비스 I/O** 페이지에 자동 등장 → **⬇ JSON** / Swagger viewer 로 확인
3. alias 등록 → `POST /v1/tx {"alias":"<CODE>","data":{…}}` 호출 (고정폭 전문 자동조립)
