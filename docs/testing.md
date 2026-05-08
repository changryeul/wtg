# WTG 테스트 시나리오

작성: 2026-05-02
대상 환경: macOS / Linux, Go 1.22+, Docker (선택 사항)

본 문서는 WTG 의 모든 기능을 단계적으로 검증하기 위한 테스트 시나리오를
정리한다. 단위 테스트 → 통합 테스트 → UI 시각 검증 → broker e2e → 다중
인스턴스 → 보안 회귀까지 순서대로 진행하면 된다.

---

## 0. 사전 준비

```bash
cd ~/mywork/wtg
go version           # 1.22 이상
ls Makefile go.mod   # 둘 다 보여야 정상
```

선택 의존성:
- **Docker** — `mymqd` 또는 `etcd` 를 컨테이너로 띄울 때
- **mymq native build** — `~/mywork/mymq/build/bin/mymqd` 가 있다면 그대로 사용 가능

---

## 1. 빌드 + 단위 테스트 (broker 불필요, 필수 first-pass)

```bash
make build           # 8개 바이너리 → build/bin/
make test            # 단위 테스트 (~10초)
```

전부 `ok` 가 떠야 다음 단계로. 여기서 깨지면 코드 회귀 — broker 없이 즉시 수정 가능.

### 1.1 통합 테스트 (embedded etcd 까지 포함)

```bash
make test-integration  # build tag 'integration', ~30초, 의존성 무거움
```

- `pkg/routing/etcd_integration_test.go` — admin → api 룰 watch 전파
- `pkg/policy/etcd_integration_test.go` — kill switch / 차단 심볼 watch 전파
- `test/etcdtest` 가 임시 dir 에 embedded etcd 띄움

### 1.2 race detector + 커버리지

```bash
make test-race       # -race 활성, coverage.out 생성
make coverage        # coverage.html 까지
```

---

## 2. UI 시각 확인 (broker 없이, 5분 안에)

가장 빠른 확인 경로 — `--no-broker` 플래그로 mymqd 연결 자체를 스킵.

```bash
./build/bin/mci-admin --dev --no-broker --listen :9090
```

다른 터미널:
```bash
open http://localhost:9090/
```

### 2.1 진입 흐름

1. 로그인 화면에서 **"개발 모드로 진입"** 펼치기
2. 사용자 ID 입력 (예: `admin01`)
3. **"ID 만으로 입장 (DevMode)"** 클릭

DevMode 는 `X-WTG-User` 헤더만 신뢰하므로 broker 호출 없이 진입 가능.

### 2.2 화면별 검증 체크리스트

| 화면 | 동작 | broker 필요? |
|------|------|------------|
| 대시보드 | KPI 카드 + sparkline 갱신 (2초 polling) + Chart.js 트레이스 누적 | ❌ (brokerCard 는 DISCONNECTED) |
| 라우팅 룰 | `+ 신규` → 모달 → 저장. 활성 토글 / 수정 / 삭제 | ❌ (in-memory) |
| 정책 엔진 | Kill Switch 토글, 정비창 datetime 입력, 차단 심볼/라우팅키 칩 | ❌ |
| 브로커 명령 | Status / Clients / Exchanges 등 — **503 reconnecting 응답** | ✅ |
| API 테스터 | 프리셋 클릭 → 즉시 실행. broker 필요한 것만 503 | 부분 |
| WS 모니터 | URL 입력 → 연결. edge-push/edge-price 띄운 후 의미 | n/a |
| 감사 로그 | 룰 / 정책 변경 즉시 timeline 에 prepend (ws push) | ❌ |
| 테마 토글 | 사이드바 하단 다크 / 라이트 / 시스템 | ❌ |

### 2.3 stream 연결 상태 확인

상단 헤더의 `stream: ●` 점등 — connected / connecting / disconnected / error.
ws 가 끊어져도 exponential backoff (1s → 30s) 로 자동 재연결.

