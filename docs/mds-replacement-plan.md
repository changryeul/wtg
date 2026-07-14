# mds → WTG 전환 계획서 (완전 폐기)

> **목적**: NH FX 시장 데이터 시스템 (mds, `/Users/winwaysystems/mywork/nmds/mds`) 을
> WTG 시세 파이프라인 (`quote-forwarder` + `mci-price` + `mci-chart` + `mci-admin`
> + `cside/wtgquery`) 으로 **완전 대치**하고 mds 바이너리를 폐기하는 단계별 실행
> 계획. 의사결정자 보고 / NH 설득 자료로 선행 작성 — 승인 후 실행되므로 단계별
> 공수·검증 게이트·롤백 절차를 모두 명시한다.
>
> **판정 근거**: 기능 cover 가능 여부는 `docs/mds-coverage.md` (cover 매트릭스,
> W9501 wire adapter PoC, latency PoC) 가 이미 입증 — 본 문서는 중복 없이
> "어떤 순서로 어떻게 옮기는가" 만 다룬다.

## 1. 결정사항 요약

| 안건 | 결정 |
|---|---|
| 전환 수준 | **mds 완전 폐기** — 축소 잔존 없음 |
| DB write (Oracle) | **교차 소비 실증됨** — mds 수집계가 write 하는 시세 테이블을 trn/lib/bat 26개+ 파일이 read (아래 trn 축 참조). **WTG 가 동일 스키마로 Oracle write 를 승계** — `mci-archive-ora` 신설, 교차 테이블 우선 (Stage 4) |
| **SHM 직접 read 소비자 (2026-07-14 발견)** | **autotrd (automkm 마켓메이킹/algotrd/autohdg 자동헤지)** 가 libmds link + `market_getfold()` 로 mds SHM 을 직접 read (`mds/WD9500/mds.c:545` shmat 확인), **mat (매칭엔진) 의 `mat/mds` 태스크**가 SHM 5개 세그먼트 (key `0x90200000~0x90240000` — BEST/REU/KMB/SMB/EBS) 직접 open. **Stage 3 신설로 이관** — 드롭인 캐시 라이브러리 (`libwtgfold`) + end-to-end 전파 지연 게이트 |
| arbit (차익 신호) | `mci-price` 의 consumer 로 **Go 포팅** (`ArbitConsumer`) |
| OTP 인증 (W9510) | 매매엔진/외부 인증 시스템으로 **이관** — WTG 는 `/v1/tx` alias passthrough 만 (인증 위임 원칙, `docs/auth.md`) |
| FOS 파일 로더 (WD950010) | `fx-sync` 에 **FOS backend 추가** → etcd 미러 |
| 회색지대 (W9501 종가 = DB read?) | 완전 폐기 결정으로 자동 해소 — **`mci-chart` 가 cover** (PoC 완료) |
| client 전환 방식 | **혼합** — 재빌드 가능 client 는 cside SDK 교체, 불가 client 는 wire 호환 AP (`mci-mds-shim` 신설) |
| trn (매매 AP) 축 | **`mywork/nh` 전수 grep 으로 결합 3축 확정**: ① **tp call 1건** — `W2006A01.pc` 의 `mymq_call("W9504A01")` 2곳 (수동 스왑포인트/마진 등록) → shim 매핑으로 커버. ② **DB 결합 (본체)** — mds 수집계 (WD9500 `rdb.pc`) 가 write 하는 시세 테이블을 업무 코드가 직접 SELECT: `CMG014F` 시장환율인터페이스 17개 파일 (trn W1200/W1400/W2000/W3200/W3510/W3600 + lib WTR005/WCM001), `CMG058M` RFS 6개 (**trn W2602A02/W2610A01 은 write 도** — 공유 테이블), `CMG035L` 5개, `CMG029M` 시세틱 4개 (trn W2103S01 + lib WLM005/006 + bat WB950001), `CMG034L` 체결내역 1개 → Stage 4 가 승계. ③ **trn 자체의 직접 의존 0건** — trn 은 SHM/mds 헤더/라이브러리 link 없음 (yuanta 원형의 W1801/W6109 는 NH 미포팅). 단 SHM 직접 read 는 trn 밖 (autotrd/mat) 에 존재 — 위 행 참조 |
| 기준 소스 | **`/Users/winwaysystems/mywork/nh`** = EC2 `/home/winway/nh-fxallone-server` 미러 (wtg 제외, 2026-07-14 rsync). `nmds`/`yuanta` 트리는 참고용 사본 — 판정은 항상 `mywork/nh` 기준 |
| 전환 전략 | **기능별 스트랭글러** — 모듈 단위 잠식, 단계별 독립 검증·독립 롤백 |

