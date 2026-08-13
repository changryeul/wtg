# 선물/파생 시세 → 원격 web 전달 — WTG 처리 정의

> 기존 유안타 선물/채권 시세 피드 핸들러(`~/mywork/yuanta/sise`)를 WTG 로 **원격 web
> 클라이언트에 전달**하기 위한 처리 방식 정의. 구현 전 설계 합의용.
> 조사 근거: yuanta/sise 아키텍처 맵 + WTG push 트랙(`pkg/quote/pushdata.go`,
> `mci-push`/`mci-edge-push`, `cside/wtgpush`).

## 1. 핵심 통찰 — 기존 피드는 **이미 WTG push 언어를 쓴다**

기존 선물시세 배포(`kbfut_sise` → `myrq_push()`)의 봉투는 `pushdata_t`:
`mkid=30`(마켓ID) + `pushmsg{ symb[20]=종목, mask=PUSH_QUOT, type, msgb[전문] }`.

WTG 의 push 트랙(`pkg/quote/pushdata.go` `EncodePushdata`/`DecodePushData`)이 쓰는
봉투가 **정확히 동일** (`mkid` + `pushmsg{symb[20], mask, type, msgb[1512]}`).

→ **결론**: 선물시세는 WTG 의 **FX quote 트랙(bid/ask 모델)이 아니라 push 트랙에
매핑**된다. 데이터 모델 재설계 없이 배포 경로만 WTG 로 바꾸면 원격 web 도달.

## 2. 기존 아키텍처 (3단계)

| 단계 | 기존 (yuanta/sise, C) |
|---|---|
| **수신** | UDP mcast `227.10.20.10:6064x`(주간)/`6065x`(야간)/`6063x`(채권) → 앞 5B TR 화이트리스트 필터(A006F/B606F/A306F/G706F…) → SysV msgq(내부 IPC) |
| **파싱** | msgq 소비 → TR별 디스패치 → `KBFUT_T`(마스터+체결+호가 5단+분봉) 구조체 |
| **배포** | **(A) SHM `/mfsise`** 현재가 스냅샷(클라 mmap+bsearch) + **(B) MyMQ push** `myrq_push(pushdata_t)` — 본문 wire `KF_CHEG_RTS_T"KA"`(체결)/`KF_HOGA_RTS_T"KB"`(호가), 채권 `"BA"/"BB"` 고정폭 ASCII |

데이터 모델(선물): 마스터(종목/만기/기준가/상하한) + 체결(현재가·시가·고가·저가·거래량·
거래대금·정산가·근원월물) + 호가(5단계 매도/매수 가격·잔량·건수·예상체결) + 분봉.
→ FX `Quote{bid,ask}` 로는 표현 불가. **push msgb(KA/KB)를 그대로 운반**하는 게 정답.

## 3. WTG 매핑 (3단계 → WTG)

| 단계 | WTG 정의 |
|---|---|
| **수신** | (트랙1) 기존 C 피드 그대로 / (트랙2) `futures-forwarder`(Go) 신규 — `quote-forwarder` 패턴, KRX mcast + TR 파서 |
| **파싱** | (트랙1) 기존 C KBFUT_T 유지 / (트랙2) Go 선물 모델 재구현 |
| **배포** | **push 트랙**: `mci-push`(pushdata_t 수신) → `mci-edge-push`(DMZ) → **web WebSocket fan-out**. 구독 필터 = `symb`(종목)/`mask`(시세). + 초기 **snapshot REST** |

## 4. 통합 지점 — 기존 배포를 WTG push 로 (2 방식)

**(권장) HTTP push — `cside/wtgpush` 로 교체**
- 기존 `myrq_push(&push)` → `wtg_push_send(...)` **한 줄 교체** (libwtgpush.a, 외부 의존 0).
- 기존 C 피드가 `mci-push` HTTP endpoint 로 직접 전송 (broker 우회, secret/mTLS).
- `mci-push` → `mci-edge-push` → web ws. WTG 가 broker SIGABRT 부하 회피 목적으로
  만든 트랙(Phase 2.x)이라 정확히 이 용도. **수신/파싱 C 코드는 무변경.**