---

## 3. broker 와 e2e

### 3.1 mymqd 띄우기

이미 빌드된 `mymqd` 가 있으면 native:
```bash
cd ~/mywork/mymq/build
./mymqd -c ../etc/mymqd.cfg &
./echo_svc &  # 옵션 — generic /v1/tx 검증용 echo 서비스
```

Docker 사용 시 (이전 세션 셋업):
```bash
cd ~/mywork/mymq
docker run --rm -d -p 11217:11217 -v $PWD:/src --name mymqd \
  wtg-mymqd /src/build/bin/mymqd -c /src/etc/mymqd.cfg
```

확인:
```bash
lsof -i :11217 | head -5     # listen 중인지
nc -z 127.0.0.1 11217 && echo OK  # connect 가능한지
```

### 3.2 WTG 서비스 운영 모드 띄우기

`--no-broker` 빼고 broker 옵션만:

```bash
# 터미널 1 — admin
./build/bin/mci-admin --listen :9090 \
  --broker-host 127.0.0.1 --broker-port 11217

# 터미널 2 — mci-api
./build/bin/mci-api --listen :8080 \
  --broker-host 127.0.0.1 --broker-port 11217

# 터미널 3 — mci-edge-api (DMZ proxy → mci-api)
./build/bin/mci-edge-api --listen :8090 \
  --upstream http://127.0.0.1:8080 --dev
```

### 3.3 인증 라이프사이클

```bash
# 로그인 — broker 가 LOGON 트랜잭션 처리 → cookie_t 발급
RESP=$(curl -s -X POST -H "Content-Type: application/json" \
  -d '{"data":{"usid":"trader01","password":"x"}}' \
  http://localhost:8080/v1/login)
echo "$RESP" | jq

ACCESS=$(echo "$RESP" | jq -r .access_token)
REFRESH=$(echo "$RESP" | jq -r .refresh_token)

# Access JWT 로 transaction 호출
curl -X POST -H "Authorization: Bearer $ACCESS" \
     -H "Content-Type: application/json" \
     -d '{"exchange":"ORDER","routing_key":"NEW","data":{"symbol":"USDKRW","qty":1000}}' \
     http://localhost:8080/v1/tx

# Refresh — 새 access + refresh 페어
curl -X POST -H "Content-Type: application/json" \
     -d "{\"refresh_token\":\"$REFRESH\"}" \
     http://localhost:8080/v1/refresh

# Logout — 세션 + refresh 모두 무효화
curl -X POST -H "Authorization: Bearer $ACCESS" \
     http://localhost:8080/v1/logout
```

### 3.4 generic transaction (passthrough)

매매 엔진의 echo 서비스 (`echo_svc`) 가 떠 있다면:
```bash
curl -X POST -H "Authorization: Bearer $ACCESS" \
     -H "Content-Type: application/json" \
     -d '{"exchange":"ECHO","routing_key":"PING","data":"Hello WTG"}' \
     http://localhost:8080/v1/tx
# → 매매 엔진이 그대로 반사, ckey 매칭 후 reply 도착
```

### 3.5 admin broker 명령

```bash
TOKEN=...  # admin 로그인으로 받은 access_token

curl -H "Authorization: Bearer $TOKEN" \
     http://localhost:9090/v1/admin/status   # broker status
curl -H "Authorization: Bearer $TOKEN" \
     http://localhost:9090/v1/admin/clients  # 활성 클라이언트
curl -H "Authorization: Bearer $TOKEN" \
     "http://localhost:9090/v1/admin/whois?usid=trader01"
```

UI 의 **브로커 명령** 화면에서 같은 결과 확인 가능.

---

## 4. 다중 인스턴스 — etcd 동기화

라우팅 룰 / 정책이 admin → api 즉시 전파되는지.

