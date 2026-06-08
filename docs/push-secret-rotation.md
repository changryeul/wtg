# mci-push 인증 운영 — secret rotate + 향후 mTLS 전환

## 현재 운영 모드 (2026-06 기준)

WTG mci-push 는 **secret-only 인증** 으로 운영. mTLS 구현 (Phase 2.4) 은
코드에 존재하지만 활성하지 않음.

### 결정 근거

| 조건 | 현재 WTG | 판정 |
|---|---|---|
| 네트워크 위치 | Internal 망 (DMZ 차단) | secret 으로 충분 |
| 운영 svc 수 | 5개 미만 | per-svc cert 운영 cost > 이득 |
| compliance 요구 | 없음 (사내) | mTLS 즉시 도입 불필요 |
| PKI 인프라 | 미정 | mTLS 운영 cost ↑ |
| **결정** | | **secret-only + Grafana audit** |

mTLS 는 다음 조건 만족 시 전환:
- 운영 svc 10개+ 도달
- compliance 요구 발생 (PCI-DSS / ISO 27001 등)
- 사내 PKI 인프라 구축
- 외부망 노출 시나리오 발생

## Secret 운영

### 생성

```bash
openssl rand -hex 32
# 예: 4a8f2c1b9e7d6a5c8b3f1d2e0a4b6c9e7f1a3d5b9c2e8f4a6d1b3c7e9f2a4d8b
```

64자 hex (256-bit entropy). brute-force 사실상 불가.

### 저장 (운영자 선택)

#### A. 환경변수 + systemd EnvironmentFile (가장 단순)

```bash
# /etc/wtg/push.env (root:root 0600)
WTG_PUSH_SECRET=4a8f2c1b9e7d6a5c8b3f1d2e0a4b6c9e7f1a3d5b9c2e8f4a6d1b3c7e9f2a4d8b
```

```ini
# /etc/systemd/system/mci-push.service
[Service]
EnvironmentFile=/etc/wtg/push.env
ExecStart=/opt/wtg/bin/mci-push --listen=:8081 ...
```

운영 svc 측도 동일 파일을 read (또는 별도 push-client.env).

#### B. HashiCorp Vault (사내 운영 표준이면)

```bash
# write
vault kv put secret/wtg/push secret=$(openssl rand -hex 32)

# read at runtime (systemd ExecStartPre 또는 wrapper script)
export WTG_PUSH_SECRET=$(vault kv get -field=secret secret/wtg/push)
exec mci-push --listen=:8081 ...
```

#### C. k8s Secret (컨테이너 환경)

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: wtg-push-secret
stringData:
  secret: 4a8f2c1b9e...
---
apiVersion: apps/v1
kind: Deployment
spec:
  template:
    spec:
      containers:
        - name: mci-push
          env:
            - name: WTG_PUSH_SECRET
              valueFrom:
                secretKeyRef:
                  name: wtg-push-secret
                  key: secret
```

운영 svc Deployment 도 같은 secret 참조.

## Rotate 절차

### 정기 rotate (6개월 권장)

**다운타임 없이 rotate 하려면 mci-push 는 이중 secret 을 못 받음** (현재 구현
은 단일 secret). 두 가지 path:

#### 옵션 1. 짧은 downtime (~10초) — 단순

```bash
# 1. 새 secret 생성 + 운영 svc 측 env 먼저 업데이트 + reload
NEW=$(openssl rand -hex 32)
# 운영 svc 들의 push secret env 갱신 → 운영 svc reload (signal)
#  → 이 순간 운영 svc 는 새 secret 으로 호출, mci-push 는 옛 secret 검증 → 401
# 2. mci-push 측 env 즉시 업데이트 + restart (10초 내 완료)
echo "WTG_PUSH_SECRET=$NEW" | sudo tee /etc/wtg/push.env
sudo systemctl restart mci-push

# 3. 운영 svc 의 push 재시도 (자동 backoff) — 30초 내 정상화
```

10초 downtime 동안 push 가 쌓이면:
- 운영 svc 의 retry buffer 에 보관 (구현 따라 다름)
- 또는 broker subscribe 가 백업 path 로 동작 (Phase 2.6 완료 전이면 OK)

#### 옵션 2. 무중단 — mci-push 측 이중 secret 지원 코드 변경 (별도 작업)

```go
// HTTPPushDeps 에 secret 2개 받는 변형 추가:
//   - Secret (현재)
//   - SecretPrev (이전 — rotate window 동안만 채움)
// 검증: 둘 중 하나라도 일치하면 통과.
```

작업량 ~30분. 필요 시 별도 PR.

### 비상 rotate (보안 사고)

secret 유출 의심 시 즉시:

```bash
# 1. 사용 중단 — mci-push 의 PUSH secret 을 무작위 garbage 로 갱신
echo "WTG_PUSH_SECRET=__INVALIDATED_$(date +%s)__" | sudo tee /etc/wtg/push.env
sudo systemctl restart mci-push
# → 이 순간부터 모든 HTTP push 401 (정상 + 침입자 둘 다 차단)

