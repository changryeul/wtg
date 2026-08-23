# 통합 시세 엣지 설계 — 폴리모픽 envelope + 심볼 라우팅

> 목표: 클라이언트가 **소켓 하나**로 자산군을 섞어 구독하되, 내부 시세 파이프라인
> (FX / KRX)은 **분리된 채로 유지**한다. 마진은 자산군별 파이프라인 **단계**로
> 표현한다(FX=적용, 증권=미적용). "통합은 엣지에서만" — 내부 코드 병합의 리스크
> 없이 클라 통합의 이득을 얻는다.
>
> 배경 결정: `docs/mci-architecture.md`(두 트랙 구조), `docs/client-quote-subscribe.md`
> (현행 구독 가이드), 본 문서는 그 위의 통합 엣지 스펙.

## 1. 배경 — 현행 두 트랙과 불일치

| 축 | FX 트랙 | KRX 트랙 |
|---|---|---|
| 엣지 | `mci-edge-price` (`internal/edge/price`) | `mci-edge-krx` (`internal/krx`) |
| 상류 | gRPC `PriceService.SubscribeQuote` (단일 mci-price) | KRX multicast 직수신 |
| 클라 envelope 판별자 | `type` (예 `"quote"`) | `kind` (예 `"fut.trade"`) |
| 심볼/라우팅 키 | `pair` `"USD/KRW"` | `code` `"101V6000"` |
| 구독 control 키 | `{"type":"subscribe","pairs":[...]}` | `{"type":"subscribe","symbols":[...]}` |
| 시각 | `ts_unix_nano` (int64) | `time` (string `"HHMMSSuuuuuu"`) |
| JWT | `?access_token=` → Bearer (필수) | **없음** |
| envelope version | 없음 (`v` 는 가격표 버전, 별개) | 없음 |
| 마진 | 적용 (`bid/ask` = 마진가, `raw_bid/raw_ask` 동봉) | 미적용 (raw) |

**정규화 대상 4가지**: ① 판별자 키(`type`/`kind`), ② 구독 키(`pairs`/`symbols`),
③ 심볼 키(`pair`/`code`), ④ 시각 표현(unix-nano/HHMMSS). envelope version 은 양쪽
다 부재라 신규 도입 지점이 깨끗하다.

## 2. 설계 원칙

1. **엣지 통합, 파이프라인 분리** — 클라는 소켓 하나. 내부 FX/KRX 파이프라인은
   손대지 않는다(마진엔진 ↔ codec 병합 = 금지, 이게 리스크의 전부).
2. **마진은 판별자로 표현** — 마진 유무는 아키텍처 분기가 아니라 payload 차이다.
   FX payload 는 마진 필드를 갖고, 증권 payload 는 안 갖는다. 클라는 `asset_class`/
   `type` 로 이미 분기 렌더하므로 특별 처리 불필요.
3. **버전 네고 후 전환** — v2(통합) envelope 는 opt-in. 기존 클라는 무영향(legacy
   default 유지). 리스크·롤백을 연결 단위로 격리.
4. **심볼은 불투명 문자열** — 엣지는 심볼 문법을 해석하지 않는다. 카탈로그
   (asset_class/market/upstream)로만 상류를 판별한다.

## 3. 폴리모픽 envelope v2

### 3-1. 구조 — 안정 헤더 + 자산별 payload

```json
{
  "ev": 2,                       // envelope version (신규, 필수)
  "type": "fx.quote",            // 판별자 (정규화: type 로 통일, 계층 네임스페이스)
  "asset_class": "FX",           // 굵은 분기용 (FX|FUTURE|OPTION|BOND|EQUITY)
  "symbol": "USD/KRW",           // 통합 심볼/라우팅 키 (pair·code 를 여기로 흡수)
  "ts_unix_nano": 1699999999000, // 정규화된 시각 (KRX HHMMSS → 엣지에서 변환)
  "data": { ...자산별 필드... }   // 폴리모픽 payload
}
```

