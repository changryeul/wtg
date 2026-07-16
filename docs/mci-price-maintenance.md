# mci-price 유지보수 가이드 (폐쇄망·단독 작업자용)

> **이 문서의 대상**: 원 개발자도, 인터넷도, AI 도움도 없는 환경에서 `mci-price` 를
> 혼자 수정해야 하는 개발자. Go 는 낯설고 C 는 익숙하다고 가정한다.
>
> **읽는 순서**: (1) "10분 지도" 로 전체 흐름을 잡고 → (2) 하려는 작업에 맞는
> "레시피" 로 바로 간다. 처음부터 코드를 정독하지 말 것. 지도 → 레시피 → 검증,
> 이 세 단계만 지키면 된다.
>
> **가장 중요한 원칙 하나**: `mci-price` 는 시세를 *받아서* → *가공해서* → *내보내는*
> 파이프라인이다. 수정의 90% 는 "이 파이프라인의 어느 마디를 건드리느냐" 로 귀결된다.
> 아래 지도에서 당신이 손댈 마디를 먼저 찾아라.

---

## 1. 10분 지도 — mci-price 가 하는 일

`mci-price` 는 **시세 팬아웃(fan-out) 엔진**이다. 한 곳에서 raw 시세를 받아, 여러
가공기(consumer)에 동시에 흘려보내고, 각 가공기가 downstream 으로 내보낸다.

```
 [시세 producer]                                    [소비처]
 cooker / quote-forwarder                           고객 ws / 챠트 / 매칭엔진
        │                                                  ▲
        │ UDP FIX → broker broadcast                       │
        │ (또는 gRPC PublishTick 로 broker 우회)           │
        ▼                                                  │
 ┌──────────────────────────────────────────────────────────────┐
 │  mci-price                                                     │
 │                                                                │
 │  ① 수신·디코드        ② 정규화          ③ 가공기들(consumer)   │
 │  ┌───────────┐        ┌──────────┐     ┌─────────────────────┐│
 │  │ Server    │──raw──▶│ Tick 으로 │──▶ │ BestConsumer  (best) ││──┐
 │  │ subscribe │        │ 변환     │     │ Aggregator    (봉)   ││  │
 │  │ loop      │        │          │     │ PricingConsumer(마진)││──┼─▶ 고객
 │  └───────────┘        └──────────┘     │ CrossRate     (합성) ││  │
 │        ▲                               │ AlgoStream    (봇)   ││  │
 │        │                               │ GRPCServer(raw 중계) ││──┘
 │  broker/UDP/gRPC                       └─────────────────────┘│
 └──────────────────────────────────────────────────────────────┘
```

**세 마디만 기억하면 된다:**

| 마디 | 하는 일 | 대표 파일 |
|-----|--------|----------|
| ① 수신·디코드 | broker/UDP/gRPC 에서 raw 바이트를 받아 `Tick` 으로 만든다 | [server.go](../internal/price/server.go) · [decoder.go](../internal/price/decoder.go) |
| ② 정규화 | 시세 1건(envelope)을 `Tick` 으로 만들어 모든 가공기에 뿌린다 | [server.go `IngestEnvelopes`](../internal/price/server.go) |
| ③ 가공기 | 각자 목적대로 가공 후 내보낸다 (best/봉/마진/합성/봇/raw중계) | best.go · aggregator.go · pricing_consumer.go · grpc.go |

> 가공기는 전부 **`OnTick(t *Tick)`** 이라는 하나의 규약(interface)만 구현한다.
> 새 가공기를 붙이는 것도, 기존 가공기를 이해하는 것도 이 함수 하나부터 보면 된다.
> (Go 의 interface 가 뭔지는 §5 치트시트 참고 — C 의 함수 포인터 테이블이라 생각하면 된다.)

---

## 2. 시세 한 틱의 여정 (파일:줄 앵커)

새 필드를 추가하려면 "값이 어디서 어디로 흐르는지" 를 정확히 알아야 한다. 아래가
**bid 값 하나가 producer 에서 고객까지 가는 전체 경로**다.

