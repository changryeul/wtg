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
| DB write (Oracle) | 하단 소비 시스템 (보고서/정산) 이 mds Oracle 테이블을 그대로 읽으므로 **WTG 가 동일 스키마로 Oracle write 를 승계** — `mci-archive-ora` 신설 |
| arbit (차익 신호) | `mci-price` 의 consumer 로 **Go 포팅** (`ArbitConsumer`) |
| OTP 인증 (W9510) | 매매엔진/외부 인증 시스템으로 **이관** — WTG 는 `/v1/tx` alias passthrough 만 (인증 위임 원칙, `docs/auth.md`) |
| FOS 파일 로더 (WD950010) | `fx-sync` 에 **FOS backend 추가** → etcd 미러 |
| 회색지대 (W9501 종가 = DB read?) | 완전 폐기 결정으로 자동 해소 — **`mci-chart` 가 cover** (PoC 완료) |
| client 전환 방식 | **혼합** — 재빌드 가능 client 는 cside SDK 교체, 불가 client 는 wire 호환 AP (`mci-mds-shim` 신설) |
| trn (매매 AP) 축 | **EC2 기준 소스 전수 grep 으로 확정**: trn → mds 의존은 `W2006A01.pc` 의 `mymq_call(..., "W9504A01", ...)` **단 1건** (호출 지점 2곳 — 수동 스왑포인트/마진 등록). yuanta 식 SHM/UDP 직접 호출 (`mds_updt_custfeed`/`mds_make_custsise`/`mds_send_rfq`) 은 NH trn 에 해당 서비스 (W1801/W6109) 자체가 미포팅 — 0건. 따라서 **shim 이 W9504A01 하나만 매핑하면 trn 축은 무수정 커버** |
| 기준 소스 | **`/Users/winwaysystems/mywork/nh`** = EC2 `/home/winway/nh-fxallone-server` 미러 (wtg 제외, 2026-07-14 rsync). `nmds`/`yuanta` 트리는 참고용 사본 — 판정은 항상 `mywork/nh` 기준 |
| 전환 전략 | **기능별 스트랭글러** — 모듈 단위 잠식, 단계별 독립 검증·독립 롤백 |

## 2. 전환 전략 — 왜 스트랭글러인가

검토한 3안 중 A안 채택:

- **A안 — 기능별 스트랭글러 (채택)**: 시세 배포 → 조회 → DB write → arbit →
  FOS 순으로 mds 를 모듈 단위 잠식. 각 단계가 독립 검증 게이트와 독립 롤백
  절차를 갖고, 게이트 통과 시점이 곧 중간 보고 지점 — 제안서 구조와 일치.
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

## 3. mds 실물 규모 (공수 산출 근거)

| 모듈 | 규모 (실측) | 전환 방식 |
|---|---|---|
| cooker UDP 수신·파싱·BEST | ~28,000 LOC C (stub 포함) | 기 구현 — `quote-forwarder` + `BestConsumer` (동일 알고리즘, `docs/mds-coverage.md`) |
| DB write (`cooker/db/mds_rdb*.pc`) | 11,921 LOC Pro*C — tick(2,057)/intr(1,958)/heod(1,451)/fill(253)/master(275) + 공통(5,927) | `mci-archive-ora` 신설. 실질 작업은 5개 도메인 INSERT 재현 — Pro*C 공통 플러밍은 Go 에선 불필요 |
| query-server (W950x 9 핸들러) | 2,767 LOC C | cside 교체 (`wtgquery`) + wire 호환 AP (`mci-mds-shim`) 혼합 |
| arbit (`cooker/parser/mds_arbit.c`) | 650 LOC C | `internal/price/arbit.go` Go 포팅 |
| FOS loader (`fos-loader/`) | ~3,500 LOC (파싱부 ~1,000 만 필요) | `fx-sync` FOS backend |
| OTP (`otp-auth/W9510.c`) | 소형 (curl 기반) | passthrough alias 만 |