**(대안) broker 경유**
- 피드 push → mymqd(broker) → `mci-push`(QF_UNSOL_REP representative receiver) subscribe.
- 기존이 별도 myrqd 면 broker 통합/브리지 필요. HTTP push 대비 홉·의존 증가.

## 5. web 전달 wire — **JSON 확정** (신규 web, 2025 결정)

신규 web 클라이언트가 대상이므로 **처음부터 JSON**. passthrough 는 채택 안 함.

**변환 위치 = WTG(Go) codec** (권장·확정 방향):
- C 피드는 이미 `KF_CHEG_RTS_T`("KA")/`KF_HOGA_RTS_T`("KB") 고정폭 전문을 생성 —
  이미 KRX TR 파싱을 끝낸 **깨끗한 레이아웃**(fpush.h). KA/KB→JSON 은 단순 오프셋
  매핑이라 Go codec 이 가볍다 (KRX 원 TR 파서 재구현 아님).
- 따라서 **C 변경 최소**(`wtg_push_send` 로 KA/KB 그대로 전송) + **WTG 가 web JSON
  스키마(계약)를 소유**. FX 시세 ws 가 이미 JSON envelope 이라 일관.
- codec 위치: `pkg/quote` 에 futures 타입 + `DecodeKF`(KA/KB→struct→JSON). push 경로
  (mci-push 수신 직후 or edge 직전)에서 디코드 후 ws.

### JSON envelope 스키마 (초안)

**체결 (KA → `fut.trade`)**
```json
{
  "kind": "fut.trade", "code": "101V6000", "time": "090005123",
  "last": 265.75, "sign": "+", "diff": 0.25, "rate": 0.09,
  "open": 265.50, "high": 265.75, "low": 265.00, "prevClose": 265.50,
  "settle": 265.60, "cvol": 3, "tvol": 12345, "tamt": 3271500000,
  "nearPrc": 265.75, "farPrc": 266.10, "upLimit": 291.00, "dnLimit": 240.00,
  "bs": "2"
}
```
(매핑: eprc→last, diff/sign/rate, oprc/hprc/lprc→open/high/low, yprc→prevClose,
sprc→settle, cvol/tvol/tamt, nprc/fprc→near/farPrc, uprc/dprc→up/dnLimit, bscd→bs)

**호가 (KB → `fut.book`, 5단계)**
```json
{
  "kind": "fut.book", "code": "101V6000", "time": "090005123",
  "askTot": 1520, "bidTot": 1340, "expPrc": 265.75, "expVol": 40,
  "ask": [{"prc":265.80,"vol":300,"cnt":12}, ... 5단],
  "bid": [{"prc":265.75,"vol":280,"cnt":9},  ... 5단]
}
```
(매핑: stvl/btvl→askTot/bidTot, xprc/xvol→expPrc/expVol, sell[5]/buy[5]→ask/bid)

색상(ocol/ecol 등 등락 부호)은 sign/diff 로 web 이 재계산 가능 → JSON 은 수치 중심,
색상은 생략 or `sign` 만.

## 6. 스냅샷 (초기 상태 조회)

web 클라는 구독 시작 시 **현재가 스냅샷**이 필요(이후 push 로 갱신). 기존 SHM `/mfsise`
대응:
- **WTG snapshot REST** `GET /v1/futures/snapshot?symb=...` — FX 의 `/v1/quote/spot` 대응.
- 소스: (a) 기존 SHM read (같은 박스면), 또는 (b) mci-push 가 push 스트림으로 최신
  상태 캐시 유지(BestConsumer 유사) → REST 서빙. **(b)가 WTG 자립적·권장.**