```
1. producer 가 JSON 한 줄을 만든다:  {"sym":"USDKRW","bid":1300.5,"ask":1300.7,"ts":...,"src":"SMB"}
        │
        ▼   이 JSON 의 스키마 = 단일 진실
2. quote.JSONEnvelope        ← pkg/quote/codec.go:18   (Sym/Bid/Ask/TS/Src/Seq)
        │                       DecodeJSONEnvelope 가 파싱+검증(codec.go:41)
        ▼
3. Server.IngestEnvelopes    ← internal/price/server.go:496
        │  envelope → Tick 으로 변환. bid/ask 는 Tick 에 "타입 필드로" 안 들어가고
        │  다시 JSON 으로 인코딩되어 Tick.Body(raw bytes)에 실린다. (중요!)
        ▼
4. Tick                      ← internal/price/decoder.go:29
        │  MarketID/Symbol/SeqNum/Mask/Type/Flag/Body/Source/Received
        │  ※ bid/ask 는 Body 안에 있다. Tick 필드가 아니다.
        ▼   c.OnTick(sub) 로 모든 가공기에 동시 전달 (server.go:540 근처)
        ├─▶ BestConsumer.OnTick    best.go:207 → Body 를 다시 DecodeJSONEnvelope(best.go:229)해서 env.Bid/env.Ask 사용
        ├─▶ PricingConsumer.OnTick pricing_consumer.go:236 → 마진 적용 → CustomerQuote 생성
        ├─▶ Aggregator.OnTick      aggregator.go:77 → OHLC 봉 누적
        └─▶ GRPCServer.OnTick      grpc.go:176 → raw Tick 을 그대로 gRPC 로 중계
        ▼
5. 두 갈래로 고객에게 나간다:
   (a) raw 중계:  grpc.go tickToProto(grpc.go:723) → proto Tick.body(=passthrough)
   (b) 마진 quote: pricing → pkg/pricing.CustomerQuote(types.go:81) → proto CustomerQuote(price.proto:200)
```

**여기서 얻어야 할 핵심 두 가지:**

1. **`Tick.Body` 와 proto `Tick.body` 는 "해석 안 하고 그대로 통과"(passthrough)** 한다.
   producer 가 JSON 에 새 필드를 넣으면, 그 필드는 아무 코드도 안 고쳐도 Body 에 실려
   고객까지 그냥 흘러간다. (edge 는 body 를 해석하지 않는다 — `price.proto:148` 주석 참고)

2. **값이 "타입 필드" 로 노출되는 곳은 딱 두 군데** — `quote.JSONEnvelope`(수신측)와
   `pricing.CustomerQuote`(고객 노출측). 새 필드를 best 산정이나 마진 계산에 쓰거나,
   gRPC 에서 이름 붙은 필드로 노출하려면 이 두 struct + proto 를 고쳐야 한다.

이 차이가 **레시피 1(쉬움)과 레시피 2(어려움)를 가른다.**

---

## 3. 레시피 — "새 시세 필드 추가"

먼저 판단하라: **고객이 그냥 보기만 하나, 아니면 mci-price 가 계산에 쓰나?**

### 레시피 1 — passthrough 필드 (고객이 표시만 함) · 난이도 ★☆☆

예: producer 가 `"vol"`(거래량), `"provider"`(호가제공사) 같은 필드를 JSON 에 추가.
mci-price 는 이 값을 계산에 안 쓰고, 고객이 화면에 표시만 한다.

**할 일: 사실상 없다.** JSON 에 필드가 있으면 `Tick.Body` 에 실려 자동으로 통과한다.

체크리스트:
- [ ] producer(cooker/quote-forwarder) 가 JSON 에 필드를 넣는가? → producer 쪽 작업 (mci-price 아님)
- [ ] mci-price 가 이 값을 검증/계산에 쓸 필요가 없는가? → 없으면 **끝**. 코드 수정 0.
- [ ] 고객(ws 클라이언트)이 body JSON 에서 그 필드를 읽어 표시 → 고객측 작업

> ⚠️ 단, `quote.JSONEnvelope`(codec.go:18)에 없는 필드는 `DecodeJSONEnvelope` 가
> **무시**한다(에러 아님). best/pricing 은 그 필드를 못 본다. "통과만" 이면 그게 정상.

---

### 레시피 2 — 1급 시세 필드 (mci-price 가 계산/노출) · 난이도 ★★★

예: `"mid"`(중간값)를 받아서 best 산정에 쓰고, gRPC 로 이름 붙은 필드로 내보내기.

**반드시 아래 순서대로.** 순서를 지키면 각 단계에서 컴파일이 깨지며 다음 할 일을 알려준다.