## 2. 전환 전략 — 왜 스트랭글러인가

검토한 3안 중 A안 채택:

- **A안 — 기능별 스트랭글러 (채택)**: 시세 배포 → 조회 → SHM 소비자 이관 →
  DB write → arbit → FOS 순으로 mds 를 모듈 단위 잠식. 각 단계가 독립 검증
  게이트와 독립 롤백 절차를 갖고, 게이트 통과 시점이 곧 중간 보고 지점 —
  제안서 구조와 일치.
- B안 — gap 선완성 후 일괄 dual-run: 절체 이벤트는 1회지만 첫 검증까지
  기간이 길고 절체일에 리스크가 한 점으로 몰림. 중간 성과 보고 불가.
- C안 — client 별 절체: 리스크 최소지만 병행 기간이 무기한으로 늘어질 수
  있고, Oracle write 처럼 client 단위로 쪼갤 수 없는 작업은 결국 A안 형태로
  회귀.

원칙 3가지:

1. **mds 무수정** — 병행 기간 중 mds 쪽 코드는 건드리지 않는다. 롤백 = WTG
   측 정지가 전부가 되도록.
2. **게이트 없이는 다음 단계 없음** — 각 Stage 의 검증 게이트를 수치로 통과해야
   다음 Stage 착수. 게이트 도구 (quote-diff / quote-replay / row-diff) 는
   Stage 0 에서 선확보.
3. **같은 입력, 같은 출력** — 검증은 항상 mds 와 WTG 에 동일 입력 (실 UDP
   feed 또는 pcap 재생) 을 넣고 출력을 기계 비교. 사람 눈 검수는 게이트가
   아니다.

## 3. mds 실물 규모 (공수 산출 근거 — `mywork/nh/mds` 실측)

| 모듈 | 규모 (실측) | 전환 방식 |
|---|---|---|
| 수집계 `WD9500/` (UDP 수신·파싱·BEST·SHM) | 37,546 LOC C/Pro*C 총계 | 기 구현 — `quote-forwarder` + `BestConsumer` (동일 알고리즘, `docs/mds-coverage.md`) |
| DB write (`WD9500/rdb.pc`) | 5,927 LOC Pro*C — 시세 테이블 25개 (틱 029M / 봉 030~033M / 테너별 040~055M 16개 / 체결 034L / 로스트 035L / RFS 058M / 시장환율 014F) | `mci-archive-ora` 신설 — 교차 소비 테이블 우선 (Stage 4 표) |
| 조회계 `W9500/` (**W950x 19 핸들러** — W9500S01~S08 유틸계 + W9501~W9506 조회/설정계) | 7,788 LOC C | cside 교체 (`wtgquery`) + wire 호환 AP (`mci-mds-shim`) 혼합 |
| SHM 소비자 (mds 밖) | autotrd 3 바이너리 (libmds link, `market_getfold` 12곳+) + `mat/mds` 태스크 (SHM 5 세그먼트) | `libwtgfold` 드롭인 교체 (Stage 3) |
| arbit (`WD9500/arbitrage.c`) | 450 LOC C | `internal/price/arbit.go` Go 포팅 |
| FOS loader (`WD950010/`) | 3,603 LOC (파싱부 ~1,000 만 필요) | `fx-sync` FOS backend |
| OTP (`W9510/`) | 소형 (curl 기반) | passthrough alias 만 |
| 리플레이 (`manual_feed/replay_*.c`) | 소형 | `cmd/quote-replay` 신설 (Stage 0) |

## 4. 단계별 계획

각 Stage 는 **목적 / 작업 항목 / 공수 / 검증 게이트 / 롤백** 5칸 표준.

### Stage 0 — 사전 준비 (~2주)