## 7. 두 구현 트랙 (재사용 vs 신규)

**트랙 1 — 기존 C 피드 유지 + wtgpush 브리지 (권장 시작)**
- 검증된 C 수신/파싱 그대로, **배포만 `wtg_push_send`** 로.
- 최소 변경, 즉시 web 도달. mds 대체의 "cside 트랙"과 동형.
- 단점: C 피드 운영 유지, 선물 포맷은 web 이 파싱(Phase 1).

**트랙 2 — 완전 Go 이관 (`futures-forwarder`)**
- KRX mcast 수신 + TR 파서 Go 재구현 + 선물 quote 모델 + fan-out.
- WTG 자립(폐쇄 C 의존 제거), JSON 네이티브. mds→WTG 이관과 동형 장기 과제.
- 단점: TR 파서/모델 재구현 비용 큼(A0/A3/G7/B6/H3… 다수).

## 8. 권장 로드맵 (JSON-first)

- **Stage 0 (관통)**: 기존 C 피드 1종목 → `wtg_push_send`(KA/KB) → mci-push →
  **WTG KA/KB→JSON codec** → edge-push → web ws. e2e JSON 도달·구독필터 검증.
  (codec 은 1 TR = KA 만 먼저, 호가 KB 후속)
- **Stage 1 (web UX)**: 호가(KB)/채권(BA/BB) codec 확장 + snapshot REST + 구독 관리
  (종목별 subscribe/unsubscribe).
- **Stage 2 (선택)**: `futures-forwarder` Go 이관으로 C 피드 은퇴 (KRX TR 파서 Go).

## 9. 결정 현황 / 남은 결정

**확정:**
- ✅ 대상 = 원격 web (신규)
- ✅ wire = **JSON** (passthrough 배제)
- ✅ 변환 위치 = **WTG(Go) codec** (C 는 KA/KB 그대로, 최소 변경)

**남은 결정:**
1. **트랙 1(C 유지+브리지) vs 트랙 2(Go 전면 이관)** — 시작점. (권장: 트랙 1 로 시작,
   codec 만 WTG. 장기 트랙 2)
2. **통합**: HTTP push(`wtg_push_send`, 권장) vs broker 경유.
3. **스냅샷 소스**: 기존 SHM read vs WTG push-stream 캐시. (권장: WTG 캐시)
4. **범위**: 주간선물만 vs 야간·채권 포함 (도메인별 포트/큐 별도).
5. **JSON 스키마 확정**: §5 초안(kind/필드명) 리뷰 — web 팀과 계약.

## 10. 관련 WTG 자산 (재사용)

- `pkg/quote/pushdata.go` — pushdata_t 인/디코더 (봉투 동일)
- `cside/wtgpush/` — C SDK(`wtg_push_send`), 기존 C 피드가 한 줄로 진입
- `internal/push` (`mci-push`) — pushdata 수신 + ws fan-out + HTTP push endpoint
- `internal/edge/push` (`mci-edge-push`) — DMZ web ws fan-out
- `docs/push-*.md` — push 트랙 운영/테스트/보안 가이드

## 11. 정답지 대사 — 트랙2 A306F 체결 등락 (C 피드 fut_real.c 기준)

트랙2(Go raw 파싱)의 값 정확성을 C 피드(`~/mywork/yuanta/sise` = 운영 정답지)와
필드·수식 단위로 대사한 결과. 대상은 표시 필수인 **전일대비(diff/rate/sign)**.

