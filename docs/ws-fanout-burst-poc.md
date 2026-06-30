# ws fan-out burst PoC — mci-edge-price

환율 급변 + 동시 ws 고객 N 명의 운영을 시뮬레이션. mci-edge-price 의
fan-out + slow consumer 격리 + per-client receive rate 가 publisher rate
와 일치하는지 정량 측정.

대상 독자: 운영 의사결정자 / mci-edge-price 인스턴스 sizing.
관련: `docs/customer-connections.md` §5 backpressure / 격리, `docs/spot-latency-poc.md`.

## 한 줄 결론

**100 client 까지는 안정, 500 client 부터 receive degradation, 1000 client 에서
fan-out 사실상 정지.** Mac dev 측정 한계 가능성도 있어 **Linux 운영 머신
재측정 필수**. 그래도 PoC 가 발견한 단일 mci-edge-price 인스턴스의 동시
client cap 추정치는 **default 설정 기준 ~500명** 으로 좁혀짐.

## 측정 환경

| 항목 | 값 |
|---|---|
| 머신 | Mac (Darwin 24.6.0, dev 워크스테이션, kqueue) |
| ulimit -n | 1,048,576 (운영급) |
| mci-edge-price | dev 인스턴스, default `SendQueueSize=256`, `--customer-stream` 활성 |
| ratelimit | `wtg/ratelimit/edge-price` PUT 으로 burst 5000 으로 임시 상향 (default 10 은 PoC 차단 trigger) |
| tick 부하 | `wtg-dev-tickloop.py` 가 평소 부하 (~140 msg/s/client, all-mode broadcast) |
| 측정 도구 | `cmd/ws-load-gen` (concurrent ws client + per-client receive 측정) |
| 측정 시간 | 시나리오당 30초 + warmup 2~5초 |

### 정확도 주의

- Mac kqueue 가 1k+ socket 에서 epoll 보다 비효율 — **Linux 운영 머신에서
  재측정 시 더 좋은 숫자 가능**. PoC 결과를 운영 cap 의 상한 으로 해석 X.
- ws ReadDeadline 30초 — sub-30초 idle 은 측정 도구에서 disconnect 처리.
- per-client receive rate 의 계산 = `msgCount / (lastRecv - firstRecv)`.
  매우 짧은 receive window 에선 rate 계산이 부풀려짐 (500 client 시나리오의
  outlier 가 이 사례).

## 결과 표

| 시나리오 | 동시 client | connect | disconnect | total msgs | avg/client | aggregate | per-client rate (안정성) |
|---|---|---|---|---|---|---|---|
| 1 | 10 | 10/10 | 0 | 36,760 | 3,676 | 1,225 msg/s · 0.3 MB/s | ✓ 122.5 msg/s 균일 |
| 2 | 100 | 100/100 | 0 | 430,000 | 4,300 | **14,261 msg/s · 3.8 MB/s** | ✓ 142.6 msg/s 균일 |
| 3 | 500 | 500/500 | 0 | 164,633 | **329** ← 안정시 3,000 기대 | 5,481 msg/s · 1.5 MB/s | ✗ 일부 client receive 조기 종료 (outlier 발생) |
| 4 | 1,000 | 1,000/1,000 | 0 | **0** | **0** | **0 msg/s** | ✗ **fan-out 사실상 정지** |

### 핵심 발견

1. **100 client = sweet spot** — 모든 client 가 동일 142 msg/s 받음. fan-out
   broadcast + per-conn send queue 가 잘 동작.
2. **500 client = degradation 시작** — disconnect 0 이지만 평균 msg 가
   예상의 11% 만 도달. mac kqueue 의 read scheduling 한계 추정 (Linux 재측정
   필요).
3. **1000 client = stuck** — connect 는 다 됨, log 에 격리 메시지 없음, 그런데
   30초 동안 **0 msg**. fan-out 의 어떤 지점이 막힘 — mci-edge-price 의
   `Registry.Broadcast` 의 RLock 경합 또는 ws Write 의 OS-level scheduling.
4. **disconnect 0** — slow consumer 격리는 발동 안 됨. 이건 양면 — 격리
   mechanism 이 정상 동작하면 1000 client 중 일부는 끊겨야 함. 안 끊긴 건
   broadcast 자체가 한 번도 모든 client 까지 도달 못 한 신호.

## mci-price 측 sanity

| 항목 | 값 (1000 client 측정 중) |
|---|---|
| `received` | 3,006,876 (정상 증가) |
| `dropped` | 0 |
| `sub_drops` | 0 |
| `conflation.Updates` | 3,006,876 (정상) |