```bash
# etcd 띄우기 (Docker)
docker run --rm -d -p 2379:2379 --name wtg-etcd \
  quay.io/coreos/etcd:v3.6.11 \
  /usr/local/bin/etcd \
  --advertise-client-urls http://0.0.0.0:2379 \
  --listen-client-urls http://0.0.0.0:2379

# admin / api 양쪽 같은 etcd
./build/bin/mci-admin --listen :9090 --etcd 127.0.0.1:2379 \
  --broker-host 127.0.0.1 --broker-port 11217

./build/bin/mci-api   --listen :8080 --etcd 127.0.0.1:2379 \
  --broker-host 127.0.0.1 --broker-port 11217
```

검증:
1. UI 에서 **라우팅 룰** → 신규 alias `ORDER_NEW` 등록
2. mci-api 측에서 즉시 사용 가능:
   ```bash
   curl -X POST -H "Authorization: Bearer $ACCESS" \
        -H "Content-Type: application/json" \
        -d '{"alias":"ORDER_NEW","data":{"symbol":"USDKRW"}}' \
        http://localhost:8080/v1/tx
   ```
3. UI 의 **정책 엔진** → Kill Switch 활성화 → mci-api 의 `/v1/tx` 즉시 503 (kill_switch)

---

## 5. WS 라이브 모니터링

### 5.1 시세 fan-out (mci-price → mci-edge-price)

```bash
# Internal 시세 collator
./build/bin/mci-price --listen :8082 --grpc :50051 \
  --broker-host 127.0.0.1 --broker-port 11217

# DMZ ws fan-out
./build/bin/mci-edge-price --listen :8083 \
  --upstream 127.0.0.1:50051 --dev
```

UI → **WS 모니터** → 프리셋 `edge-price (8083)` → 연결 → broker 로 시세
brodacast 가 흐르면 실시간 표시.

### 5.2 사용자별 push (mci-push → mci-edge-push)

```bash
./build/bin/mci-push      --listen :8081 --grpc :50052 --queue mci_push \
  --broker-host 127.0.0.1 --broker-port 11217

./build/bin/mci-edge-push --listen :8084 \
  --upstream 127.0.0.1:50052 --dev
```

UI WS 모니터 프리셋 `edge-push (8084)` 에 사용자 토큰 첨부 후 연결 → 그
사용자 logon_id 로 보내진 push 메시지만 수신.

---

## 6. 보안 회귀 시나리오

### 6.1 인증 우회 시도 — 모두 401 이어야

```bash
# 토큰 없음
curl -i http://localhost:8080/v1/tx          # 401
# 잘못된 JWT
curl -i -H "Authorization: Bearer xxx" http://localhost:8080/v1/tx  # 401
# 만료된 JWT (15분 후)
sleep 16m && curl -i -H "Authorization: Bearer $ACCESS" \
     http://localhost:8080/v1/tx              # 401 — refresh 또는 재로그인 필요
```

### 6.2 Edge → API 헤더 위조 시도 (mci-edge-api 통과 시)

```bash
# 외부에서 X-WTG-SID 직접 주입 → edge 가 strip 후 자기 검증값 주입
curl -i -H "X-WTG-SID: FORGED" -H "X-WTG-User: FORGED" \
     -H "Authorization: Bearer $ACCESS" \
     http://localhost:8090/v1/tx
# → upstream (mci-api) 은 edge 가 검증한 SID 만 봄, FORGED 는 무시됨
```

### 6.3 정책 차단 회귀

```bash
# UI 에서 차단 심볼 USDKRW 추가
# 그 다음 API 호출
curl -X POST -H "Authorization: Bearer $ACCESS" \
     -H "Content-Type: application/json" \
     -d '{"routing_key":"NEW","data":{"symbol":"USDKRW"}}' \
     http://localhost:8080/v1/tx
# → 403 blocked_symbol

# UI 에서 차단 해제 후
curl -X POST ...   # 다시 정상 응답
```

### 6.4 mTLS 검증