# 2. broker subscribe 가 backup path 면 push 자체는 계속 동작
#    (Phase 2.6 완료 전이면 자동 fallback)
#    Phase 2.6 완료 후엔 운영 svc 가 backup 없음 — 빨리 단계 3 진행

# 3. 새 secret 발행 + 운영 svc + mci-push 동시 갱신
NEW=$(openssl rand -hex 32)
# Vault / k8s Secret / EnvironmentFile 각 위치 일괄 갱신
# 운영 svc + mci-push 동시 restart

# 4. audit log 분석
grep "push: mTLS client" /var/log/mci-push.log  # mTLS 안 켰으므로 빈값
grep "unauthorized" /var/log/mci-push.log       # 401 호출 source IP 확인
```

**현재 secret-only 운영에선 audit 정보가 제한적** — IP 만 남음. 보안 사고
대응이 필요해지면 그 시점에 mTLS 전환 (다음 절).

## 향후 mTLS 전환 시 마이그레이션

Phase 2.4 의 mTLS 코드는 이미 동작. 켜는 절차:

### 1. 사내 PKI 발급 (또는 자체 CA)

```bash
# 사내 CA 가 있으면 거기서 발급. 없으면 cfssl/step-ca 등.
# 운영 svc 별 cert (CN = svc 이름):
#   order-engine-prod.crt / .key
#   trade-confirm-prod.crt / .key
#   ...
# mci-push server cert:
#   mci-push.internal.crt / .key
```

### 2. mci-push 활성 — secret + mTLS 병행 (이중 검증)

```bash
mci-push --listen=:8081 \
  --push-secret=$WTG_PUSH_SECRET \
  --http-tls-cert=/etc/wtg/certs/server.crt \
  --http-tls-key=/etc/wtg/certs/server.key \
  --http-tls-client-ca=/etc/wtg/certs/svc-ca.crt
# 둘 다 통과해야 OK (defense-in-depth)
```

### 3. 운영 svc 전환 (svc 단위 점진)

```go
push.MustNewClient(push.ClientOptions{
    BaseURL:           "https://mci-push.internal:8081",
    Secret:            os.Getenv("WTG_PUSH_SECRET"), // 유지
    TLSClientCertFile: "/etc/wtg/certs/svc.crt",
    TLSClientKeyFile:  "/etc/wtg/certs/svc.key",
    TLSServerCAFile:   "/etc/wtg/certs/ca.crt",
    TLSServerName:     "mci-push.internal",
})
```

각 운영 svc 가 mTLS 로 호출 시작하면 `mci_push_http_inject_total{cn="..."}`
에 CN 등장 — Grafana 대시보드의 "Inject rate by CN" 패널에서 실시간 확인.

### 4. 전환 완료 후 secret 제거 (선택)

```bash
mci-push --listen=:8081 \
  --http-tls-cert=... --http-tls-key=... --http-tls-client-ca=...
  # --push-secret 제거 → mTLS only
```

운영 svc 측도 `Secret` 옵션 제거.

## 운영 체크리스트

- [ ] secret 64자 이상 (`openssl rand -hex 32`)
- [ ] secret 파일 permission `0600` (root:root)
- [ ] secret 이 git 에 commit 안 됨 (`.gitignore` + `git secret` 검토)
- [ ] secret 6개월마다 rotate 일정 (캘린더 등록)
- [ ] Grafana `PushAuthFailureSurge` alert 활성 (Phase 2.5)
- [ ] 운영 svc 측 push 호출에 retry + backoff (rotate 다운타임 대응)
- [ ] 비상 rotate runbook 운영 wiki 에 명시

## 관련 문서

- `docs/push-monitoring.md` — Grafana / Prometheus 모니터링
- `docs/operations.md` — 서비스별 flag/env 카탈로그
- `pkg/push/client.go` — Go SDK (TLS 옵션 코드 path)
- `internal/push/http_push.go` — 서버 측 인증 코드 (secret + mTLS 두 path)