## 4. 단계별 계획

각 Stage 는 **목적 / 작업 항목 / 공수 / 검증 게이트 / 롤백** 5칸 표준.

### Stage 0 — 사전 준비 (~2주)

| | |
|---|---|
| **목적** | 이후 모든 단계의 검증 도구와 전환 대상 목록을 선확보 |
| **작업** | ① **client 인벤토리 조사** — W950x 호출 client 전수 파악, 재빌드 가능(cside 트랙)/불가(shim 트랙) 분류. **trn 축은 완료** — EC2 미러 (`mywork/nh/win/src/trn`) 전수 grep 결과 W2006A01 → W9504A01 1건뿐. 잔여 조사 = trn 외 호출자 (딜러 화면/사내 도구 등, NH 협조 필요). ② **`cmd/quote-replay` 신설** — mds `manual_feed/replay_smb2/kmb2/ebs2` 동등의 pcap 재생기. `load-gen` (합성) 과 달리 실 캡처를 mds/WTG 양쪽에 동일 재생 가능 |
| **공수** | 2주 (조사 0.5 + quote-replay 1 + 리허설 0.5) |
| **게이트** | 동일 pcap 을 mds cooker 에 재생했을 때 기존 동작 재현 확인 (도구 자체 검증) |
| **롤백** | 해당 없음 (운영 무영향) |

### Stage 1 — 시세 배포 dual-run (구현 ~2주 + 병행 4주)

| | |
|---|---|
| **목적** | mds cooker 의 UDP 수신·BEST 산정을 WTG 가 실 트래픽으로 동등 수행함을 입증 |
| **작업** | mds cooker 와 `quote-forwarder`+`mci-price` 가 **동일 UDP feed 를 병렬 수신** (feed 는 broadcast/이중 배선 — mds 무영향). `cmd/quote-diff` 로 양쪽 BEST 출력 상시 비교, `mci-admin` 대시보드에 일치율 노출 |
| **공수** | 2주 (배선·diff 자동화 1 + 관측 대시보드 1) + 병행 관측 4주 |
| **게이트** | ① BEST 일치율 ≥ 99.9% (불일치는 전건 원인 분류 — timing 차이만 허용) ② `/v1/quote/spot` p99 ≤ 2ms 재확인 (`docs/spot-latency-poc.md` 재현) ③ 병행 4주간 WTG 측 drop/재시작 0 |
| **롤백** | WTG 측 정지만 — mds 무변경이라 리스크 0 |

### Stage 2 — 조회 서비스 (W950x) 전환 (~3주)

| | |
|---|---|
| **목적** | query-server 의 9개 서비스 호출자를 WTG 백엔드로 이동 |
| **작업** | ① **gap fill**: W9501S02/S03 audit 성 필드 (시초/고저/전일대비/base/fill — Aggregator 봉 영역 연결, ~1주) + FWD pdcd → `/v1/quote/forward-snapshot` 연결 (`internal/price/forward_snapshot.go`) ② **cside 트랙**: 재빌드 가능 client 의 `mymq_call` → `wtg_query_*` stub 교체 (`cside/wtgquery`, PoC 검증 완료) ③ **wire 호환 트랙**: **`mci-mds-shim` 신설** — `pkg/mymq` 로 broker 에 AP 로 등록, W950x 고정폭 전문 수신 → mci-chart/mci-price REST 변환 응답. client 완전 무수정 (~1.5주 — `pkg/svcio` 파서 + wtgquery 매핑 로직 재사용). **조회(S) 계열뿐 아니라 trn 발 설정(A01) 계열 tp call 도 shim 이 수신**: W9502A01 킬스위치 → `pkg/policy`, W9504A01 수동 스왑포인트/마진 → `mci-admin` pricing (etcd → `PricingTable.SwapPoint` hot reload), W9505A01 거래소별 전송 on/off → Profile/policy, W9506A01 BEST 심볼 설정 → `mci-admin` symbols (etcd → `BestConsumer`) ④ OTP W9510 → `/v1/tx` alias 로 매매엔진/외부 인증 라우팅 |
| **공수** | 3주 (gap fill 1 + shim 1.5 + client 교체 지원 0.5) |
| **게이트** | ① 동일 조회를 mds query-server 와 WTG 양쪽에 던져 필드 단위 diff 0 (자동화, pcap 재생 상태에서) ② shim 경유 p99 가 기존 mymq_call 대비 NH SLA (ms 수준) 내 ③ 전환 client 의 운영 오류 0 (1주 관측) |
| **롤백** | client 별 stub 원복 (cside 트랙) / shim 정지 후 mds query-server 재기동 (wire 트랙) — client 단위 부분 롤백 가능 |