| | |
|---|---|
| **목적** | 이후 모든 단계의 검증 도구와 전환 대상 목록을 선확보 |
| **작업** | ① **client 인벤토리 조사** — W950x 호출 client 전수 파악, 재빌드 가능(cside 트랙)/불가(shim 트랙) 분류. **trn 축은 완료** — EC2 미러 (`mywork/nh/win/src/trn`) 전수 grep 결과 W2006A01 → W9504A01 1건뿐. 잔여 조사 = trn 외 호출자 (딜러 화면/사내 도구 등, NH 협조 필요). ② **`cmd/quote-replay` 신설** — mds `manual_feed/replay_smb2/kmb2/ebs2` 동등의 `.trc` 실 캡처 재생기. `load-gen` (합성) 과 달리 실 캡처를 mds/WTG 양쪽에 동일 재생 가능. **✓ 구현 완료 (2026-07-14)** — `--file/--dest(다중)/--speed/--loop/--dry-run`, 원형과 달리 상대 간격 페이싱이라 아무 시각에나 실행 가능 |
| **공수** | 2주 (조사 0.5 + quote-replay 1 + 리허설 0.5) |
| **게이트** | 동일 pcap 을 mds cooker 에 재생했을 때 기존 동작 재현 확인 (도구 자체 검증) |
| **롤백** | 해당 없음 (운영 무영향) |

### Stage 1 — 시세 배포 dual-run (구현 ~2주 + 병행 4주)

| | |
|---|---|
| **목적** | mds cooker 의 UDP 수신·BEST 산정을 WTG 가 실 트래픽으로 동등 수행함을 입증 |
| **작업** | mds cooker 와 `quote-forwarder`+`mci-price` 가 **동일 UDP feed 를 병렬 수신** (feed 는 broadcast/이중 배선 — mds 무영향). `cmd/quote-diff` 로 양쪽 BEST 출력 상시 비교, `mci-admin` 대시보드에 일치율 노출 |
| **공수** | 2주 (배선·diff 자동화 1 + 관측 대시보드 1) + 병행 관측 4주 |
| **게이트** | ① BEST 일치율 ≥ 99.9% (불일치는 전건 원인 분류 — timing 차이만 허용) ② `/v1/quote/spot` p99 ≤ 2ms 재확인 (`docs/spot-latency-poc.md` 재현) ③ 병행 4주간 WTG 측 drop/재시작 0 ④ **전파 지연 baseline 확보** — UDP 수신 → mci-price BEST 반영까지 p50/p99 실측 (Stage 3 의 SHM 대체 게이트가 이 수치를 기준으로 사용) |
| **롤백** | WTG 측 정지만 — mds 무변경이라 리스크 0 |

### Stage 2 — 조회 서비스 (W950x) 전환 (~3주)

| | |
|---|---|
| **목적** | query-server 의 19개 서비스 호출자를 WTG 백엔드로 이동 |
| **작업** | ① **gap fill**: W9501S02/S03 audit 성 필드 (시초/고저/전일대비/base/fill — Aggregator 봉 영역 연결, ~1주) + FWD pdcd → `/v1/quote/forward-snapshot` 연결 (`internal/price/forward_snapshot.go`) ② **cside 트랙**: 재빌드 가능 client 의 `mymq_call` → `wtg_query_*` stub 교체 (`cside/wtgquery`, PoC 검증 완료) ③ **wire 호환 트랙**: **`mci-mds-shim` 신설** — `pkg/mymq` 로 broker 에 AP 로 등록, W950x 고정폭 전문 수신 → mci-chart/mci-price REST 변환 응답. client 완전 무수정 (~1.5주 — `pkg/svcio` 파서 + wtgquery 매핑 로직 재사용). **조회(S) 계열뿐 아니라 trn 발 설정(A01) 계열 tp call 도 shim 이 수신**: W9502A01 킬스위치 → `pkg/policy`, W9504A01 수동 스왑포인트/마진 → `mci-admin` pricing (etcd → `PricingTable.SwapPoint` hot reload), W9505A01 거래소별 전송 on/off → Profile/policy, W9506A01 BEST 심볼 설정 → `mci-admin` symbols (etcd → `BestConsumer`) ④ **유틸계 W9500S01~S08 매핑** — S08 영업일 조회 (autotrd 가 기동 시 호출) 는 WTG 에 영업일 캘린더 개념이 없으므로 데이터 소스 선결정 필요: `fx-sync` 가 영업일 테이블 (CMG012M 계열) 을 etcd 미러 → shim 이 etcd read (권장) 또는 shim 이 직접 DB read. S01~S07 은 착수 1주차에 실체 확인 후 매핑표 확정 ⑤ OTP W9510 → `/v1/tx` alias 로 매매엔진/외부 인증 라우팅 |
| **공수** | 3주 (gap fill 1 + shim 1.5 + client 교체 지원 0.5) |
| **게이트** | ① 동일 조회를 mds query-server 와 WTG 양쪽에 던져 필드 단위 diff 0 (자동화, pcap 재생 상태에서) ② shim 경유 p99 가 기존 mymq_call 대비 NH SLA (ms 수준) 내 ③ 전환 client 의 운영 오류 0 (1주 관측) |
| **롤백** | client 별 stub 원복 (cside 트랙) / shim 정지 후 mds query-server 재기동 (wire 트랙) — client 단위 부분 롤백 가능 |

