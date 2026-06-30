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

## 의심 위치 (코드 인덱스 추정)

| 위치 | 의심 사유 |
|---|---|
| `internal/edge/price/registry.go:348` `Broadcast` | 매 tick 마다 `r.mu.RLock` + 모든 subs iterate. 1000 sub × 1초당 N tick = 큰 RLock 경합 |
| `Subscriber.Send` non-blocking enqueue | OK — 격리 mechanism 정상이라면 일부는 끊겼어야 |
| `writeLoop` 의 `SetWriteDeadline(10s)` | 10초 안에 write 못 하면 ws 끊김 — log 에 안 보임 = 10초 안엔 처리되지만 read 까지 못 옴 |
| mci-price `SubscribeQuote` stream 의 mci-edge-price 측 send chan | 가능성 있음. backpressure 80% 알람도 발동 안 했으니 다른 layer |

→ **PoC 범위 밖** — 별도 추후 진단 작업 필요.

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