### 11.1 오프셋 대사 — `A306F_T` (inc/A306F.h) ↔ `pkg/krx.DecodeA306F`
| 필드 | C 오프셋 | Go 오프셋 | 폭 | 판정 |
|-----|---------|----------|----|-----|
| code | 17 | 17 | 12 | ✅ |
| time | 35 | 35 | 12 | ✅ |
| cprc(체결가) | 47 | 47 | 9 | ✅ → Last/Cprc |
| cvol | 56 | 56 | 9 | ✅ |
| nprc/fprc(근·원월) | 65/74 | 65/74 | 9 | ✅ |
| oprc/hprc/lprc | 83/92/101 | 83/92/101 | 9 | ✅ |
| pprc(직전가) | 110 | 110 | 9 | ✅ |
| tvol/tamt | 119/131 | 119/131 | 12/22 | ✅ |
| ftcd(매도매수) | 153 | 153 | 1 | ✅ → Bs |
| uldp/lldp(동적상하한) | 154/163 | 154/163 | 9 | ✅ → Up/DnLimit |
| **총 길이** | 173 | `SZA306F`=173 | | ✅ |

스케일: C `l_s2d = atof()` (무스케일), KRX 가격은 소수점 포함 ASCII → Go `ffloat`
(`TrimSpace`+`ParseFloat`) 와 동치. ✅

### 11.2 수식 대사 — 전일대비 (`set_fsise_diff` ↔ `enrichFutTrade`)
C 원문 (fut_real.c:299-342) 을 그대로 이관:
```
yPrc = fsise.yPrc(전일종가);  if(yPrc<=0) yPrc = fsise.bPrc(기준가);   // 대체
sign = ' ';
if(yPrc<=0 || ePrc<=0 || yPrc==ePrc) { diff=0; rate=0; }              // 가드
else { diff = ePrc - yPrc; rate = diff/yPrc*100; sign = rate>0?'+':rate<0?'-':' '; }
```
- yPrc/bPrc 모두 **A006F 마스터**에서 옴 (yprc→PrevClose, bprc→BasePrc) → `MasterCache`.
- 전송 `prevClose` 는 C(`rts->yprc=fsise.yPrc`)와 동일하게 **대체 전 원 전일종가**.
- 최초 이관 시 누락했던 3건을 정답지 대사로 발견·수정:
  1. **기준가 fallback** (yprc≤0 → bprc) 누락 → 신규상장 등락 미표시.
  2. **체결가≤0 가드** 누락 → 음수 diff 오산.
  3. `!=0` → **`<=0`** (음수 전일종가 무효 처리).
- 대사 테스트: `internal/krx/enrich_test.go`(`TestEnrichGroundTruth` 6 케이스 —
  상승/하락/보합/기준가대체/체결가0/둘다0).

### 11.3 직전대비 대사 (`set_fcheg_diff` ↔ `PriceDiff`)
직전대비(직전가 pPrc 대비 등락)는 A306F TR 내부값(cprc,pprc)만으로 계산 →
**decode-time**(`pkg/krx.DecodeA306F` 안 `PriceDiff`). 마스터 불필요.
```
if(cPrc<=0 || pPrc<=0 || cPrc==pPrc) { cDif=0; cRat=0; sign=' '; }
else { cDif=cPrc-pPrc; cRat=cDif/pPrc*100; sign=cRat>0?'+':cRat<0?'-':' '; }
```
- Track1(KA)은 C 가 이미 계산해 실어보내므로 `DecodeKChe` 가 그대로 읽음
  (pprc@209/cdif@218/crat@227/csgn@233).
- FutTrade 필드: `prevTradePrc`/`cdiff`/`crate`/`csign`.

### 11.4 정산가 대사 (H306F ↔ `DecodeH306F`+`applyFutSettle`)
정산가/최종결제가는 A306F 에 없고 **H306F**(IFMSRID0009)로 별도 분배 → C 는
`fsise.sPrc/lsPr/sPcd` 로 보관해 매 KA push 에 실어보냄. WTG 도 동형:
- `DecodeH306F` (53B: code@5, sprc@23[18], spcd@41[2], lspr@43[8], lspc@51) → `FutSettle`.
- `MasterCache.PutSettle` 로 code 별 캐시, `IngestH306F` 가 `fut.settle` 도 fan-out.
- `IngestA306F` 가 `applyFutSettle` 로 후속 체결에 settle/finalSettle/settleCd 주입.
- FutTrade 필드: `settle`/`finalSettle`/`settleCd`.