### Stage 3 — 자동매매·매칭엔진 SHM 이관 (~3.5주)

| | |
|---|---|
| **목적** | autotrd (automkm/algotrd/autohdg) 와 mat 매칭엔진의 mds SHM 직접 read 를 WTG 시세 스트림으로 이관 — mds 폐기의 마지막 남은 실시간 소비자 |
| **작업** | ① **`cside/wtgfold` (libwtgfold) 신설** — libmds 의 `market_getfold()` 시그니처를 유지하는 드롭인 대체 라이브러리 (외부 의존 0 원칙, `wtgpush`/`wtgprice` 패턴). 백그라운드 수신 스레드가 WTG 시세 스트림을 받아 프로세스 내 MDFOLD 호환 캐시를 갱신 → read 는 기존과 동일한 로컬 메모리 접근 (μs). 전송로는 raw TCP 프레임 (설계 기존안 `docs/edge-tcp-quote-push.md` 의 QUOTE 프레임 재사용 또는 mci-price 직결 TCP — 착수 시 확정. gRPC `SubscribeAlgo` 는 C 소비자에겐 의존성 과대) ② **autotrd 이관** — automkm/algotrd/autohdg 의 `-lmds` → `-lwtgfold` 링크 교체 (호출부 12곳+ 무수정 목표) ③ **mat/mds 태스크 이관** — SHM 5 세그먼트 (BEST/REU/KMB/SMB/EBS) open 을 libwtgfold source 별 캐시로 교체 ④ **end-to-end 전파 지연 하네스** — tick UDP 수신 → 소비자 캐시 가시화까지 p50/p99 측정 (mds SHM 경로와 동일 pcap 으로 비교) |
| **공수** | 3.5주 (libwtgfold 1.5 + autotrd 이관 0.5 + mat 이관 1 + 지연 하네스 0.5) |
| **게이트** | ① 동일 pcap 재생 시 `market_getfold` 반환 필드가 SHM 경로와 diff 0 ② **전파 지연 p99 가 마켓메이킹·매칭 요건 내** — 요건 수치는 착수 1주차에 mds 현행 실측 후 NH 와 합의 ③ automkm 모의 호가 시나리오에서 주문 결정 동일 (재정가 계산 USD/CNH·USD/KRW 경로 포함) |
| **롤백** | 링크를 `-lmds` 로 원복 — mds SHM 은 폐기 (Stage 7) 전까지 계속 유지되므로 즉시 복귀 가능 |

### Stage 4 — Oracle write 승계 (구현 ~3주 + dual-write 2주)

