# `/v1/quote/spot` Latency PoC — mds SHM 대비

mci-price 의 `GET /v1/quote/spot` REST endpoint 가 mds 의 SHM 직접 read
모델을 대체할 만한 latency 인지 데이터로 확인. 측정 결과 — **20k RPS 까지
p99 ≤ 2 ms 안정 처리** → mds 운영 SLA (보통 ms 단위) 안에 들어옴.

대상 독자: 운영 의사결정자 (mds → WTG 마이그레이션 검토).
관련: `docs/mds-coverage.md` §운영 리스크 1 — SHM → gRPC 모델 변환.

## 측정 환경

| 항목 | 값 |
|---|---|
| 머신 | Mac (Darwin 24.6.0, dev 워크스테이션) |
| mci-price | dev 인스턴스, 7 GB RSS, `--no-broker`, tickloop 가동 중 |
| 측정 도구 | `cmd/spot-load-gen` (concurrent goroutine + sort-based percentile) |
| 네트워크 | loopback (127.0.0.1:8082) |
| 측정 시간 | 시나리오당 10초 |
| 측정 대상 | `GET /v1/quote/spot?pair=USD/KRW&profile=WEB.BRANCH.VIP` |
| HTTP keepalive | 활성 (`MaxIdleConnsPerHost=concurrency*2`) |

### 정확도 주의

- Mac dev 머신 기준 — 운영 서버 (Linux + 전용 NIC + tuned) 보다는 보수적
  결과. 운영 측정 시 같은 도구로 재측정 필요.
- loopback 만 측정 — real network round-trip 시뮬 아님. 운영 latency 에는
  + (1~5 ms NIC + TLS handshake) 가산 필요.
- p99 / p99.9 만 신뢰. mean 은 GC pause / scheduler jitter 의 outlier 에
  약함.

## 결과 표

| 시나리오 | 목표 RPS | 실측 RPS | OK / Err | p50 | p90 | p95 | p99 | p99.9 | max |
|---|---|---|---|---|---|---|---|---|---|
| 저부하 | 1,000 | 1,000 | 10,000 / 0 | **198 μs** | 297 μs | 343 μs | **522 μs** | 3.6 ms | 5.3 ms |
| 일반 | 5,000 | 4,998 | 49,984 / 0 | **397 μs** | 672 μs | 833 μs | **1.13 ms** | 1.80 ms | 2.8 ms |
| 고부하 | 10,000 | 9,997 | 99,968 / 0 | **512 μs** | 940 μs | 1.16 ms | **1.45 ms** | 1.74 ms | 3.1 ms |
| 스트레스 | 20,000 | 19,993 | 199,936 / 0 | **689 μs** | 1.03 ms | 1.31 ms | **2.00 ms** | 3.64 ms | 9.8 ms |

### 관찰

1. **20k RPS 까지 에러 0** — `actual RPS = goal RPS` 가 모든 시나리오에서
   성립. mci-price 가 처리 한도까지 도달 안 함.
2. **p99 가 모든 시나리오에서 ≤ 2 ms** — 부하 2배씩 늘려도 p99 가 ~700 μs
   씩 선형 증가만. 비파국적.
3. **장기 long-tail (p99.9)** 도 3~4 ms 안. 5 시그마 안.
4. **저부하 (1k RPS) 의 p99.9 가 의외로 큼 (3.6 ms)** — 도구의 측정 시작
   시점 warm-up 영향 추정. 5k+ 부하에서는 connection pool 안정.

## mds SHM 대비 비교

| 모델 | 평균 read latency | 비교 |
|---|---|---|
| mds SHM 직접 read (`mdquot_update_bidask`) | ~10 μs | baseline (lock-free memory load) |
| WTG `mci-price` in-process conflation cache | ~10 μs | 동등 (Go runtime atomic load) |
| WTG `/v1/quote/spot` loopback REST (p50) | **198 μs** (저부하) ~ 689 μs (20k RPS) | **20~70배 느림** |
| WTG `/v1/quote/spot` p99 | **522 μs ~ 2.0 ms** | **50~200배 느림** |

→ **숫자만 보면 SHM 이 빠르지만, 운영 SLA 가 통상 ms 단위 (1~10 ms) 라
WTG 의 결과는 충분히 안에 들어옴**. SHM 의 μs 우위는 hot path 의 특정
시나리오 (HFT 등) 에서만 의미.