```bash
# 자체발급 인증서로 round-trip — pkg/tlsutil 의 e2e 테스트로 자동 검증
go test -run TestReloaderMTLSRoundTrip ./pkg/tlsutil/...
go test -run TestGRPCMTLS ./internal/edge/price/...
go test -run TestPushGRPCMTLS ./internal/edge/push/...
```

---

## 7. mTLS 인증서 회전 시나리오

```bash
# 1) 인증서 1차 발급 + admin 띄우기
TLSDIR=/tmp/wtg-tls && mkdir -p $TLSDIR
# ... cert v1 발급 ...
./build/bin/mci-admin --listen :9090 \
  --tls-cert $TLSDIR/server.crt --tls-key $TLSDIR/server.key &

# 2) cert 갱신 (cert-manager 시뮬레이션)
# ... cert v2 발급, 같은 경로에 덮어쓰기 ...

# 3a) SIGHUP 으로 즉시 reload
kill -HUP $(pgrep mci-admin)

# 3b) 또는 30s polling 으로 자동 reload (mtime 변경 감지)
sleep 35

# 4) 검증
echo | openssl s_client -connect localhost:9090 -showcerts 2>/dev/null \
     | openssl x509 -noout -subject -dates
# → 새 cert 의 Subject / NotBefore 가 보여야
```

진행 중 연결은 끊기지 않음 (zero-downtime cert rotation).

---

## 8. 트러블슈팅

| 증상 | 원인 / 해결 |
|------|----------|
| `mymq.Open: dial: connection refused` | broker 미기동. `lsof -i :11217` 확인. UI 만 보려면 `--no-broker` |
| 401 unauthorized 계속 | DevMode 미활성 + 토큰 없음. `--dev` 또는 `/v1/login` |
| 라우팅 룰 등록 후 mci-api 가 못 봄 | etcd 미사용 + 별도 in-memory. `--etcd 127.0.0.1:2379` 양쪽 |
| ws stream 연결 안 됨 | DevMode 는 헤더 인증 — ws 가 헤더 못 보냄. JWT 모드 (`/v1/login`) 필요 |
| `make test` flaky `TestClientCloseFailsPending` | 알려진 race, 단독 실행 시 통과. 무시 |
| go.sum 충돌 / etcd 버전 mismatch | `go mod tidy` 후 `make build` |
| UI 가 캐시된 구 버전 | 브라우저 강제 reload (`Cmd+Shift+R`) — index.html 은 단일 파일, 매번 fresh |

---

## 9. 정리 / 종료

```bash
# 모든 WTG 서비스
pkill -f 'build/bin/mci-' 2>/dev/null

# Docker (broker / etcd)
docker rm -f mymqd wtg-etcd 2>/dev/null

# 임시 인증서 / 데이터
rm -rf /tmp/wtg-tls
```

---

## 10. 빠른 시나리오 요약

| 목적 | 시간 | 명령 |
|------|------|-----|
| 코드 회귀 검증 | 30초 | `make test` |
| 통합 테스트 | 1분 | `make test-integration` |
| UI 시각 확인 (broker X) | 5분 | `./build/bin/mci-admin --dev --no-broker --listen :9090 && open http://localhost:9090` |
| broker e2e | 10~15분 | §3.1~3.5 따라가기 |
| 다중 인스턴스 동기화 | 15분 | §4 |
| 보안 회귀 | 5분 | §6.1~6.3 |
| mTLS 회전 | 5분 | §7 |

---

## 11. 추가 참고

- `docs/auth.md` — 인증 / 권한 위임 설계
- `docs/broker-tls.md` — broker TLS 운영팀 협의 명세
- `docs/conventions.md` — passthrough 패턴 / 위임 원칙
- `docs/roadmap.md` — Phase 별 계획
- `internal/admin/ui/index.html` — UI 단일 파일 SPA (수정 시 `make build` 로 embed 갱신)
