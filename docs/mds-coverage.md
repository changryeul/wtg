# mds → WTG cover 매트릭스

NH은행 FX 시장 데이터 시스템 (mds, `/Users/winwaysystems/mywork/nmds/mds`) 의
운영 기능 중 **WTG 의 시세 배포 엔진 (forwarder + mci-price + mci-edge-price
+ mci-push + mci-chart + mci-admin) 으로 cover 가능한 범위** 를 한 장으로
정리한 매트릭스. 운영자·아키텍트가 마이그레이션 의사결정을 1주 안에 내릴
수 있도록 합치 / 제외 / 회색지대를 분명히 표시한다.

## 범위 (in/out)

다음 4개는 **명시적 비범위** — WTG 가 cover 안 함:

| 비범위 | 이유 | 잔여 처리 주체 |
|---|---|---|
| **DB write/read 처리** | mds 의 Oracle/SQLite write (sync_queue / mds_rdb_*.pc) 는 WTG 의 책임이 아님. 운영 SOR 는 mds 또는 후속 시스템에 남김. | mds 그대로 또는 신규 SOR |
| **arbit (차익) 계산** | mds `arbit_worker` + `mds_arbit.h` 의 차익 거래 신호 로직. WTG `CrossRateConsumer` 는 단순 cross-rate 만 — 1:1 동등 아님. | mds 그대로 |
| **OTP 인증 (W9510 otp-auth)** | NH 특화 인증 보조. WTG 의 인증 위임 원칙상 (auth/MFA 는 매매 엔진 / 외부 시스템) cover 안 함. | mds 또는 NH 인증 시스템 |
| **FOS 파일 로더 (WD950010)** | DB 카탈로그 로딩 — DB 처리에 포함. WTG 의 마스터 데이터 동기화는 `fx-sync` (etcd 미러) 가 대체하지만 FOS 파일 포맷 자체 cover 는 아님. | mds 또는 `cmd/fx-sync` 확장 |

위 4개를 제외한 모든 mds 운영 기능이 **WTG 의 기존 컴포넌트로 cover 가능**.

## cover 매트릭스 — 모듈 별

### 1. `cooker` (`WD9500`) — UDP 수신·SHM·BEST·차익·DB

| mds 기능 | WTG 대응 | 위치 | Cover |
|---|---|---|---|
| UDP FIX 4.4 수신·파싱 (SMB/KMB/EBS/Reuters) | `quote-forwarder` (per-feed reader/worker 분리 + batch publish) | `cmd/quote-forwarder/main.go` | ✓ |
| SHM 시세 캐시 (`mdquot_update_bidask`, lock-free) | `mci-price` conflation 캐시 + `BestConsumer.cache` (per Symbol×Source) | `internal/price/best.go`, `internal/price/server.go` | ✓ (모델만 SHM → gRPC/HTTP) |
| 다중시장 BEST 산정 (`mdssise_make_best`) | `BestConsumer` — max(bid)/min(ask) + cross fallback (mds 와 동일 알고리즘) | `internal/price/best.go` | ✓ |
| `update_cached_time` (g_cached_time syscall 절감) | `mci-price` 의 `time.Now()` 직접 호출 — Go runtime 이 vDSO 사용해 동등 성능 | — | ✓ (암묵적) |
| 객체 풀 (`pool_alloc`/`pool_free` — `MDCANDLE`) | Go GC + `sync.Pool` (필요 시) — 현재 hot path 는 alloc 없음 | `internal/price/server.go` | ✓ |
| arbit 계산 (`arbit_worker` 스레드) | — | — | **제외** |
| DB write (`worker_save_tick/_intr/_swap/_fill`, mutex 4개) | — | — | **제외** |

### 2. `query-server` (`W9500` + 9 service handler)

| Service | 역할 (docs/06-아키텍처.md) | WTG 대응 | Cover |
|---|---|---|---|
| `W9501S01~S03` | 종가/스왑 조회 | `mci-chart` REST `/v1/chart` (historical 봉) + `mci-price` `/v1/quote/forward-snapshot` (swap point) | △ (종가는 회색지대 — 아래 참조) |
| `W9502S01/A01` | 킬스위치 조회/설정 | `pkg/policy` (etcd watch) + `mci-admin` 의 kill switch UI | ✓ |
| `W9503A01` | 알림 전송 | `mci-push` 의 broker rep receiver 트랙 또는 HTTP push endpoint (Phase 2.x) | ✓ |
| `W9504A01` | 시장 설정 | `mci-admin` 의 symbols/pricing/profiles CRUD (etcd 즉시 반영) | ✓ |
| `W9505S01/A01` | 클라이언트 전송 설정 | Profile (Channel/Site/Tier) + `mci-admin` profiles 페이지 | ✓ |
| `W9506S01/A01` | BEST 시세 설정 | `BestConsumer` config (`MaxStaleness` 등 flag) + `mci-admin` 에서 운영 가시화 (`/v1/best-stats`) | ✓ |