```
① 수신 스키마에 필드 추가
   pkg/quote/codec.go:18  JSONEnvelope struct 에 필드 추가
     Mid float64 `json:"mid,omitempty"`
   (필요하면 DecodeJSONEnvelope(codec.go:41)에 검증 규칙 추가)

② 이 값을 쓰는 가공기를 수정
   - best 산정에 쓰려면:   internal/price/best.go:229 근처 (env 에서 env.Mid 사용)
   - 마진에 쓰려면:        internal/price/pricing_consumer.go:236~ (raw quote 구성부)

③ 고객 노출 struct 에 추가 (gRPC 로 이름 붙은 필드로 내보낼 때만)
   pkg/pricing/types.go:81  CustomerQuote struct 에 Mid 추가
   api/proto/wtg/v1/price.proto:200  message CustomerQuote 에 필드 번호 추가
     double mid = 15;   // ← 반드시 새 번호. 기존 번호 재사용 금지!

④ proto 재생성 (③ 을 했을 때만)
   make proto
   → pkg/wtgpb/v1/*.pb.go 가 다시 만들어진다. 이 파일은 손으로 고치지 말 것.

⑤ 매핑 함수 갱신 (③ 을 했을 때만)
   internal/price/grpc.go customerQuoteToProto (grpc.go:821~) 에 Mid 매핑 한 줄 추가
   (raw Tick 으로 내보내면 grpc.go:723 tickToProto — 단 raw 는 body passthrough 라 보통 불필요)
```

**필드 추가 파일 지도 (한 장 요약):**

| 목적 | 고칠 파일 | proto 재생성 |
|-----|----------|:-----------:|
| 받기만(통과) | 없음 (레시피 1) | ✗ |
| best/마진 계산에 사용 | codec.go + best.go/pricing_consumer.go | ✗ |
| gRPC 로 이름 붙여 노출 | 위 + pricing/types.go + price.proto + grpc.go 매핑 | ✅ `make proto` |

> ⚠️ **proto 필드 번호 규칙 (폐쇄망에서 특히 조심):** 기존 번호를 절대 바꾸거나
> 재사용하지 말 것. 새 필드는 항상 다음 번호. 번호를 바꾸면 이미 배포된 edge/매칭엔진과
> wire 호환이 깨져 조용히 값이 틀어진다 (에러도 안 남).

---

## 4. 레시피 — 카탈로그/설정 변경 (코드 수정 아님) · 난이도 ★☆☆

통화쌍 추가, 마진(spread/skew) 조정, Profile 신설 등은 **코드를 고치지 않는다.**
`mci-admin` UI 에서 바꾸면 etcd 에 저장되고, 모든 `mci-price` 인스턴스가 watch 로
**재배포 없이 즉시** 반영한다.

| 바꿀 것 | 어디서 | 반영 방식 |
|--------|-------|----------|
| 통화쌍/심볼 | mci-admin UI → symbols | etcd watch hot-reload |
| 마진 정책 | mci-admin UI → pricing | etcd watch hot-reload |
| Profile(채널·사이트·등급) | mci-admin UI → profiles | etcd watch hot-reload |

정적 파일 모드(etcd 없이)면 `etc/{symbols,profiles,pricing}.json` 을 고치고 재기동.
자세히는 [operations.md](operations.md), [margin-business-spec.md](margin-business-spec.md).

> 👉 "필드/계산 로직" 이 아니라 "값/정책" 을 바꾸는 거라면 **코드를 열지 마라.**
> 십중팔구 UI 에서 된다.

---

## 5. Go-magic 치트시트 (C 개발자가 걸려 넘어지는 것)

Go 코드에서 "이게 뭐지?" 싶은 것들을 C 로 번역했다.

| Go 에서 본 것 | C 로 치면 | mci-price 어디서 |
|-------------|----------|----------------|
| `type TickConsumer interface { OnTick(*Tick) }` | **함수 포인터 테이블(vtable).** 이 함수만 있으면 어떤 struct든 "가공기" 로 취급 | server.go:39. 새 가공기 = OnTick 만 구현 |
| `for _, c := range s.consumers { c.OnTick(t) }` | 리스트 순회하며 콜백 호출 | server.go:540 근처 — 시세를 모든 가공기에 뿌리는 곳 |
| `go someFunc()` | **스레드 하나 띄우기.** 반환값 없음, 알아서 돈다 | Start/subscribeLoop 등 |
| `ch <- t` / `<-ch` | **스레드 안전 큐(FIFO) push/pop.** lock 안 걸어도 됨 | gRPC 구독자 fan-out (grpc.go) |
| `ctx context.Context` | **"이제 그만" 신호선.** ctx.Done() 닫히면 종료하라는 뜻 | 거의 모든 Start/Loop 첫 인자 |
| `func NewServer(cfg, logger, consumers ...)` | **가변인자 + 생성자.** `...` 는 C 의 `...`(va_list)와 같음 | server.go:125 |
| `s.AttachXxx(...)` 무더기 | **의존성 조립.** main.go 가 부품을 Server 에 꽂는 과정. 런타임 로직 아님 | server.go 곳곳, cmd/mci-price/main.go |
| `slog.Info("x", slog.Int("n", 3))` | 구조화 로그(printf 아님). key=value 로 찍힌다 | 전역 |
| `atomic.Uint64` / `.Add(1)` | lock 없는 카운터 (`__atomic_add`) | 통계 카운터들 |
| `err != nil` 반복 | Go 는 예외 없음. **모든 함수가 에러를 반환값으로 돌려준다.** if 로 매번 확인 | 전역 |

