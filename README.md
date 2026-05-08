# Winway Trading Gateway (WTG)

웹 기반 외환 트레이딩 게이트웨이.
주문/체결/시세/푸시 기능을 web/FIX/CS 채널로 동시에 노출하면서, 기존 C 기반
MyMQ 엔진(`/Users/winwaysystems/mywork/mymq`)을 그대로 사용한다.

핵심 설계 원칙:

- **기존 C 엔진 무수정** — 외환 트랜잭션 처리는 검증된 MyMQ broker(`mymqd`) 그대로
- **DMZ ↔ Internal 분리** — 외부 고객은 DMZ 프록시(`mci-edge-*`)만, 내부 직원은 사내망 직접
- **순수 Go 클라이언트** — `libmymq-go` 신규 구현, cgo 회피
- **단일 connection 멀티플렉싱** — `mqhdr.ckey` 필드를 correlation_id로 활용

## 디렉토리 구조

```
wtg/
├── go.mod                        # 단일 모듈 (MyMQ src/ 와 동일한 monorepo 철학)
├── Makefile
├── docs/
│   ├── phase0-analysis.md        # MyMQ wire protocol 분석 + 설계 결정
│   ├── roadmap.md                # 9-Phase 구현 로드맵 (~22주)
│   └── cooker-patch.md           # 옵션 A-1 cooker C 패치 명세
├── pkg/                          # 공유 라이브러리 (= mymq/src/lib/ 와 대응)
│   ├── mymq/                     # libmymq-go: MyMQ 브로커 클라이언트
│   ├── proto/, auth/, session/, metrics/, config/, log/   (TBD)
├── cmd/                          # 서비스 entrypoint (= mymq/src/mqd, src/rqd, ...)
│   ├── mci-test/                 # Phase 1 검증 CLI (ckey echo)
│   ├── mci-price/                # FX 시세 fan-out 프로토타입
│   ├── mci-api/                  # REST + sync RPC          (Phase 2)
│   ├── mci-push/                 # WebSocket fan-out         (Phase 3)
│   ├── mci-admin/                # 관리/Control plane         (Phase 6)
│   └── mci-edge-{api,push,price} # DMZ 프록시 3종            (Phase 5)
├── internal/                     # 서비스별 비즈니스 로직
├── etc/                          # 설정 (= mymq/etc/)
├── scripts/, deploy/, test/integration/
```

## 빌드 / 테스트

```bash
make build         # cmd/<svc>/*.go 가 있는 서비스 자동 빌드
make test          # 단위 테스트
make test-v        # verbose
make test-race     # race detector + coverage
make coverage      # coverage.html 생성
make lint          # fmt-check + vet + staticcheck
make vulncheck     # govulncheck (의존성 CVE)
make ci            # CI 와 동일한 전체 검증 (commit/PR 전에 권장)
make ckey-echo     # GO/NO-GO 검증 (mymqd 가 ckey 를 echo 하는지)
```

`cmd/<service>/main.go` 가 추가되면 자동으로 빌드 대상에 포함된다.

GitHub Actions(.github/workflows/ci.yml) 가 PR/main push 마다 동일한 검증을
수행하며, Dependabot 이 매주 의존성 업데이트 PR 을 생성한다.

## 통합 테스트 (실 mymqd 연동)

```bash
MYMQD_HOST=10.0.0.10 MYMQD_PORT=11217 \
  go test -v ./test/integration/...
```

`MYMQD_HOST` 미설정 시 자동 skip — CI 는 mymqd 없이도 green 유지.

## 설계 결정 요약

| 영역 | 결정 |
|-----|-----|
| 통합 방식 | Go-native libmymq-go (cgo 회피) |
| Wire 호환성 | `mymq.h` / `mq.h` 기반 84바이트 mqhdr + navi[] + 가변영역 |
| Endianness | Big-endian (network byte order) |
| Framing | Length-prefixed (4 bytes BE) |
| Heartbeat | 4바이트 빈 프레임 |
| Multiplex | `mqhdr.ckey` 를 correlation_id 로 활용 |
| 시세 (mci-price) | 옵션 A-1: Cooker 가 myrqd + mymqd 양쪽 publish |
| 모듈 구조 | monorepo + 단일 go.mod (MyMQ 와 동일 철학) |

자세한 분석은 [`docs/phase0-analysis.md`](docs/phase0-analysis.md) 참조.
전체 구현 계획은 [`docs/roadmap.md`](docs/roadmap.md) 참조.
Cooker 패치 명세는 [`docs/cooker-patch.md`](docs/cooker-patch.md) 참조.
명명 컨벤션은 [`docs/conventions.md`](docs/conventions.md) 참조.
인증/권한 명세는 [`docs/auth.md`](docs/auth.md) 참조.

## 컴포넌트 책임

| 컴포넌트 | 위치 | 역할 |
|---------|-----|-----|
| `mci-api` | Internal | REST/HTTP, sync RPC. mymqd 로 `mymq_call` |
| `mci-push` | Internal | unsolicited 모드 mymqd 클라이언트, 체결/주문상태/알림 fan-out |
| `mci-price` | Internal | unsolicited 모드, FX 시세 fan-out (broker 경유) |
| `mci-admin` | Internal | 직원용 관리 UI/API (사내망 전용) |
| `mci-edge-api` | DMZ | TLS termination + JWT 검증 + REST 프록시 |
| `mci-edge-push` | DMZ | WebSocket 게이트웨이 |
| `mci-edge-price` | DMZ | 시세 fan-out edge (대량 처리) |

## Go 설치

이 환경에 Go 가 미설치 상태라면:

```bash
brew install go    # macOS
# 또는
curl -L https://go.dev/dl/go1.23.0.darwin-arm64.tar.gz | sudo tar -C /usr/local -xzf -
export PATH=/usr/local/go/bin:$PATH
```

Go 1.23+ 권장 (모듈 명세 기준).

## 라이선스

Promentor Co., Ltd. — 내부용. 외부 배포 금지.