- **헤더 6필드는 전 자산 공통·불변.** 클라는 `type`(또는 `asset_class`)으로 스위치하고
  `data` 만 자산별로 파싱 → 라이브러리 한 벌로 전 자산 처리.
- `symbol` 은 라우팅·식별 통합 키. FX 는 `"USD/KRW"`, KRX 는 `"101V6000"` 이 그대로
  들어감(엣지는 문자열로만 취급).
- `ev` 는 **schema version** — 기존 FX 의 `v`(=PricingTable.Version, `server.go:448`)와
  혼동 금지. `v` 는 `data` 안으로 이동.

### 3-2. `type` 네임스페이스 (판별자 통일)

현행 `type="quote"` / `kind="fut.trade"` 이원화를 **`type` 하나**로 통일하고,
값을 `<asset>.<msg>` 계층으로:

| type | asset_class | data 스키마 출처 |
|---|---|---|
| `fx.quote` | FX | `encodeCustomerQuoteJSON` (`server.go:435`) |
| `fx.tick` | FX | raw best tick (`encodeTickJSON`) |
| `krx.fut.trade` | FUTURE | `pkg/krx.FutTrade` |
| `krx.fut.book` | FUTURE | `pkg/krx.FutBook` (호가 N단 배열) |
| `krx.fut.settle` | FUTURE | `pkg/krx.FutSettle` |
| `krx.fut.master` | FUTURE | `pkg/krx.FutMaster` |
| `krx.bond.trade` | BOND | `pkg/krx.BondTrade` |
| `krx.bond.book` | BOND | `pkg/krx.BondBook` |
| `krx.bond.master` | BOND | `pkg/krx.BondMaster` |
| (control) `subscribed`/`unsubscribed`/`error` | — | 헤더만, `data` 없음 |

### 3-3. data payload — 마진 차이는 여기서 자연 표현

**FX (`fx.quote`)** — 마진 적용가 + 원시값:
```json
"data": {
  "bid": 1330.20, "ask": 1330.80,       // 마진 적용
  "raw_bid": 1330.40, "raw_ask": 1330.60,
  "tenor": "SPOT", "chan": "WEB", "site": "HQ", "tier": "VIP",
  "v": 42,                               // PricingTable.Version (기존 최상위 v 이동)
  "quote_id": "...", "valid_until_unix_nano": 0, "customer_id": "..."
}
```

**KRX 선물 (`krx.fut.book`)** — 마진 필드 없음, 호가 N단:
```json
"data": {
  "askTot": 100, "bidTot": 120,
  "ask": [{"prc":405.10,"vol":30,"cnt":3}, ...],   // 5단 배열
  "bid": [{"prc":405.05,"vol":40,"cnt":4}, ...],
  "expPrc": 405.08, "expVol": 500
}
```

> 마진 유무 = payload 필드 유무. `raw_bid`/`raw_ask` 는 FX 에만, 호가배열은 KRX 에만.
> 엣지·클라 어느 쪽도 "이 자산은 마진 있음/없음" 특수 분기 코드가 필요 없다 —
> `type` 만 보면 됨.

### 3-4. 버저닝 & 마이그레이션 (리스크 격리)

- **네고**: 연결 시 `?ev=2` 쿼리 또는 WS subprotocol `wtg.quote.v2` 로 opt-in.
  미지정 = **legacy 유지** (FX 는 현행 flat `{type:"quote",pair,...}`, KRX 는 flat `{kind,...}`).
- 엣지는 연결별 `ev` 를 기억하고 인코더를 분기(legacy encoder vs v2 encoder). 두 인코더
  공존 → 기존 클라 **무영향**, 신규 클라만 v2.
- 롤백 = 클라가 `?ev` 를 빼면 즉시 legacy. 서버 재배포 불필요.
- legacy encoder 는 v2 안정화 + 클라 이관 완료 후 별도 릴리즈에서 제거.

## 4. 통합 구독 프로토콜

```json
{"type":"subscribe",   "symbols":["USD/KRW","005930","101V6000"]}
{"type":"unsubscribe", "symbols":["101V6000"]}
```