### 3. `replay-tools` (UDP 캡처 리플레이)

| 도구 | 역할 | WTG 대응 | Cover |
|---|---|---|---|
| `replay_smb2` / `replay_kmb2` / `replay_ebs2` | pcap 또는 raw 캡처 재생 (UDP 송신) | `load-gen` (합성 부하만 — `scripts/load-test.sh` 의 low/mid/high 시나리오) | △ (도구 추가 ~1주) |

→ pcap 그대로 재생이 필요하면 `cmd/quote-replay` 신설. 합성 시나리오만으로 충분하면 `load-gen` 그대로.

## 회색 지대 — `W9501` 종가 조회

"DB 처리 제외" 의 정의에 따라 처리 방식이 갈린다:

- **종가 (일자별 close)** 는 본질적으로 historical data read = DB. WTG 가
  cover 하려면 `mci-chart` 의 TimescaleDB SELECT (`/v1/chart`) 가 그 역할.
  이게 "DB 처리" 에 해당하면 제외, mds 의 W9501S01 그대로 유지.
- **스왑 포인트 (swap_point)** 는 `pricing.PricingTable.SwapPoint` (메모리,
  etcd 동기화) 의 read — DB 무관, WTG `/v1/quote/forward-snapshot` 가 그대로
  cover.

운영자 결정 필요: **"DB 처리 제외" = (a) mds 내부 Oracle/SQLite write 만**
인지 **(b) historical read 도 포함** 인지. (a) 라면 종가 조회도 WTG cover.

## 핵심 차이 / 운영 리스크

### 1. SHM → gRPC/HTTP 모델 변환 (latency)

mds 는 lock-free SHM 으로 `query-server` 가 `cooker` 와 **같은 머신에서
직접 메모리 read** — μs 수준. WTG 는:

- **in-process** — 단일 `mci-price` 안에서 conflation 캐시 직접 read (μs)
- **inter-process** — `mci-edge-price` 분리 → gRPC `SubscribeQuote` stream
  또는 REST `/v1/quote/spot` (ms 수준)

같은 호스트 inter-process 가 latency 요건이면 `mci-price` + `mci-edge-price`
를 동일 호스트에서 Unix domain socket 또는 loopback 으로 묶는 배치가 필요.
부하 테스트로 SLA 검증 필요 (`scripts/load-test.sh` + p99 측정).

### 2. wire 호환성 — W9500 시리즈가 MyMQ 위

`W9500.h` 의 service handler 들은 `MyMQ* mymq` 인자를 받는 broker-mediated
TCP 호출이다. 기존 NH 사내 client 가 이 wire 를 그대로 호출 중이라면 WTG
endpoint 단순 교체로는 안 됨 — **adapter 필요**:

- WTG `/v1/tx` alias 시스템 (`mci-api`) 을 경유해 mds-W9501 → mci-chart 라우팅
- 또는 cside C SDK (`cside/wtgquery` 신설, `cside/wtgprice` 패턴 그대로)
  로 기존 client 의 stub 만 교체

→ adapter 1개 (예: W9501S01 종가 조회) PoC 1~2일 권장.

### 3. 운영 마이그레이션 단위

- mds: 4개 독립 바이너리 + `libmds.so` (단일 머신)
- WTG: `mci-api` / `mci-push` / `mci-price` / `mci-chart` / `mci-admin`
  + `mci-edge-*` 4개 + `quote-forwarder` (~10개 바이너리)

배포 단위는 늘어나지만 `mci-admin` UI 가 운영 복잡도를 흡수
(`docs/operations/admin-ui-manual.md` 참조).

## 남는 실제 작업