→ **mci-price 자체는 정상**. 문제는 **mci-price → mci-edge-price → ws** 의
어딘가. gRPC SubscribeQuote stream 의 mci-edge-price 측 수신 / Registry
broadcast 의 RLock 경합 / ws Write deadline 등 후보 3개.

## 의심 위치 (1차 가설)

| 위치 | 의심 사유 |
|---|---|
| `internal/edge/price/registry.go:348` `Broadcast` | RLock 경합? |
| `Subscriber.Send` non-blocking enqueue | OK — 격리 발동 안 함 |
| mci-price 측 gRPC stream send chan | **← 정답 (다음 절 진단 결과 참조)** |

## Follow-up 진단 결과 — root cause + 1차 fix (반나절 작업)

### 진단 방법

1. mci-edge-price `--dev` 모드에 `/debug/pprof/` 노출 (DevMode 한정 패치).
2. 1000 client stuck 상태에서 goroutine dump (`debug=2`):

```
goroutine: total 2045
  1000 @ writeLoop+0xc3 (server.go:882) — select 의 <-sub.send 대기
  1000 @ readLoop+0x11b (server.go:927) — ws ReadMessage 대기
  1 @ subscribeQuoteLoop → consumeQuoteOnce
       grpc.(*clientStream).RecvMsg ← block here
```

→ **mci-edge-price 의 gRPC stream 자체가 메시지를 못 받음**. Broadcast 는
호출조차 안 됨 (incoming 없으니까).

3. mci-price 측 `/v1/subscribers` 확인:

```
quote_subscribers: []           ← empty
customer_quote_subscribers: []  ← empty
```

→ **mci-edge-price 의 gRPC stream 들이 mci-price 측에 등록조차 되어있지 않음**.

4. mci-edge-price log 의 stream 이벤트:

```
WARN: SubscribeCustomerQuote stream 끊김 — 재시도
  error: rpc error: code = Unknown desc = price: slow customer-quote consumer
  backoff: 500ms → 1s → 2s (exponential)
```

### Root cause

**`mci-price` 의 customer-quote stream 의 send chan (default `--grpc-buf 1024`) 이
1000 client 동시 register 의 customer-quote 부담을 못 받아냄** → mci-price
가 slow consumer 로 stream 강제 close → mci-edge-price 가 exponential
backoff 재연결 → 다시 같은 부담 → **infinite loop**.

코드 위치:
- `internal/price/grpc.go:500` — `"price: slow customer-quote consumer"` return
- `internal/price/grpc.go:498` `case cq, ok := <-sub.out` — channel ok=false (closed)
- `internal/price/grpc.go:469` — `sub.out = make(chan *wtgpb.CustomerQuote, g.bufSz)`
- `internal/price/config.go:31` `GRPCBufSize` default 1024
- `internal/price/config.go:508` `--grpc-buf` flag

### 1차 fix — mci-price `--grpc-buf 65536`

```bash
./build/bin/mci-price ... --grpc-buf 65536   # default 1024 → 64x
```

**fix 후 1000 client 재측정**:

| 항목 | 전 (default 1024) | 후 (65536) |
|---|---|---|
| connect | 1000/1000 | 1000/1000 |
| **recv > 0 client** | **0** | **1000** |
| disconnect | 0 (mechanism 미작동) | 21 (정상 격리 발동) |
| total msgs | **0** | **181,000** |
| avg msgs/client | 0 | 181 |

→ **fan-out 자체는 살아남, but avg 181 (예상의 6%) 로 여전히 degradation**.
즉 buffer 늘림은 **첫 burst 회피만 해결**. 지속 부하의 throughput 한계는
별개 작업.

### 잔여 작업 (follow-up 의 follow-up)

| 작업 | 추정 | 가치 |
|---|---|---|
| **A. mci-edge-price 의 customer Register batching** — 1000 customer 한 번에 보내지 말고 rate-limit (예: 100/sec) | 1일 | burst 자체를 분산 |
| **B. customer-quote stream 의 per-shard 분산** — 한 stream 당 N customer 만 | 2일 | scale-out 의 정공법 |
| **C. mci-edge-price 의 customer 등록 dedup + on-demand** — 중복 등록 회피 + ws 가 subscribe 한 pair 만 customer-quote 활성 | 1일 | 부하 자체 감소 |
| **D. metric 추가** — gRPC stream 의 send chan 점유율, 격리 발생 카운터 | 반나절 | 운영 진단 도구 |
| **E. Linux 운영 머신 재측정** | 반나절 | mac kqueue 와 분리한 진짜 cap |