- **구독 키 `symbols` 로 통일** (KRX 현행과 동일, superset). FX 의 `pairs` 는
  **deprecated alias** 로 계속 수용(`controlRequest` 에 둘 다 매핑).
- 심볼은 자산 섞어서 한 구독에 지정 가능. 엣지가 심볼별로 상류를 라우팅(§6).
- 서버 echo: `{"type":"subscribed","symbols":[...]}` (KRX 엣지에 현재 echo 없음 →
  통합 시 FX 와 동일하게 echo/error 프레임 추가).

## 5. 엣지 아키텍처 — `mci-edge-price` 를 통합 엣지로

새 바이너리를 만들지 않고 **`mci-edge-price` 를 통합 엣지로 승격**한다. 이유:
- JWT(`?access_token=`), profile 매칭 fan-out(`registry.SendByProfile`), per-subscriber
  send-queue + slow-consumer 격리(`writeLoop`, `server.go:877`)가 **이미 완성** — KRX
  엣지엔 없는 것들. KRX 를 여기로 접으면 JWT·격리를 공짜로 얻는다.
- DMZ 노출 지점(`mci-edge-price` 8083)이 하나로 유지 → 클라 소켓 하나.

추가 구성요소:
```
                          ┌──────────────── mci-edge-price (통합 엣지) ────────────────┐
 mci-price ──SubscribeQuote(gRPC)──▶│ FX upstream adapter ─┐                              │
                                    │                       ├─▶ symbol router ─▶ fan-out ─┼─▶ 클라 WS
 mci-price-krx ──(신규 stream)─────▶│ KRX upstream adapter ─┘   (asset_class 별)          │  (v2 envelope)
                                    └──────────────────────────────────────────────────┘
```

- **FX upstream adapter**: 현행 그대로(`consumeQuoteOnce` → CustomerQuote). 재사용.
- **KRX upstream adapter (신규)**: KRX 시세를 엣지로 넣는 경로. 두 안:
  - (A) `mci-price-krx` 가 gRPC stream 을 신설(§7의 KRX proto) → 엣지가 gRPC fan-in.
    상류 연결이 gRPC 로 균일, mTLS·재연결 로직 재사용. **권장.**
  - (B) 통합 엣지가 KRX multicast 를 직접 join. 신규 proto 불필요하나 DMZ 가 mcast 를
    받아야 해 망 구조상 부담. 비권장.
- **symbol router (신규)**: 심볼 → 상류/자산군 판별. §6 카탈로그로 구동. 구독 요청의
  각 심볼을 해당 adapter 로 등록, 수신 tick 을 v2 envelope 로 인코딩해 fan-out.

## 6. Instrument 카탈로그 — 심볼→상류 판별 (유일 필수 신규)

현재 symbol→상류 판별 근거가 없다: FX `SymbolEntry`(`symbolmap.go:11`)는
`{symbol,pair,active}` 뿐 asset_class/market 없음, KRX 는 etcd 카탈로그 자체가 없고
종목코드가 시세 스트림에 내장.

**통합 카탈로그** 도입 (etcd `wtg/catalog/instruments/<symbol>`):
```json
{
  "symbol": "USD/KRW",
  "asset_class": "FX",          // FX|FUTURE|OPTION|BOND|EQUITY
  "market": "OTC",              // OTC|KRX|...
  "upstream": "fx",             // 엣지 라우팅 태그: fx|krx
  "active": true
  // FX 전용: "pair":"USD/KRW"  / KRX 전용: 필요 시 underlying/expiry 등
}
```

- FX 는 기존 `SymbolEntry` 를 **superset 으로 확장**(`asset_class`/`market`/`upstream`
  추가, 기존 필드 유지 → 하위호환). `EtcdSymbolWatcher` 로직 재사용.
- KRX 는 신규 seed — 마스터 TR(`FutMaster`의 optType/underlying/expiry)을 카탈로그로
  올리는 배치 또는 정적 seed.
- 이 카탈로그는 이후 SecurityMaster(tick-size/lot-size/거래시간)로 확장되는 씨앗
  (별건 로드맵의 Phase A와 합류). 지금은 **라우팅에 필요한 최소 3필드
  (asset_class/market/upstream)** 만.

