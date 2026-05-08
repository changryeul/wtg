# MyMQ broker TLS 통합 명세

WTG 와 MyMQ broker (`mymqd`) 간 TCP 연결의 TLS 화 — 운영팀/MyMQ 엔지니어 협의용.

마지막 갱신: 2026-05-02
결정 상태: **WTG 클라이언트 측 옵션 완료, broker 측 변경 협의 대기**

---

## 1. 배경

WTG 의 모든 Internal 서비스 (`mci-api`, `mci-admin`, `mci-push`, `mci-price`) 는
`pkg/mymq.Open` 로 broker 와 TCP 연결을 맺는다. 현재 이 구간은 **plain TCP** —
다른 Internal/DMZ 통신 (gRPC, HTTPS) 은 mTLS 가 적용되었으나 broker TCP 만
유일한 미적용 구간.

운영 환경에서 broker 와 WTG 가 같은 노드/같은 네임스페이스라면 위험은 낮지만,
**LAN 분리 환경 / 다중 호스트 / regulatory** 요구사항에서 broker TCP 도 TLS
보호가 필요하다.

---

## 2. 책임 분담

| 항목 | WTG 측 | broker 측 |
|-----|-------|-----------|
| TLS 클라이언트 dial (tls.Dial) | ✅ 완료 | n/a |
| 클라이언트 인증서 (mTLS) 보유 | ✅ Config 옵션 완료 | n/a |
| 서버 인증서 검증 (CA bundle) | ✅ Config 옵션 완료 | n/a |
| **TLS listener 노출** | n/a | ⏳ **변경 필요** |
| **mTLS 정책 (클라이언트 cert 요구)** | n/a | ⏳ **변경 필요** |
| 인증서 회전 | ✅ pkg/tlsutil/Reloader (HTTP/gRPC), 클라이언트 측은 재시작 또는 reconnect 시 자동 적용 | ⏳ broker 정책 |

---

## 3. WTG 측 (이미 완료)

### 3.1 라이브러리

`pkg/mymq.Options` 에 `TLS *tls.Config` 필드 추가.

```go
opts := mymq.Options{
    ApplName: "mci-api",
    TLS:      tlsCfg, // pkg/tlsutil.LoadClient(...) 결과
    // ...
}
mymq.Open(ctx, host, port, opts)
```

`Open` 과 `tryReconnect` 가 공유하는 `dialBroker` 헬퍼가 `TLS != nil` 일 때
`tls.Dial` 사용. 재연결 시에도 동일 `*tls.Config` 재사용 → 인증서 갱신은
호출자가 별도 처리.

### 3.2 서비스 Config

`mci-api`, `mci-admin`, `mci-push`, `mci-price` 모두 동일 옵션:

| Flag | env | 설명 |
|------|-----|------|
| `--broker-tls-cert` | `WTG_<SVC>_BROKER_TLS_CERT` | 클라이언트 cert PEM |
| `--broker-tls-key`  | `WTG_<SVC>_BROKER_TLS_KEY`  | 클라이언트 key PEM |
| `--broker-tls-ca`   | `WTG_<SVC>_BROKER_TLS_CA`   | broker 서버 검증용 CA |
| `--broker-tls-sni`  | `WTG_<SVC>_BROKER_TLS_SNI`  | TLS SNI / hostname |

`<SVC>` ∈ `{API, ADMIN, PUSH, PRICE}`. cert/key 또는 CA 둘 중 하나라도 채워지면
TLS 활성. 비어있으면 기존 plain TCP (호환 모드).

### 3.3 운영 예시

```bash
mci-api \
  --broker-host mymq-prod \
  --broker-port 11217 \
  --broker-tls-cert /etc/wtg/tls/client.crt \
  --broker-tls-key  /etc/wtg/tls/client.key \
  --broker-tls-ca   /etc/wtg/tls/broker-ca.pem \
  --broker-tls-sni  mymq-prod.internal
```

---

## 4. broker 측 (협의 필요)

`mymqd` 는 현재 `bind()` + `listen()` 후 평문 TCP `accept()` 만. TLS 추가에는
다음 옵션이 있다.

### 4.1 옵션 A — `mymqd` 자체에 OpenSSL 통합

장점:
- 단일 프로세스, 추가 hop 없음.
- `mymqd.cfg` 에서 cert/key 경로 관리 가능.

단점:
- C 코드 변경 (OpenSSL 의존성 추가, accept 루프 + read/write 분기).
- 모든 broker 인스턴스에 cert 배포 필요.

작업 항목:
- [ ] `mymq_listen.c` 에 `SSL_CTX` 초기화
- [ ] `accept()` 후 `SSL_accept()` 호출
- [ ] read/write 매크로 분기 (TLS 활성 시 `SSL_read`/`SSL_write`)
- [ ] `mymqd.cfg` 에 `tls_cert_file` / `tls_key_file` / `tls_client_ca` / `tls_min_version`
- [ ] 인증서 회전 (SIGHUP 시 `SSL_CTX` 재로딩)