### 11.5 채권 raw 대사 (A301K 체결 / B601K 호가)
채권 raw TR 은 C 피드에 핸들러가 없어(=런타임 정답지 없음) `.h` 레이아웃 + BA/BB
push 빌더(bpush.h) + 내부 시세 struct(bcheg.h)를 모델로 이관:
- `DecodeA301K`(223B) → `BondTrade`: cprc/cvol/camt/tyld + OHLC + OHL수익률 + tvol/tamt.
- `DecodeB601K`(462B) → `BondBook`: hoga[5] 인터리브(sprc/bprc/svol/bvol/syld/byld), stvl/btvl.
- **직전대비**(diff/rate/sign): A301K 는 직전가 미포함 → `MasterCache.bondPrev` 로 code별
  직전 체결가 보관(`BondPrevAndSet`), 체결가 대비. 모델은 bcheg.h `pPrc`/`cDif`(선물 동형).
- **전일대비**(yDiff/yRate/ySign): 채권은 전일종가 TR 이 없음(A001B 는 bprc 만) →
  **기준가(bprc)** 를 기준으로 계산. C BA push 의 ydif 와 동형(기준가 대비).
- 수식/가드는 선물과 공통 `wire.PriceDiff` (set_fcheg_diff/set_fsise_diff 이관).
- `IngestA001B` 가 이제 마스터를 캐시(PutBond)해야 전일대비가 채워짐(기존 fan-out만 → 수정).
- 테스트: `a301k_test`/`b601k_test`(offset/guard), `enrich_test.TestBondEnrichment`(2틱 e2e).

### 11.6 남은 미이관
- 호가(B606F/B601K): 등락 계산 없음(호가는 대비 미산정) → 대사 불필요.
- 색상코드(COLORD/COLORL): web 이 부호로 자체 렌더 → 미이관.

### 11.7 런타임 대조 — C 오라클 ↔ Go 디코더 (`make verify-krx`)
정적(오프셋/수식) 대사에 더해, **실제 sise 구조체 레이아웃 기준**으로 값을 런타임
대조하는 자동 게이트. C 피드가 raw TR 핸들러를 전부 갖고 있지 않아도(A301K/B601K),
헤더의 struct(`A306F_T` 등, 순수 char[])는 그 자체가 오프셋 정답지다.
- `cside/krxverify/oracle.c` — 원 TR 바이트를 **실제 sise 구조체로 캐스팅**해
  `l_s2d`(=trim+atof) 동형으로 각 필드 파싱 → 정규 CSV.
- `cmd/krx-verify` — `gen`(결정적 length-prefixed 캡처 생성) / `decode`(같은 파일을
  `pkg/krx` 로 디코드 → 동일 CSV).
- `scripts/krx-verify.sh` — gen → C 오라클 CSV → Go CSV → `diff`. 값이 어긋나면
  오프셋/파싱이 C 구조체와 불일치한 것 (컴파일러가 struct 오프셋을 계산하므로
  손으로 센 Go 오프셋 드리프트를 자동 검출).
- 대상 7종: A306F/A301K/B606F/B601K/H306F/A006F/A001B (가격/수량/수익률/문자/승수 등).
- 헤더는 **리포 vendored**(`cside/krxverify/inc/*.h`, 순수 char[] struct)라 `cc` 만
  있으면 **자립 동작**(외부 sise 폴더 불요). 원 sise inc 경로를 인자로 주면 원본 대조도 가능.
  `make ci` 미포함(선택 게이트). WTG↔sise 분리 배경은 그 디렉토리 README 참고.
- **결과: 7 레코드 전 필드 일치** — Go 오프셋/파싱이 C 구조체 레이아웃과 동치 확인.