| | |
|---|---|
| **목적** | trn/lib/bat 업무 코드 무수정을 전제로 mds 수집계 (`WD9500/rdb.pc`) 의 Oracle write 를 WTG 가 승계 |
| **작업** | **`mci-archive-ora` 신설** — `PriceService.SubscribeQuote`/`SubscribeBar` gRPC 소비 → 기존 스키마 (`FXPL.TB_FXB_CMG*`) 그대로 write. driver 는 **go-ora (pure Go)** — cgo·Oracle client 불필요, 폐쇄망 배포 단순. 기존 `Archiver` (TimescaleDB, pgx batch) 구조를 골격으로 재사용. **교차 소비 테이블 우선순위로 3-tier 분할**: |
| **1-tier (거래 경로)** | `CMG014F` 시장환율인터페이스 — trn 거래/한도/평가 로직 17개 파일이 현재 환율 스냅샷으로 참조. 성격이 이력 INSERT 가 아니라 **현재가 UPDATE (저지연 요건)** — BEST tick 발생 시 즉시 UPDATE. 갱신 지연 SLA 를 mds 현행과 비교 측정 필수 |
| **2-tier (업무 read)** | `CMG029M` 시세틱 (trn W2103S01, lib WLM005/006, bat WB950001) · `CMG034L` 체결내역 (W3530S02) · `CMG035L` (5개 파일) — 이력 INSERT, batch 재현 |
| **3-tier (mds 내부/배치)** | 봉 계열 030~033M + 테너별 040~055M 16개 — 외부 소비는 bat WB950001 정도. 스키마 재현하되 dual-write 검증 우선순위 하위 |
| **별도 조사** | `CMG058M` RFS — **trn W2602A02/W2610A01 도 write 하는 공유 테이블**. 행/컬럼 소유권 분석 후 mds 몫만 승계할지, trn 존치로 둘지 판정 (Stage 4 착수 1주차) |
| **공수** | 3주 (1-tier 0.5 + 2-tier 1 + 3-tier 1 + 재접속·batch·backpressure 0.5) + dual-write 검증 2주 |
| **게이트** | ① mds 와 WTG **병행 write** (WTG 는 검증용 테이블 접미사) → row-level diff 배치 일 단위 비교, 불일치 0 (commit timing ±1 row 는 원인 분류 후 판정) ② 1-tier `CMG014F` 는 갱신 지연 p99 가 mds 현행 동등 이내 |
| **롤백** | WTG write 정지 + 검증 테이블 drop — 운영 테이블은 mds 가 계속 write 중이므로 무영향. **절체 (mds write 정지) 는 게이트 통과 후 별도 승인** |

### Stage 5 — arbit 포팅 (~1.5주)

| | |
|---|---|
| **목적** | 차익 신호 계산을 mci-price consumer 로 이식 |
| **작업** | `internal/price/arbit.go` — `ArbitConsumer` (`OnTick` 규약, `crossrate_consumer.go` 가 구조 발판). `WD9500/arbitrage.c` 450 LOC 이식 + 신호 출력은 기존 fan-out 경로 (broker publish 또는 gRPC) 재사용 |
| **공수** | 1.5주 (이식 1 + 검증 0.5) |
| **게이트** | 동일 pcap 재생 시 mds `arbit_worker` 와 신호 diff 0 |
| **롤백** | consumer 등록 해제 flag — mds arbit 는 폐기 전까지 계속 기동 상태 |

### Stage 6 — FOS → fx-sync (~1주)

| | |
|---|---|
| **목적** | 마스터 카탈로그 로딩을 etcd 미러로 일원화 |
| **작업** | `internal/fxsync` 에 FOS 파일 포맷 backend 추가 (기존 `Backend` 인터페이스 — `file_backend.go` 패턴) → etcd write 는 기존 `Syncer` 그대로 |
| **공수** | 1주 |
| **게이트** | 동일 FOS 파일 입력 시 mds WD950010 의 DB 적재 결과와 etcd 미러 항목 수·값 일치 |
| **롤백** | backend flag 원복 |

### Stage 7 — mds 폐기 (관측 2주 + 정지)

| | |
|---|---|
| **목적** | 안전 확인 후 mds 정지·아카이브 |
| **작업** | ① broker 측 W950x 트래픽 0 확인 (2주 관측) ② cooker UDP 소켓 유휴 확인 ③ **SHM 세그먼트 (0x902xxxxx) attach 프로세스 0 확인** (`ipcs -m` — autotrd/mat 이관 완료 검증) ④ Stage 4 절체 승인 후 mds write 정지 ⑤ mds 바이너리 + `libmds` 정지 ⑥ 소스·설정·최종 DB 스냅샷 아카이브 |
| **공수** | 0.5주 (관측 2주 병행) |
| **게이트** | 정지 후 1주간 하단 소비 시스템·client 이상 보고 0 |
| **롤백** | 아카이브에서 mds 재기동 (정지일로부터 4주간 즉시 재기동 가능 상태 유지) |

### 범위 밖 — 별도 신규 기능 트랙 (mds 대치 아님)

