# 운영 배포 체크리스트 — Linux 서버에 WTG 띄우기

> 본 문서는 운영 Linux 서버에 직접 실행할 명령 모음이다. 한 줄씩 따라가면 부팅까지.
> 사전 검증 (dev 환경) 완료된 unit / env / flag 를 기반으로 한다.

---

## 0. 사전 준비 (배포 30분 전)

| 항목 | 확인 |
|---|---|
| □ etcd cluster 3-node 부팅 + quorum OK | `etcdctl --endpoints=... endpoint health` |
| □ Redis Sentinel master 살아있음 | `redis-cli -p 26379 sentinel masters` |
| □ TimescaleDB primary 부팅 + schema 적용 | `psql -d wtg -c '\d quote_bars'` |
| □ broker (mymqd) active 부팅 + ckey echo OK | `mci-test --ckey-echo` |
| □ TLS cert 만료까지 60일+ | `openssl x509 -in /etc/pki/wtg/int.crt -noout -enddate` |
| □ Prometheus / Grafana / OTel collector 부팅 | Prometheus `:9091/targets`, Grafana `/login` |

위 6가지 중 하나라도 NO 면 배포 중단 + 인프라 팀에 alert.

---

## 1. 운영 사용자 / 디렉토리 생성

```bash
sudo useradd --system --no-create-home --shell /usr/sbin/nologin wtg
sudo mkdir -p /opt/wtg/{bin,etc,share/doc}
sudo mkdir -p /etc/wtg
sudo mkdir -p /var/log/wtg /var/lib/wtg
sudo chown -R wtg:wtg /var/log/wtg /var/lib/wtg
sudo chmod 750 /etc/wtg
```

---

## 2. 바이너리 + 설정 배포

빌드 머신에서 :
```bash
# 빌드 (Go 1.23+ 필요)
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 make build

# 운영 서버로 전송 (예시 — 환경 맞게 수정)
scp -r build/bin/ wtg@int1.internal:/tmp/wtg-bin/
scp -r etc/      wtg@int1.internal:/tmp/wtg-etc/
scp -r deploy/systemd/ wtg@int1.internal:/tmp/wtg-systemd/
```

운영 서버에서 :
```bash
sudo cp /tmp/wtg-bin/* /opt/wtg/bin/
sudo cp -r /tmp/wtg-etc/* /opt/wtg/etc/
sudo chown -R wtg:wtg /opt/wtg/{bin,etc}
sudo chmod 755 /opt/wtg/bin/*
```

---

## 3. 환경 파일 작성 (`/etc/wtg/wtg.env`)

```bash
sudo cp /tmp/wtg-systemd/wtg.env.sample /etc/wtg/wtg.env
sudo chown root:wtg /etc/wtg/wtg.env
sudo chmod 0640 /etc/wtg/wtg.env
sudo vi /etc/wtg/wtg.env
```

본 환경의 실제 endpoint 로 채울 항목 :

```
WTG_ETCD=https://etcd1.<env>.internal:2379,...
WTG_BROKER_HOST=broker-vip.<env>.internal    ← VIP 또는 DNS round-robin
WTG_BROKER_PORT=11217
WTG_QUOTEID_REDIS=redis1.<env>.internal:26379,...
WTG_QUOTEID_INSTANCE=int1                     ← int2 호스트는 int2
WTG_OTEL_ENDPOINT=otel-col.<env>.internal:4317
WTG_PROM_URL=http://prom.<env>.internal:9091
WTG_GRAFANA_URL=http://grafana.<env>.internal:3000
WTG_GRAFANA_USER=wtg-readonly
WTG_GRAFANA_PASS=<grafana read-only 사용자 password>
WTG_DSN=postgres://wtg:<password>@db1.<env>.internal/wtg?sslmode=require
```

> **시크릿** : env 파일의 password / DSN 은 mode 0640 보호. 운영 환경에선 Vault 같은 secret manager 도입 권장 — 그 경우 systemd `LoadCredential=` 패턴 사용.

---

## 4. systemd unit 배치 + 활성화

```bash
sudo cp /tmp/wtg-systemd/wtg-*.service /etc/systemd/system/
sudo systemctl daemon-reload

# 순서대로 활성화 (인프라 → 핵심 → DMZ)
sudo systemctl enable --now wtg-mci-price
sleep 5
sudo systemctl enable --now wtg-mci-edge-price
sleep 3
sudo systemctl enable --now wtg-mci-admin
```

---

## 5. 부팅 검증

```bash
# unit 상태
sudo systemctl status 'wtg-*' --no-pager

# 로그 — 부팅 30초 정도 따라가기
sudo journalctl -u wtg-mci-price -f &
sleep 30
kill %1
```