### Stage 3 — Oracle write 승계 (구현 ~3주 + dual-write 2주)

| | |
|---|---|
| **목적** | 하단 소비 시스템 무수정을 전제로 mds 의 Oracle write 를 WTG 가 승계 |
| **작업** | **`mci-archive-ora` 신설** — `PriceService.SubscribeQuote`/`SubscribeBar` gRPC 소비 → 기존 mds 스키마 그대로 Oracle INSERT (tick/intr/heod/fill/master 5개 도메인). driver 는 **go-ora (pure Go)** — cgo·Oracle client 라이브러리 불필요, 폐쇄망 배포 단순. 기존 `Archiver` (TimescaleDB, pgx batch) 구조를 골격으로 재사용 |
| **공수** | 3주 (5개 도메인 INSERT 2 + 재접속·batch·backpressure 1) + dual-write 검증 2주 |
| **게이트** | mds 와 WTG 가 **병행 write** (WTG 는 검증용 테이블 접미사) → row-level diff 배치가 일 단위 비교, 불일치 0 (허용 오차: commit timing 에 따른 ±1 row 는 원인 분류 후 판정) |
| **롤백** | WTG write 정지 + 검증 테이블 drop — 운영 테이블은 mds 가 계속 write 중이므로 무영향. **절체 (mds write 정지) 는 게이트 통과 후 별도 승인** |

### Stage 4 — arbit 포팅 (~1.5주)

| | |
|---|---|
| **목적** | 차익 신호 계산을 mci-price consumer 로 이식 |
| **작업** | `internal/price/arbit.go` — `ArbitConsumer` (`OnTick` 규약, `crossrate_consumer.go` 가 구조 발판). `mds_arbit.c` 650 LOC 이식 + 신호 출력은 기존 fan-out 경로 (broker publish 또는 gRPC) 재사용 |
| **공수** | 1.5주 (이식 1 + 검증 0.5) |
| **게이트** | 동일 pcap 재생 시 mds `arbit_worker` 와 신호 diff 0 |
| **롤백** | consumer 등록 해제 flag — mds arbit 는 폐기 전까지 계속 기동 상태 |

### Stage 5 — FOS → fx-sync (~1주)

| | |
|---|---|
| **목적** | 마스터 카탈로그 로딩을 etcd 미러로 일원화 |
| **작업** | `internal/fxsync` 에 FOS 파일 포맷 backend 추가 (기존 `Backend` 인터페이스 — `file_backend.go` 패턴) → etcd write 는 기존 `Syncer` 그대로 |
| **공수** | 1주 |
| **게이트** | 동일 FOS 파일 입력 시 mds WD950010 의 DB 적재 결과와 etcd 미러 항목 수·값 일치 |
| **롤백** | backend flag 원복 |

### Stage 6 — mds 폐기 (관측 2주 + 정지)