> **가장 헷갈리는 지점**: `main.go`(cmd/mci-price/main.go, 823줄)의 대부분은
> **"부품 조립"** 이지 "로직" 이 아니다. `if cfg.X { srv.AttachX(...) }` 는
> "X 기능을 켜면 이 부품을 꽂는다" 는 뜻. 실제 시세 처리 로직은 여기 없고
> §1 지도의 가공기 파일들에 있다. main.go 를 정독하려 하지 마라.

---

## 6. 고치고 나서 검증하는 법 (AI·인터넷 없이)

수정 후 이 순서로 확인하면 대부분의 실수를 잡는다.

```bash
# 1. 컴파일 + 단위 테스트 (broker 불필요)
make test

# 2. 방금 건드린 부분만 집중 테스트 (빠름)
go test ./internal/price/ -run TestBest -v          # best 를 고쳤다면
go test ./internal/price/ -run TestPricing -v       # 마진을 고쳤다면
go test ./pkg/quote/  -run TestEnvelope -v          # envelope 스키마를 고쳤다면

# 3. proto 를 고쳤다면 재생성이 잘 됐는지
make proto && make build

# 4. broker 없이 단독 부팅해서 살아있는지 (폐쇄망 스모크 테스트)
./build/bin/mci-price --no-broker --listen :8082

# 5. 다른 창에서 가짜 틱을 주입해 end-to-end 확인 (DevMode)
curl -X POST localhost:8082/v1/dev/tick \
  -d '{"sym":"USDKRW","bid":1300.5,"ask":1300.7,"src":"SMB"}'
curl localhost:8082/v1/best-stats        # best 산정 결과 확인
curl localhost:8082/v1/price-stats       # 수신 카운터 확인
```

> 테스트가 이미 촘촘하게 깔려 있다(파일마다 `*_test.go`). **새 필드를 추가했으면
> 기존 테스트를 복사해서 그 필드에 대한 케이스를 한 줄 추가하는 것**이 가장 안전하다.
> 예: `best_test.go` 를 열어 유사 케이스를 흉내낸다.

---

## 7. 막혔을 때 — 증상별 지도

| 증상 | 먼저 볼 곳 |
|-----|----------|
| 새 필드가 고객까지 안 감 | ① Body passthrough 면 producer JSON 확인 ② 타입 필드면 §3 레시피2 ③⑤ 매핑 누락 |
| best 값이 이상함 | best.go:207 `OnTick` → best.go:229 디코드부 |
| 마진 적용이 안 됨 | pricing_consumer.go:236 `OnTick`, 그리고 UI 의 pricing 설정(§4) |
| envelope 파싱 에러 | codec.go:41 `DecodeJSONEnvelope` 의 검증 규칙 (bid>0, ask>=bid 등) |
| proto 값이 틀어짐 | price.proto 필드 번호 재사용 여부(§3 경고), `make proto` 재실행 |
| 부팅이 안 됨 | config.go 의 flag/env 파싱, main.go 의 Attach 순서 |
| 시세가 아예 안 들어옴 | broker 연결(server.go subscribeLoop) 또는 producer 쪽 |

---

## 부록 — 파일 한 줄 요약 (internal/price)

| 파일 | 한 줄 |
|-----|------|
| server.go | 수신 loop + Tick 정규화(IngestEnvelopes) + HTTP 라우트 + 조립(Attach*) |
| decoder.go | raw 바이트 ↔ Tick 변환. `Tick` struct 정의 |
| codec.go (pkg/quote) | **시세 수신 스키마의 단일 진실** — JSONEnvelope |
| best.go | 다중시장 best 호가 산정 (max bid / min ask) |
| pricing_consumer.go | Profile 별 마진 적용 → CustomerQuote 생성 |
| aggregator.go | OHLC 봉 누적 |
| crossrate_consumer.go | 합성 통화쌍(cross rate) 계산 |
| grpc.go | 모든 gRPC 스트림(raw/quote/bar/algo/customer) + proto 매핑 |
| config.go | flag/env 파싱 (로직 아님, 설정만) |
| cmd/mci-price/main.go | 부품 조립 + 기동 (로직 아님) |

---

*이 문서는 "A안(문서/레시피 우선)" 샘플이다. 코드 자체는 거의 바꾸지 않고, 지도·레시피·*
*치트시트로 단독 작업자를 돕는다. 코드가 바뀌면 이 문서의 파일:줄 앵커도 함께 갱신해야 한다*
*— 그것이 A안의 최대 약점(drift)이다.*