### 4.2 옵션 B — `stunnel` 또는 `nginx stream` 프록시 (권장)

장점:
- broker C 코드 무수정.
- TLS 종단 분리 — 인증서 회전, mTLS 정책, 로깅이 stunnel 측에서 표준 도구로.
- 단계적 도입: stunnel 만 먼저 띄우고 broker 는 그대로 plain.

단점:
- 추가 프로세스 (네트워크 hop 1단계 추가, latency ~100µs ~ 1ms).
- stunnel 설정 관리 필요.

토폴로지:
```
[WTG 클라이언트]
   │ tls.Dial → 11217 (TLS)
   ▼
[stunnel] (mci 노드 또는 별도 sidecar)
   │ plain TCP → 11218 (loopback only)
   ▼
[mymqd]
```

작업 항목:
- [ ] `stunnel.conf` 작성 (cert/key/CA, accept=11217, connect=127.0.0.1:11218)
- [ ] systemd unit / k8s sidecar 컨테이너 정의
- [ ] mymqd 의 listen 포트를 loopback only 로 제한 (다른 클라이언트 차단)
- [ ] WTG 가 stunnel 의 11217 로 TLS dial — 위 §3.3 옵션 그대로

### 4.3 옵션 C — k8s mTLS service mesh (Istio / Linkerd)

장점:
- 자동 mTLS — 인증서 발급/회전 자동.
- 옵션 B 의 stunnel 을 service mesh proxy 가 대신.

단점:
- service mesh 도입 비용 (운영 복잡도 ↑).
- WTG 와 mymqd 모두 k8s pod 안에 있어야.

옵션 B 의 일반화 — 대규모 운영에서 표준이지만 도입은 별도 결정.

---

## 5. 권장 — 옵션 B (stunnel)

운영팀 합의 시 권장 경로:

1. **즉시**: stunnel 사이드카 1개 띄우고 broker 한 인스턴스에 적용
2. **검증**: WTG 한 서비스 (예: mci-admin) 만 TLS 옵션 켜서 round-trip 확인
3. **롤아웃**: 모든 mymqd 인스턴스 + 모든 WTG 서비스에 적용
4. **mTLS 강화**: stunnel 의 `verify=2` 로 클라이언트 cert 강제 + WTG 가 발급 받은 cert 사용

mymqd C 코드는 무수정 → 운영 위험 최소.

---

## 6. 인증서 발급/배포

WTG 다른 mTLS 와 동일 PKI 재사용:

- **CA**: 사내 CA 또는 cert-manager + Let's Encrypt
- **client cert**: 서비스별 (mci-api/mci-admin/mci-push/mci-price 각각)
- **server cert (broker)**: `mymq-prod.internal` 등 SAN 포함
- **회전**: cert-manager 가 파일 갱신 시
  - WTG 측: `pkg/tlsutil.Reloader` 가 자동 reload (HTTP/gRPC). broker dial 은
    `*tls.Config` 가 동일 객체 참조라 즉시 새 cert 사용.
  - broker 측 (stunnel): `systemctl reload stunnel`

---

## 7. 보안 정책 권장

- TLS 1.2 minimum (운영 합의 시 1.3 강제 가능)
- mTLS — 클라이언트 cert 검증 강제 (`stunnel verify=2`)
- cipher suites: 운영팀 표준 따름 (Mozilla "intermediate" 정도 권장)
- 키 길이: RSA 2048+ 또는 ECDSA P-256+

---

## 8. 검증 시나리오

운영 합의 후 단계적 검증:

```bash
# 1) WTG → stunnel (TLS) → mymqd (plain). 단일 서비스부터.
mci-admin --broker-tls-cert ./client.crt --broker-tls-key ./client.key \
          --broker-tls-ca ./ca.pem --broker-tls-sni mymq-prod \
          --broker-host mymq-prod --broker-port 11217

# 2) WTG 응답 확인 (handshake 성공 + LOGON / admin cmd round-trip)
curl https://admin.example.internal/v1/admin/status

# 3) WireShark / tcpdump 로 broker 포트 트래픽 → 암호화 확인
sudo tcpdump -i eth0 'port 11217' -A | head
# → TLS 헤더 (0x16 0x03) 외에는 평문 노출 없어야

# 4) mTLS 강제 확인 — cert 없이 dial 시도
openssl s_client -connect mymq-prod:11217 -showcerts
# → "no certificate or crl found" / handshake failure
```

---

## 9. 다음 단계 (운영팀 결정 필요)

- [ ] 옵션 A/B/C 중 선택
- [ ] PKI: 사내 CA 사용 vs cert-manager
- [ ] 클라이언트 cert 주체 (서비스별 vs 공통)
- [ ] 회전 정책 (90일 / 1년 / 자동)
- [ ] 단계 롤아웃 일정 (검증 → 점진 → 전체)
- [ ] 변경 감사 — 인증서 발급/폐기 로그 보관 정책