기대 신호 :
- `wtg-mci-price` : `gRPC listen 시작`, `BestConsumer 활성`, `PricingConsumer 활성`
- `wtg-mci-edge-price` : `PriceService Subscribe 시작`, `SubscribeQuote 시작`, `SubscribeCustomerQuote 시작`
- `wtg-mci-admin` : `HTTP listen 시작`, `EtcdRegistry 초기 로드`

오류 신호 :
- `connection refused` → endpoint 설정 오류 (env 재확인)
- `tls: certificate signed by unknown authority` → CA 경로 / chain 확인
- `flag provided but not defined: -X` → unit 의 flag 오타 (배포 전 검증 빠짐)

---

## 6. 기능 검증

```bash
# 1) HTTP /metrics 살아있는지
curl -sk https://int1.internal:8082/metrics | head -5
curl -sk https://dmz1.internal:8083/metrics | head -5
curl -sk https://int1.internal:9090/  -o /dev/null -w "%{http_code}\n"

# 2) Prometheus 가 새 target 을 scrape 시작했는지
curl -s http://prom.internal:9091/api/v1/targets | jq '.data.activeTargets[] | select(.scrapePool=="mci-price") | {url:.scrapeUrl,health:.health}'

# 3) admin UI 로 broker 연결 확인 (브라우저)
#    → 대시보드 broker 카드 ● 연결됨

# 4) 매매 1건 dry-run (test alias 권장)
curl -sk -X POST https://api.<domain>/v1/tx \
     -H "Authorization: Bearer <test_token>" \
     -d '{"alias":"WECHO_PING","data":""}'
#    → 200 + {"pong":true,...}
```

---

## 7. 사후 점검 (배포 30분 후)

```bash
# 카운터가 정상 누적되는지
sudo journalctl -u wtg-mci-price --since "30 min ago" | grep -E "received|matched|dropped" | tail -5

# Prometheus 카드들 (admin UI 의 운영 모니터링 페이지)
# → HTTP 5xx rate = 0, Broker disconnects = 0

# systemd 자동 재시작 동작 검증 (선택, 정비창에)
sudo systemctl kill wtg-mci-price
sleep 10
sudo systemctl status wtg-mci-price
# → 5초 후 자동 재시작, "active (running)"
```

---

## 8. 롤백 절차 (사고 시)

```bash
# 1. 즉시 거리 두기 — admin UI 의 🛡 정책 엔진 → Kill switch ON

# 2. 새 바이너리 stop
sudo systemctl stop 'wtg-*'

# 3. 백업된 이전 바이너리로 교체
sudo cp /opt/wtg/bin.backup/* /opt/wtg/bin/

# 4. 재시작
sudo systemctl start wtg-mci-price wtg-mci-edge-price wtg-mci-admin

# 5. 부팅 검증 (§5, §6 재실행)

# 6. Kill switch OFF
```

> **배포 전 반드시** `/opt/wtg/bin.backup/` 에 현재 바이너리 백업 :
> `sudo cp -r /opt/wtg/bin /opt/wtg/bin.backup-$(date +%Y%m%d)`

---

## 9. 다른 노드 배포 (HA 구성 시)

본 절차를 `int2.internal`, `dmz2.internal` 에 반복. 차이는 :
- `WTG_QUOTEID_INSTANCE=int2` (int2 호스트의 경우)
- TLS cert 의 SAN 에 호스트명 포함
- 같은 `/etc/wtg/wtg.env` (시크릿 동일)

순서 :
1. int2 배포 + 검증 (int1 살아있는 상태에서)
2. dmz1 → dmz2 순서

---

## 10. 검증된 flag / 변수 (dev 환경에서 사전 확인)

다음은 본 unit 파일의 flag 가 실제 binary 가 받는지 자동 검증한 결과 (2026-06-20 기준) :

| 서비스 | flag 개수 | binary 일치 |
|---|---|---|
| mci-price | 19 | 19 / 19 ✓ |
| mci-edge-price | 10 | 10 / 10 ✓ |
| mci-admin | 19 | 19 / 19 ✓ |
| **env 변수** | 18 | 18 / 18 ✓ |

→ unit / env 가 호환됨이 사전 검증된 상태. 단 운영 환경 endpoint / 시크릿은 사용자가 입력.

---

## 11. 참고

- `docs/operations-routine.md` — 운영자 일일 / 주간 / 사고 SOP
- `docs/deployment-software.md` — 모든 인프라 의존성
- `docs/deployment-scenario-ha-channel.md` — HA + 채널 분리 시나리오 §6 (서비스 flag)
- `docs/simplification-guide.md` — 단순화 의사결정
- `docs/admin-ui-manual.md` §12 — 운영 시나리오 7가지
- `deploy/systemd/README.md` — systemd unit 설치 절차 (요약)