| 작업 | 목적 | 상태 |
|---|---|---|
| W9501S01 / S02 / S03 wire adapter PoC | NH 사내 client 가 binary 만 교체하면 transparent — adapter 패턴 검증 | ✓ 완료 (`cside/wtgquery/` + `make test-wtgquery`) |
| FWD pdcd / W9501S02 의 audit 필드 (시초/전일대비/base/fill 등) 채움 | 현재 PoC 가 핵심 필드 (bid/ask/best) 만 — audit 성 필드 채우려면 forward-snapshot + 봉 영역 연결 | ~1주 |
| `cmd/quote-replay` 신설 (pcap 재생) | mds `replay_smb2` 와 동등 — 회귀 / 사후분석 | ~1주 |
| SHM → gRPC p99 측정 | 같은 호스트 단일 mci-price vs 분리된 edge — latency 비교 | 반나절 |
| 마스터 데이터 (FOS 외) → `fx-sync` 확장 | NH 마스터 카탈로그를 etcd 미러 — FOS 파일 포맷 cover 가 필요하면 | 1주 |

### W9501 PoC 결과 요약

`cside/wtgquery/` — mds W9501S01 의 input/output struct (`W9501S01_in_t` /
`_dat_t` / `_out_t`) 와 memory layout 동일한 wire-compat 헤더 + mci-chart
REST 백엔드를 한 줄 호출로 묶은 C SDK. NH 사내 client 는

```c
ret = mymq_call(broker, "W9501S01", &in, ..., &out, ...);   // 기존
```

을

```c
ret = wtg_query_w9501s01(&cli, &in, &out, sizeof(out_buf)); // PoC
```

로 단순 교체 — broker 우회, `pkg/mymq` 의존 제거. 매핑:

| mds | WTG |
|---|---|
| `pdcd "SPT"` | `tf "1d"` |
| `symb "USDKRW"` (6 chars) | `pair "USD/KRW"` |
| `tenor ""` | (무시 — spot 만) |
| `opened_at` RFC3339 | `kymd "yyyymmdd"` + `khms "HHmmss"` |
| `open_bid` float64 | `bid_open "%.5f"` 16-char ASCII |

검증: `make wtgquery && make test-wtgquery`.

**W9501S01 (종가)** — `mci-chart /v1/chart` 백엔드:
- `TestCSideWtgquery_W9501S01_HappyPath` (1 일봉)
- `TestCSideWtgquery_W9501S01_MultiBar` (3 일봉)

**W9501S02 / S03 (거래소별 spot)** — `mci-price /v1/best-stats` 백엔드.
`BestSymbolStat.SourceQuotes` 신설 (per-source bid/ask/ts 노출) → wtgquery
가 활용:
- `TestCSideWtgquery_W9501S02_BEST` — exnm "BEST" → BestBid/Ask
- `TestCSideWtgquery_W9501S02_BySource` — exnm "KMB" → SourceQuotes[KMB]
- `TestCSideWtgquery_W9501S02_UnknownExnm` — miss 시 bid/ask 0, src=' ',
  best 만 채워짐 (mds 동일 동작)
- `TestCSideWtgquery_W9501S03_Bulk` — 1회 best-stats fetch 로 3 pair fill

S02/S03 의 채움 범위는 PoC 의 **핵심 필드** (exnm/symb/bid/ask/bid_best/
ask_best/bid_source/ask_source) 만. mds 의 audit 성 필드 (시초/고저/전일대비/
base/fill/mid 등) 는 빈 문자열 — phase 2 에서 봉 영역 (Aggregator) 과 연결.

FWD pdcd 는 `WTGQUERY_E_UNSUPPORTED` — forward-snapshot endpoint 와의
연결은 phase 2.

## 참조

### mds 측
- `/Users/winwaysystems/mywork/nmds/mds/CLAUDE.md` — 두 가지 빌드 모드 (standalone/server) + 핫 패스 불변식
- `/Users/winwaysystems/mywork/nmds/mds/docs/06-아키텍처.md` — query-server service 카탈로그 + cooker 아키텍처
- `/Users/winwaysystems/mywork/nmds/mds/docs/08-베스트호가분석.md` — `mdssise_make_best` 모델 (WTG `BestConsumer` 가 동일 정책 채택)

### WTG 측
- `docs/mci-architecture.md` — WTG 의 컴포넌트 흐름도
- `docs/observability.md` — `/v1/best-stats` 등 운영 진단 endpoint
- `internal/price/best.go` — `BestConsumer` (mds `mdssise_make_best` 와 동일 알고리즘)
- `cside/wtgprice/` — cside C SDK 패턴 (adapter PoC 의 참조)
