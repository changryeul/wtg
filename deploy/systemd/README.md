# WTG systemd unit 템플릿

운영 환경 (Linux) 에 WTG 를 배포할 때 사용하는 systemd unit 파일들.

## 설치 절차

```bash
# 1. 운영 사용자 / 디렉토리
sudo useradd --system --no-create-home --shell /usr/sbin/nologin wtg
sudo mkdir -p /opt/wtg/{bin,etc} /etc/wtg /var/log/wtg /var/lib/wtg
sudo chown -R wtg:wtg /var/log/wtg /var/lib/wtg

# 2. 바이너리 배포
sudo make install PREFIX=/opt/wtg   # build/bin/* + etc/* 복사

# 3. 환경 파일 작성 (시크릿 / 엔드포인트)
sudo cp deploy/systemd/wtg.env.sample /etc/wtg/wtg.env
sudo chmod 600 /etc/wtg/wtg.env
sudo vi /etc/wtg/wtg.env

# 4. unit 파일 배치
sudo cp deploy/systemd/wtg-*.service /etc/systemd/system/

# 5. enable + start
sudo systemctl daemon-reload
sudo systemctl enable --now wtg-mci-price wtg-mci-edge-price wtg-mci-admin

# 6. 상태 확인
sudo systemctl status 'wtg-*'
sudo journalctl -u wtg-mci-price -f
```

## 표준 정책 (모든 unit 공통)

- `Restart=on-failure` + `RestartSec=5`
- `StartLimitBurst=5` / `StartLimitIntervalSec=60` — 1분 안 5회 실패하면 disable (무한 재시작 방지)
- `TimeoutStopSec=30` — graceful shutdown 30초
- `User=wtg` / `Group=wtg`
- `EnvironmentFile=/etc/wtg/wtg.env` — 환경별 설정 한 곳
- `LimitNOFILE=65536` — ws / 소켓 충분히

## 환경 파일 (`/etc/wtg/wtg.env`)

```bash
# 공통
WTG_ENV=prod
WTG_ETCD=https://etcd-sl-1.internal:2379,https://etcd-sl-2.internal:2379,https://etcd-sl-3.internal:2379
WTG_ETCD_TLS_CA=/etc/pki/wtg/ca.crt
WTG_ETCD_TLS_CERT=/etc/pki/wtg/int.crt
WTG_ETCD_TLS_KEY=/etc/pki/wtg/int.key

# broker
WTG_BROKER=ap1.internal:11217,ap2.internal:11217
WTG_BROKER_TLS_CA=/etc/pki/wtg/ca.crt
WTG_BROKER_TLS_CERT=/etc/pki/wtg/int.crt
WTG_BROKER_TLS_KEY=/etc/pki/wtg/int.key

# Redis (세션용)
WTG_AUTH_REDIS=redis-sl-1:26379,redis-sl-2:26379,redis-sl-3:26379
WTG_AUTH_REDIS_MASTER=wtg-auth-master

# Redis (quoteid Registry 용)
WTG_QUOTEID_REDIS=redis-sl-1:26379,redis-sl-2:26379,redis-sl-3:26379
WTG_QUOTEID_REDIS_MASTER=wtg-quoteid-master
WTG_QUOTEID_INSTANCE=int1   # int2 는 int2

# 관측
WTG_OTEL_ENDPOINT=otel-col.obs1.internal:4317
WTG_PROM_URL=http://prom.obs1.internal:9091
WTG_GRAFANA_URL=http://grafana.obs1.internal:3000
WTG_GRAFANA_USER=wtg-readonly
WTG_GRAFANA_PASS_FILE=/etc/pki/wtg/grafana-pass

# TimescaleDB
WTG_DSN=postgres://wtg:secret@db1.internal/wtg?sslmode=require

# 단순화 v3 — 본 환경에서 끄는 feature
WTG_ENABLE_SWAP_LOCK=false    # swap 거래 안 함
WTG_CUSTOMER_STREAM=false     # 5L customer quote 안 씀 (Profile-only 만)
```

## 변경 후 적용

```bash
sudo systemctl daemon-reload
sudo systemctl restart wtg-mci-price
sudo systemctl status wtg-mci-price
```

## 모든 unit 동시 작업

```bash
# WTG 서비스 모두 재시작
sudo systemctl restart 'wtg-*'

# WTG 서비스 모두 상태
sudo systemctl status 'wtg-*'

# WTG 서비스 모두 disable (정비창)
sudo systemctl stop 'wtg-*'
```

## 참고

- 표준 정책 / 부트스트랩 순서 → `docs/deployment-software.md` §7~8
- 시나리오별 설정 → `docs/deployment-scenario-ha-channel.md` §6, `docs/deployment-scenario-multi-site.md` §6
- 운영자 SOP → `docs/operations-routine.md`