## 7. gRPC — KRX stream 신설 (안 A 채택 시)

현행 `price.proto` 에 KRX 시세 메시지 없음. 안 A 라면 KRX 전용 stream 추가(FX proto
는 무변경, 필드번호 append-only):
- `KrxQuoteService.SubscribeKrx(KrxSubscribeRequest) → stream KrxEvent`
- `KrxEvent { string type; string symbol; int64 ts_unix_nano; bytes payload; }`
  — payload 는 `pkg/krx` struct 의 JSON(엣지 passthrough), 또는 타입별 oneof.
- 엣지는 FX(CustomerQuote)·KRX(KrxEvent) 둘 다 v2 envelope 로 정규화 인코딩.

## 8. 단계별 구현 (독립 가치 + 리스크 순)

**Phase 1 — envelope/구독 정규화 (fan-in 없음, 저리스크)**
- v2 envelope 인코더 + `ev` 네고를 **양 엣지에 각각** 추가(legacy 병존).
- 구독 control `symbols` 통일(`pairs` alias 유지), KRX 엣지에 echo/error 프레임 추가.
- 시각 정규화(KRX HHMMSS → ts_unix_nano, KST 기준).
- 산출: 두 엣지가 **동일 v2 프로토콜**을 말함. 클라 라이브러리 한 벌 작성 가능.
  (아직 소켓은 둘 — 프로토콜만 통일)

**Phase 2 — 단일 소켓 fan-in (목표 달성)**
- Instrument 카탈로그(§6) 도입 + symbol router.
- KRX upstream adapter(§5 안 A: KRX gRPC stream §7) → 통합 엣지 fan-in.
- KRX 트래픽에 JWT·slow-consumer 격리 적용(엣지 재사용으로 자동).
- 산출: 클라가 `mci-edge-price` 소켓 하나로 FX+KRX 혼합 구독. **목표 완료.**

**명시적 비목표 (하지 않음)**
- `pkg/pricing`(FX 마진엔진) ↔ `pkg/krx`(codec) 코드 병합. 두 파이프라인은 계속 분리.
- 내부 tick 도메인 통합(공통 Quote 타입). 별건 로드맵 Phase A~B 로 이관.

## 9. 재사용 vs 신규 요약

| 구성요소 | 판정 | 근거 |
|---|---|---|
| JWT(`?access_token=`)·profile fan-out·slow-consumer 격리 | 재사용 | `edge/price/server.go`, `registry.go` |
| FX upstream(gRPC SubscribeQuote) | 재사용 | `server.go:386-430` |
| 구독 control 파서 | 확장 | `pairs`→`symbols` 통일, alias 유지 |
| envelope 인코더 | 신규(v2) + 기존(legacy) 병존 | `encodeCustomerQuoteJSON` 기반 확장 |
| envelope `ev` version + 네고 | 신규 | 양 트랙 부재 |
| `type` 네임스페이스 판별자 | 신규(정규화) | `type`/`kind` 통일 |
| 시각 정규화(HHMMSS→nano) | 신규 | KRX `time` 문자열 |
| Instrument 카탈로그(asset_class/market/upstream) | 신규(필수) | symbol→상류 판별 근거 부재 |
| symbol router | 신규 | 엣지 라우팅 |
| KRX gRPC stream | 신규(안 A) | `price.proto` 에 KRX 부재 |
| FX 마진엔진 / KRX codec | **무변경** | 비목표(리스크 회피) |

## 10. 구현 현황

**Phase 1 — 완료 (양 엣지 정규화, 소켓은 아직 둘).**

FX 엣지 (`internal/edge/price/`):
- `?ev=2` 버전 네고 (`Subscriber.ev`, `/v1/connections` 진단에 `ev` 노출).
- `encodeCustomerQuoteV2` — `fx.quote` 폴리모픽 envelope + `encodeQuoteVariants`.
- `SendByProfileV`/`SendByCustomerIDV` — fan-out당 v1·v2 각 1회 인코딩, subscriber별
  `SendVersioned` 선택 (같은 profile 에 legacy·v2 공존).