| | |
|---|---|
| **목적** | 안전 확인 후 mds 정지·아카이브 |
| **작업** | ① broker 측 W950x 트래픽 0 확인 (2주 관측) ② cooker UDP 소켓 유휴 확인 ③ Stage 3 절체 승인 후 mds write 정지 ④ mds 4개 바이너리 + `libmds.so` 정지 ⑤ 소스·설정·최종 DB 스냅샷 아카이브 |
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
| 3 Oracle 승계 | 3주 | 2주 |
| 4 arbit | 1.5주 | (pcap 검증) |
| 5 FOS | 1주 | — |
| 6 폐기 | 0.5주 | 2주 + 유예 4주 |
| **합계** | **~13주** | 관측은 다음 Stage 구현과 병행 |

관측 기간을 후속 구현과 겹치면 **달력 기준 약 4개월**. Stage 1·2 게이트
통과가 각각 중간 보고 지점 — "이미 실 트래픽으로 동등성이 수치 입증됨" 을
들고 다음 단계 승인을 받는 구조.

## 6. 리스크와 대응

| 리스크 | 대응 |
|---|---|
| client 인벤토리 누락 (미파악 W950x 호출자) | Stage 6 관측 2주에서 broker 트래픽 0 을 게이트로 — 누락 호출자는 여기서 반드시 드러남. 발견 시 shim 트랙으로 흡수 (client 무수정이므로 추가 공수 미미). trn 호출자는 `wfrx` 트리 grep (Stage 0) 으로 선제 파악 |
| go-ora 의 NH Oracle 버전 호환 | Stage 3 착수 1일차에 대상 Oracle 버전 스모크 테스트 선행. 실패 시 godror (cgo) 로 전환 — 공수 +0.5주, 폐쇄망 배포 절차에 Oracle client 라이브러리 추가 |
| BEST timing 차이로 일치율 미달 | mds 와 WTG 의 tick 처리 순서가 완전 동일할 수 없음 — 불일치 전건을 "값 오류 / timing 차이" 로 분류하는 diff 규칙을 Stage 1 첫 주에 확정. 값 오류만 게이트 대상 |
| audit 필드 의미 불명 (mds 만 아는 파생값) | W9501S02 audit 필드 채움 시 mds 소스 (`W9501S02.c`) 를 필드 단위 대조 — 산식이 문서에 없으므로 소스가 명세 |
| shim 이 새 단일 장애점 | shim 은 stateless (broker AP + REST 변환) — 다중 인스턴스 기동, broker 가 AP 분배. 재시작 = 무손실 |

## 7. 참조

- `docs/mds-coverage.md` — cover 매트릭스 + W9501 PoC + latency PoC (판정 근거)
- `docs/spot-latency-poc.md` — 20k RPS p99 ≤ 2ms 측정
- `cside/wtgquery/` — W9501 wire adapter (Stage 2 cside 트랙의 실물)
- `internal/price/best.go` — `BestConsumer` (mds `mdssise_make_best` 동일 알고리즘)
- `internal/price/archiver.go` / `archiver_pgx.go` — Stage 3 `mci-archive-ora` 의 골격
- `internal/price/crossrate_consumer.go` — Stage 4 `ArbitConsumer` 의 구조 발판
- `internal/fxsync/backend.go` — Stage 5 FOS backend 의 인터페이스
- `/Users/winwaysystems/mywork/nh/` — **기준 소스** (EC2 `/home/winway/nh-fxallone-server` 미러, wtg 제외)
- `/Users/winwaysystems/mywork/nh/win/src/trn/W2000/W2006A01.pc:702,1131` — trn → mds tp call 전부 (`mymq_call "W9504A01"`)
- `/Users/winwaysystems/mywork/nh/mds/` — NH mds 실물 (W9500/WD9500/W9510/WD950010/manual_feed)
- `/Users/winwaysystems/mywork/nmds/mds/docs/06-아키텍처.md` — mds query-server 서비스 카탈로그 (참고 사본)
- `/Users/winwaysystems/mywork/yuanta/win/src/trn/` — yuanta 원형의 mds 직접 호출 3곳 (W1801A01/W1801U01/W6109A02) — NH 미포팅, 범위 밖 트랙의 원형 참고용