yuanta trn 에는 있으나 **NH 에는 해당 trn 서비스 (W1801U01/W6109A02) 자체가
미포팅**이라 현재 수요 미확정인 2건. NH 포팅에서 이 서비스들이 살아나면 mds 가
아니라 WTG 로 직접 붙인다 — 본 계획의 공수·게이트에서 제외, 수요 확인 후 착수:

| 기능 | yuanta 원형 | WTG 수용 방안 | 공수 |
|---|---|---|---|
| 수동 시세 주입 | `mds_make_custsise` — 수동(HAND) 시세를 UDP 로 feed 처럼 주입 (trn W1801U01) | `PriceService.PublishTick` gRPC 에 `Source="HAND"` tick — 주입용 소형 REST/cside API 또는 `mci-admin` 수동시세 페이지 신설 | ~2-3일 |
| RFQ 응답 → FIX 송신 | `mds_send_rfq` — 딜러 RFQ 환율 (bid/ask/near/far) UDP multicast → FIX 송신 (trn W6109A02) | `mci-edge-fix-md` 에 Quote(35=S) 트랙 신설. near/far = swap 2-leg — `docs/swap-trade-spec.md` Phase S3 연동 | ~1-2주 |

## 5. 공수 총괄 / 타임라인

| Stage | 구현 공수 | 병행 관측 |
|---|---|---|
| 0 사전 준비 | 2주 | — |
| 1 시세 dual-run | 2주 | 4주 |
| 2 조회 전환 | 3주 | 1주 |
| 3 SHM 이관 (autotrd/mat) | 3.5주 | (pcap + 지연 비교) |
| 4 Oracle 승계 | 3주 | 2주 |
| 5 arbit | 1.5주 | (pcap 검증) |
| 6 FOS | 1주 | — |
| 7 폐기 | 0.5주 | 2주 + 유예 4주 |
| **합계** | **~16.5주** | 관측은 다음 Stage 구현과 병행 |

관측 기간을 후속 구현과 겹치면 **달력 기준 약 5개월**. Stage 1·2·3 게이트
통과가 각각 중간 보고 지점 — "이미 실 트래픽으로 동등성이 수치 입증됨" 을
들고 다음 단계 승인을 받는 구조.

## 6. 리스크와 대응

| 리스크 | 대응 |
|---|---|
| client 인벤토리 누락 (미파악 W950x 호출자) | Stage 7 관측 2주에서 broker 트래픽 0 을 게이트로 — 누락 호출자는 여기서 반드시 드러남. 발견 시 shim 트랙으로 흡수 (client 무수정이므로 추가 공수 미미). trn 호출자는 `wfrx` 트리 grep (Stage 0) 으로 선제 파악 |
| **SHM→스트림 전파 지연** | mds SHM 은 전파 지연 0 (cooker 가 같은 메모리에 write), WTG 는 UDP→forwarder→mci-price→TCP 스트림→로컬 캐시 hop 이 존재. 마켓메이킹·매칭이 이 지연을 수용 못 하면 Stage 3 실패 — **착수 1주차에 mds 현행 tick→소비 지연을 실측해 요건 수치를 먼저 확정**하고, 미달 시 mci-price 를 소비자와 동일 호스트에 배치 (loopback/UDS) 하는 축소 hop 배치안 적용 |
| autotrd/mat 코드 수정 협조 | Stage 3 은 유일하게 **소비자 측 재빌드가 필수** (링크 교체) — libwtgfold 가 `market_getfold` 시그니처를 유지해 수정 폭을 링크 옵션으로 최소화하고, 롤백은 `-lmds` 원복으로 즉시 가능함을 협조 요청의 근거로 제시 |
| go-ora 의 NH Oracle 버전 호환 | Stage 4 착수 1일차에 대상 Oracle (19c — mds 빌드 옵션 기준) 스모크 테스트 선행. 실패 시 godror (cgo) 로 전환 — 공수 +0.5주, 폐쇄망 배포 절차에 Oracle client 라이브러리 추가 |
| `CMG058M` 공유 write 충돌 | mds 와 trn (W2602A02/W2610A01) 이 같은 테이블에 write — WTG 승계 시 trn write 와 경합/덮어쓰기 위험. Stage 4 1주차에 행/컬럼 소유권 분석을 선행하고, 소유 구분이 불가하면 058M 은 mds 폐기 시점까지 mds 잔존 write 로 남기는 fallback |
| `CMG014F` 갱신 지연 | trn 거래 로직이 읽는 현재 환율 스냅샷 — WTG 경유 (UDP→forwarder→mci-price→archive-ora→Oracle) 가 mds 직접 write 보다 hop 이 많음. Stage 4 게이트에 갱신 지연 p99 비교를 명시 (미달 시 mci-price 에서 직접 UPDATE 하는 우회 경로) |
| BEST timing 차이로 일치율 미달 | mds 와 WTG 의 tick 처리 순서가 완전 동일할 수 없음 — 불일치 전건을 "값 오류 / timing 차이" 로 분류하는 diff 규칙을 Stage 1 첫 주에 확정. 값 오류만 게이트 대상 |
| audit 필드 의미 불명 (mds 만 아는 파생값) | W9501S02 audit 필드 채움 시 mds 소스 (`W9501S02.c`) 를 필드 단위 대조 — 산식이 문서에 없으므로 소스가 명세 |
| shim 이 새 단일 장애점 | shim 은 stateless (broker AP + REST 변환) — 다중 인스턴스 기동, broker 가 AP 분배. 재시작 = 무손실 |