- 구독 `symbols`(통일)+`pairs`(alias) 합집합, echo 에 둘 다.
- 테스트: `envelope_v2_test.go` (v2 구조/버전선택/per-connection/키합집합).

KRX 엣지 (`internal/krx/`):
- `?ev=2` 네고 (`Subscriber.ev`), `BroadcastBySymbolV` variant fan-out.
- `buildKrxV2` — `krx.<kind>` 판별자 + `asset_class`(FUTURE/BOND) + `symbol=code`
  + `data`(원 struct 무손실 passthrough).
- **시각 정규화** `krxTimeToUnixNano` — `HHMMSSuuuuuu`→`ts_unix_nano` (Asia/Seoul).
- 구독 echo/error 프레임 추가 (기존엔 없던 것 — FX 엣지와 동일 프로토콜로).
- 테스트: `envelope_v2_test.go` + `ws_e2e_test.go`(echo 소비).

**Phase 1 잔여**: KRX 엣지 JWT 는 **Phase 2 로 이관** — 통합 엣지(edge-price)로
접으면 기존 JWT·slow-consumer 격리를 상속하므로, 별도로 두 번 만들지 않는다.

**Phase 2 — 완료 (단일 소켓 달성, 안 A 채택).**
- ✅ **Instrument 카탈로그 + symbol router** (`pkg/instrument`) — 자산-중립 신규 패키지.
  `Instrument{symbol,asset_class,market,upstream,active}`, `Catalog`(atomic snapshot)
  `Lookup`/`Route`/`RouteAll`, `EtcdCatalogWatcher`(prefix `wtg/catalog/instruments/`,
  EtcdSymbolWatcher 패턴), `LoadFile`(정적 JSON). 시드 `etc/instruments.json`. 테스트 완비.
  FX 도메인(pkg/quote) import 0 — 공통 추상 레이어.
- ✅ **KRX gRPC stream (안 A)** — `KrxPriceService.SubscribeKrx`(price.proto 신설,
  `KrxEvent{type,asset_class,symbol,ts_unix_nano,payload}`). `mci-price-krx` 가
  `--grpc-listen` 으로 노출 — SHM 경로와 독립하게 동일 원 TR 을 `krx.Server`(디코드·
  enrich)로 gRPC fan-out (오늘 두 프로세스가 각자 디코드하던 것과 동일 비용).
- ✅ **통합 엣지 fan-in** — `mci-edge-price` 가 `--krx-upstream` 으로 mci-price-krx
  fan-in. `--instruments-file`/`--instruments-etcd-prefix` 로 카탈로그 로드. 구독 gate
  가 카탈로그 active 심볼(KRX 등)을 FX pairValidator 우회해 허용, KRX 이벤트를 v2 로
  wrap 해 종목(symbol) 구독자에게 `BroadcastBySymbolV` fan-out (profile 무관). 진단
  `GET /v1/catalog`. KRX 트래픽이 edge-price 의 JWT·slow-consumer 격리 상속.
- **결과: 클라 소켓 하나(`mci-edge-price` 8083)로 FX+KRX 혼합 구독.** 목표 달성.
  기존 FX 클라 무영향(카탈로그/KRX 상류 미설정 시 기존 동작), 롤백=플래그 제거.

**남은 정리(선택)**: 기존 `mci-edge-krx`(독립 WS)는 통합 엣지로 대체 가능 —
전환 완료 후 은퇴. FX↔KRX codec 병합은 여전히 비목표.

## 11. 리스크 & 롤백

- **최대 리스크는 symbol router 오라우팅 + envelope 버저닝** — 그래서 `ev` 네고로
  연결 단위 격리하고, 카탈로그 오류 시 해당 심볼만 영향(전체 아님).
- FX↔증권 가격로직 병합 같은 대형 리스크는 **설계상 발생하지 않음**(§8 비목표).
- 롤백: 클라 `?ev` 제거 → legacy 즉시 복귀. Phase 2 fan-in 장애 시 KRX adapter 만
  비활성(FX 경로 무영향).