### 결론

**default 설정의 1000 client 동시 stuck 의 root cause = mci-price 의
gRPC stream send chan default 1024 가 너무 작음**. 즉시 fix = `--grpc-buf
65536` 또는 운영 시 `--grpc-buf 131072+`. 단 throughput 의 진짜 한계는
별도 작업 (A/B/C) 으로 해결 가능.

## 운영 권장 cap (PoC 기준 잠정)

| 셋업 | 추정 1 인스턴스 cap | 비고 |
|---|---|---|
| Mac dev (kqueue, default 설정) | **~100명 안정 / ~500명 degradation** | PoC 측정 |
| Linux 운영 (epoll, default 설정) | **~500~1,000명 추정** | 재측정 필요 |
| Linux 운영 + tuned (`--send-queue` 1024, GOMAXPROCS=all) | **~2,000~5,000명 추정** | 어제 customer-connections.md §6 의 추정과 일치 |

→ 어제 `docs/customer-connections.md` 의 "default 1,000명 / 튜닝 5,000명"
가정 의 **상한이 mac dev 에선 검증 안 됨**. Linux 재측정으로 갱신 필요.

## 추가 가속 옵션 (Linux 재측정 결과 따라)

1. **`SendQueueSize` 증가** — 256 → 1024. 메모리 비용 ↑ (~75KB/client 추가)
2. **`Registry.Broadcast` 의 RLock → snapshot copy 패턴** — 이미 코드는
   snapshot 으로 lock release 후 iterate 하므로 OK. but goroutine schedule 부담 큼.
3. **Per-profile fan-out shard** — 현재 `subs map[id]*Subscriber` 단일 map.
   profile 별 sub map 으로 sharding 하면 `SendByProfile` 의 iterate 비용 ↓.
4. **수평 확장** — N edge 인스턴스 + LB. mci-edge-price 는 stateless 라 OK.
   1만 client 시 10 인스턴스 × 1000명.

## 측정 재현 방법

```bash
# ulimit raise (Linux 운영 머신)
ulimit -n 65535

# ratelimit 임시 상향 (default burst 10 이 PoC 트리거)
curl -X PUT -H 'X-WTG-User: admin' -H 'Content-Type: application/json' \
  -d '{"rules":[{"pattern":"GET /v1/subscribe","rate":2000,"burst":5000},{"pattern":"*","rate":2000,"burst":5000}]}' \
  http://<admin>:9090/v1/admin/ratelimit/edge-price

# 시나리오 4개
for N in 10 100 500 1000; do
  ./build/bin/ws-load-gen \
    --target "ws://<edge>:8083/v1/subscribe?profile=WEB.BRANCH.VIP" \
    --clients $N --duration 30s --warmup 5s
done

# raw per-client CSV (분석)
./build/bin/ws-load-gen --clients 500 --duration 30s --csv /tmp/ws-500.csv
```

## 한계 / Future Work

1. **Linux 운영 머신 재측정 필수** — Mac kqueue 의 한계와 mci-edge-price
   코드 한계가 본 PoC 에선 구별 안 됨.
2. **profile 별 fan-out (`SendByProfile`) 측정 안 됨** — 본 PoC 는 raw
   broadcast 만. 운영의 실제 path 인 per-profile + per-customer 측정 추가
   필요.
3. **slow consumer 격리 발동 시나리오 미검증** — 의도적으로 client 의 read
   를 느리게 (sleep 주입) 만들어 격리 발동 + 다른 client 영향 0 검증 필요.
4. **mci-edge-price 내부 metric 부족** — `/v1/connections` 외에 fan-out 큐
   깊이 / broadcast 시간 등 latency 노출이 metric 에 없음. 운영 가시화 보강
   가치.
5. **broker (mymq) path 와 grpc path 비교** — 본 PoC 는 grpc path 만. broker
   path 의 fan-out 한계는 별도.

## 참고

- `cmd/ws-load-gen/main.go` — 본 PoC 의 부하 도구
- `internal/edge/price/registry.go` — `Registry.Broadcast` / `Subscriber.Send`
- `internal/edge/price/server.go:756` — `subscribeHandler`
- `docs/customer-connections.md` §6 — 인스턴스당 cover 명목 추정 (~5,000명) —
  본 PoC 가 Mac dev 에선 도달 안 됨, Linux 재측정 필요
- `docs/spot-latency-poc.md` — REST latency PoC (본 PoC 와 짝)