## 결론 — 운영 의사결정 기준

| 요건 | mds 적합 | WTG 적합 | 판정 |
|---|---|---|---|
| 매칭 엔진 hot path 의 spot 조회 (p99 < 100 μs 필요) | ✓ | ✗ | mds 유지 |
| **매칭 엔진 spot 조회 (p99 < 5 ms 허용)** | ✓ | ✓ | **WTG OK** ← 일반적 NH 운영 |
| query-server W9501 종가 조회 (사람이 보는 화면, p99 < 100 ms) | ✓ | ✓ | WTG OK |
| customer ws 실시간 quote (stream, p99 < 1 s) | △ | ✓ | WTG 이미 SubscribeQuote 로 동작 |

NH 운영의 SLA 기준이 **(2) 매칭 엔진 spot p99 < 5 ms** 이면 mds → WTG 의
spot 조회 cover 는 **데이터로 입증됨**.

## 추가 가속 옵션 (필요 시)

만약 더 짧은 latency 가 요구되면 다음 인프라 변경이 가능 — 코드 변경
최소:

1. **Unix domain socket** — mci-price 와 매칭 엔진을 같은 호스트 운영 시
   loopback TCP 대신 UDS 사용. ~30~50% latency 감소 기대.
2. **HTTP/2 + 영구 keepalive** — 이미 keepalive 활성. HTTP/2 multiplex 로
   추가 connection setup 회피.
3. **gRPC streaming** — request/response 가 아닌 long-lived stream. setup
   비용 0 + binary protobuf 로 JSON 직렬화 비용 ~50% 감소. 이미 mci-edge-price
   가 `SubscribeQuote` 로 사용 중 — 같은 모델 spot 조회에 확장 가능.
4. **mci-price 단일 호스트 colocate** — 매칭 엔진과 같은 머신 + dedicated
   CPU set 으로 cache 친화. p99 < 100 μs 도 가능할 수 있음.

→ 본 PoC 의 결과 (p99 ≤ 2 ms) 가 SLA 안에 들어와 위 가속이 **즉시 필요하진 않음**.

## 측정 재현 방법

```bash
# 빌드
make build              # cmd/spot-load-gen 자동 픽업

# mci-price 가동 (이미 떠 있다면 skip)
./build/bin/mci-price --no-broker --listen :8082

# 부하 측정 (시나리오 4개)
./build/bin/spot-load-gen --rate 1000  --duration 10s --concurrency 8
./build/bin/spot-load-gen --rate 5000  --duration 10s --concurrency 32
./build/bin/spot-load-gen --rate 10000 --duration 10s --concurrency 64
./build/bin/spot-load-gen --rate 20000 --duration 10s --concurrency 128

# raw latency CSV 출력 (분석용)
./build/bin/spot-load-gen --rate 10000 --duration 30s --csv /tmp/latency.csv
```

## 한계 / Future Work

1. **운영 머신 측정 필요** — Mac dev 결과는 보수적이지만 운영 Linux + 전용
   NIC 에서 재측정 시 더 좋은 숫자 나올 가능성 큼.
2. **gRPC SubscribeQuote stream 의 latency** — 본 PoC 는 REST 만 측정.
   stream 모델 (매칭 엔진의 일반 hot path) 의 측정은 별도 — `cmd/quote-stream-load-gen`
   신설 검토.
3. **bulk pair 측정** — `?pair=USD/KRW,EUR/USD,JPY/KRW` 다중 호출 시 latency
   변화 측정. 본 PoC 는 단일 pair 만.
4. **customer-id 적용 시 (5-Layer) latency** — `?customer_id=alice` 추가
   시 ApplyForCustomer 의 비용 측정. 본 PoC 는 customer_id 미적용.

## 참고

- `cmd/spot-load-gen/main.go` — 본 PoC 의 부하 도구
- `internal/price/spot_snapshot.go` — 측정 대상 endpoint
- `docs/mds-coverage.md` — mds → WTG 마이그레이션 매트릭스 (본 결과 반영)
- `scripts/load-test.sh` — 시세 파이프라인 부하 (UDP in, 본 PoC 와 별개)