## 7. 참조

- `docs/mds-coverage.md` — cover 매트릭스 + W9501 PoC + latency PoC (판정 근거)
- `docs/spot-latency-poc.md` — 20k RPS p99 ≤ 2ms 측정
- `cside/wtgquery/` — W9501 wire adapter (Stage 2 cside 트랙의 실물)
- `internal/price/best.go` — `BestConsumer` (mds `mdssise_make_best` 동일 알고리즘)
- `internal/price/archiver.go` / `archiver_pgx.go` — Stage 4 `mci-archive-ora` 의 골격
- `internal/price/crossrate_consumer.go` — Stage 5 `ArbitConsumer` 의 구조 발판
- `internal/fxsync/backend.go` — Stage 6 FOS backend 의 인터페이스
- `docs/edge-tcp-quote-push.md` — raw TCP QUOTE 프레임 설계 (Stage 3 libwtgfold 전송로 후보)
- `cside/wtgpush/` `cside/wtgprice/` — 외부 의존 0 C SDK 패턴 (Stage 3 libwtgfold 의 선례)
- `/Users/winwaysystems/mywork/nh/autotrd/` — SHM 소비자 1: automkm/algotrd/autohdg (`-lmds` link, `market_getfold` 사용처는 automkm.c 등 12곳+)
- `/Users/winwaysystems/mywork/nh/mat/mds/main.c` — SHM 소비자 2: 매칭엔진 브리지 (`MdsMem[]` key 0x90200000~0x90240000)
- `/Users/winwaysystems/mywork/nh/mds/WD9500/mds.c:545` — libmds SHM attach 실체 (reader/writer 모드)
- `/Users/winwaysystems/mywork/nh/` — **기준 소스** (EC2 `/home/winway/nh-fxallone-server` 미러, wtg 제외)
- `/Users/winwaysystems/mywork/nh/win/src/trn/W2000/W2006A01.pc:702,1131` — trn → mds tp call 전부 (`mymq_call "W9504A01"`)
- `/Users/winwaysystems/mywork/nh/mds/` — NH mds 실물 (W9500 조회계 / WD9500 수집계 / W9510 OTP / WD950010 FOS / manual_feed 리플레이)
- `/Users/winwaysystems/mywork/nh/mds/WD9500/rdb.pc` — Oracle write 전부 (시세 테이블 25개) — Stage 4 의 이식 원본
- `/Users/winwaysystems/mywork/nh/table.sql` — `FXPL.TB_FXB_CMG*` DDL + 한글 테이블 정의 (CP949)
- 교차 소비 지도 (2026-07-14 grep): 014F←17파일 / 058M←6(+write 2) / 035L←5 / 029M←4 / 034L←1 / 040M←1 (bat WB950001)
- `/Users/winwaysystems/mywork/nmds/mds/docs/06-아키텍처.md` — mds query-server 서비스 카탈로그 (참고 사본)
- `/Users/winwaysystems/mywork/yuanta/win/src/trn/` — yuanta 원형의 mds 직접 호출 3곳 (W1801A01/W1801U01/W6109A02) — NH 미포팅, 범위 밖 트랙의 원형 참고용
