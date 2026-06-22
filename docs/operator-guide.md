# WTG 운영자 가이드 — 설정 / 로그 / 명령 한 권

> 운영자가 매일 만지는 것 (설정 / 로그 / 명령) 의 단일 출처.
> "이걸 바꾸려면 어디를 만지지?", "이 메시지는 무슨 뜻이지?" 에 즉답.
> 자세한 운영 SOP 는 `docs/operations-routine.md`, 페이지별 설명은 `docs/admin-ui-manual.md`.

---

## 1. 설정 4 layer — 어디서 무엇을 만지는가

WTG 는 단일 `config.yaml` 한 권이 아니라 **목적별로 분리된 4 layer**:

| layer | 위치 | 무엇을 결정 | 변경 시 |
|---|---|---|---|
| **1. CLI flag** | systemd unit (`/etc/systemd/system/wtg-*.service` 의 `ExecStart=`) | listen 포트 / TLS 경로 / endpoint 주소 / feature toggle | 서비스 재시작 (10초 down) |
| **2. 환경변수** | `/etc/wtg/wtg.env` (systemd `EnvironmentFile=`) | broker / Redis / etcd endpoint / 시크릿 / Grafana password | 서비스 재시작 |
| **3. 정적 JSON** | `/opt/wtg/etc/*.json` | 통화쌍 / Profile / 마진 초기 seed (운영은 거의 안 씀) | mci-price 재시작 (정적 모드에서만) |
| **4. etcd watch** | etcd `wtg/*` keys — admin UI 가 GUI 로 편집 | 라우팅 룰 / 정책 / 마진 / Profile / QuoteID 엔진 / rate-limit | **즉시 hot reload (0초)** |

운영 환경에서 운영자가 만지는 빈도 :

```
매일      :  layer 4 (admin UI)               ────── 정책 변경 / Kill switch / 마진
가끔      :  layer 2 (env 파일)               ────── endpoint 이전 / 시크릿 회전
드물게    :  layer 1 (systemd unit)           ────── flag 추가 / feature toggle
운영 거의 X :  layer 3 (정적 JSON)             ────── dev 또는 초기 시드 전용
```

## 2. "이걸 바꾸려면 어디를 만지지?" 매트릭스

| 무엇을 바꾸고 싶나 | 어디 만지나 | 다운타임 |
|---|---|---|
| 마진 정책 (HQ / Site / Customer / Window / Swap) | admin UI `💰 마진 테이블` | 0 |
| Profile 추가 / 삭제 (Channel.Site.Tier) | admin UI `🧩 프로파일` | 0 |
| 사용자의 Profile 매핑 | admin UI `👥 사용자 프로파일` | 0 |
| 통화쌍 활성/비활성 | admin UI `🔗 통화쌍 마스터` | 0 |
| 매매 transaction alias | admin UI `🔀 라우팅 룰` | 0 |
| Kill switch (긴급 거래 중단) | admin UI `🛡 정책 엔진` | 0 |
| rate-limit 룰 | admin UI `🚦 Rate Limit 정책` | 0 |
| QuoteID 엔진 / 권한 | admin UI `🔑 QuoteID 엔진` | 0 |
| 휴일 캘린더 | admin UI `📆 휴일 캘린더` | 0 |
| broker active endpoint 이전 | `/etc/wtg/wtg.env` 의 `WTG_BROKER_HOST` | 서비스 재시작 |
| TLS cert 회전 | `/etc/pki/wtg/*.crt` 교체 + systemd reload | 0 (reload) |
| Grafana password 변경 | `wtg.env` 의 `WTG_GRAFANA_PASS` | mci-admin 재시작 |
| Prometheus / OTel endpoint | `wtg.env` 의 `WTG_PROM_URL` / `WTG_OTEL_ENDPOINT` | 서비스 재시작 |
| swap-lock 활성/비활성 | mci-price systemd unit 의 `--enable-swap-lock` flag | 서비스 재시작 |
| Profile customer quote 활성 | mci-edge-price unit 의 `--quote-stream` | 재시작 |
| 5L customer quote 활성 | mci-edge-price unit 의 `--customer-stream` | 재시작 |
| ws queue 크기 (backpressure 회피) | mci-edge-price 의 코드 default — flag 없음 | 코드 변경 |
| 봉 retention 정책 | TimescaleDB 의 `add_retention_policy` (psql) | 0 |

→ **정책은 GUI, endpoint/feature 는 env/flag, 코드는 거의 안 만짐**.

## 3. 로그 — 어디에, 어떤 형식

### 3.1 위치

| 환경 | 어디 |
|---|---|
| **dev (macOS 로컬)** | `logs/<service>.log` — `wtg-stack-up.sh` 가 `nohup ... > logs/<name>.log` 로 리디렉트 |
| **운영 (Linux systemd)** | `journalctl -u wtg-<service>` — systemd unit 의 `StandardOutput=journal` |
| (선택) 운영 + Loki | Promtail / Vector agent 가 journald → Loki | Grafana 의 Explore 에서 검색 |

dev 환경 logs 디렉토리 :
```
logs/mci-admin.log        ← mci-admin (slog JSON + embedded etcd zap)
logs/mci-price.log        ← mci-price (slog JSON)
logs/mci-edge-price.log   ← mci-edge-price (slog JSON + HTTP access)
logs/mci-chart.log        ← mci-chart (slog JSON)
logs/mci-edge-chart.log   ← mci-edge-chart
logs/quote-forwarder.log  ← quote-forwarder (logfmt)
logs/prometheus.log       ← Prometheus (자체 logfmt)
logs/dev-tick.log         ← dev tickloop (Python plain print)
logs/load-gen.log         ← load-gen
logs/postgres.log         ← postgres (Docker container 의 stdout)
```

### 3.2 로그 형식 — 3 가지 혼재

#### 형식 A — slog JSON (mci-* 핵심 서비스)

```json
{"time":"2026-06-20T23:38:06.501572+09:00","level":"INFO","msg":"PublishTick 종료","accepted_total":0,"dropped_total":0}
{"time":"2026-06-20T23:38:06.51475+09:00","level":"INFO","msg":"PricingConsumer 최종 통계","ticks_in":199196,"ticks_dropped":0,"quotes_published":1394372,"publish_errors":0}
```

| 필드 | 의미 |
|---|---|
| `time` | RFC3339 + ns 정밀 |
| `level` | `DEBUG` / `INFO` / `WARN` / `ERROR` |
| `msg` | 한글 가능. 한 줄 한 이벤트 |
| 나머지 | 컨텍스트 (count / id / latency / 등) — 키별로 단일 값 |

→ Go 표준 `log/slog` (Go 1.21+). `mci-price` / `mci-edge-price` / `mci-admin` / `mci-chart` / `mci-edge-*` / `mci-api` / `mci-push` 모두 이 형식.

#### 형식 B — logfmt (quote-forwarder, Prometheus, mci-admin 의 embedded etcd 부분만 zap)

```
time=2026-06-20T21:45:29.541+09:00 level=INFO msg="UDP listen" feed=SMB addr=127.0.0.1:30044 rcvbuf_req=4194304 rcvbuf_actual=4194304
```

| 필드 | 의미 |
|---|---|
| `time` | (위와 동일) |
| `level` | (위와 동일) |
| `msg` | 따옴표로 감싼 메시지 |
| 나머지 | key=value 페어 |

→ Go 의 `slog.TextHandler` 또는 Prometheus 자체 형식. `quote-forwarder` / `Prometheus`.

mci-admin 의 로그에 **두 형식이 섞임** :
- 서비스 자체 로그 = slog JSON (`{"time":..., "msg":...}`)
- embedded etcd 부분 = zap (`{"level":..., "ts":..., "caller":..., "stacktrace":...}`)

#### 형식 C — plain text (Python tickloop)

```
tickloop: http://127.0.0.1:8082/v1/dev/tick  period=0.2s  pairs=6
```

→ dev 전용. 운영 환경에선 없음.

### 3.3 HTTP access 로그 — JSON 안에 통합

DMZ 서비스 (`mci-edge-*`) 와 admin 은 모든 HTTP 요청을 같은 슬로그 라인으로 :

```json
{"time":"2026-06-20T23:38:00.51916+09:00","level":"INFO","msg":"http",
 "method":"GET","path":"/metrics","status":200,"dur":1361292,
 "remote":"127.0.0.1:55558","ua":"Prometheus/3.12.0","rid":"f6a78d0ba82c6760"}
```

| 필드 | 의미 |
|---|---|
| `msg` | 항상 `"http"` |
| `method` | GET / POST / PUT / DELETE |
| `path` | URL path |
| `status` | HTTP status code |
| `dur` | 처리 시간 (나노초) |
| `remote` | 클라이언트 IP:port |
| `ua` | User-Agent |
| `rid` | **request id** — 분산 trace 추적용 (OTel trace_id 와 별개) |

→ `rid` 로 같은 요청의 모든 로그를 묶어 추적 가능.

## 4. dev vs 운영 — 로그 명령 모음

### 4.1 dev (macOS)

```bash
# 한 서비스 실시간
tail -f logs/mci-price.log

# 한 서비스 JSON 정렬해서 사람 보기 좋게
tail -f logs/mci-price.log | jq -C .

# 특정 키워드만
tail -f logs/mci-price.log | jq 'select(.level=="WARN" or .level=="ERROR")'

# msg 별 카운트 (마지막 1만 줄)
tail -10000 logs/mci-price.log | jq -r '.msg' | sort | uniq -c | sort -rn | head -20

# request id 한 건의 모든 로그
grep '"rid":"f6a78d0ba82c6760"' logs/mci-edge-price.log

# 5xx 응답만
tail -f logs/mci-edge-price.log | jq 'select(.status >= 500)'

# 시간 범위 (오늘 09:00~10:00)
awk '/"time":"2026-06-22T09/' logs/mci-price.log | head -100
```

### 4.2 운영 (Linux systemd)

```bash
# 실시간
sudo journalctl -u wtg-mci-price -f

# JSON 그대로 (jq 와 같이)
sudo journalctl -u wtg-mci-price -f -o cat | jq -C .

# 5xx 만
sudo journalctl -u wtg-mci-edge-price --since "10 min ago" -o cat \
  | jq 'select(.status >= 500)'

# 모든 WTG 서비스 동시
sudo journalctl -u 'wtg-*' -f -o cat | jq -C .

# 특정 시간 범위
sudo journalctl -u wtg-mci-price --since "2026-06-22 09:00" --until "2026-06-22 10:00"

# WARN/ERROR 만 빠르게
sudo journalctl -u wtg-mci-price -p warning --since "1 hour ago"

# 디스크 사용량 확인 / 정리
sudo journalctl --disk-usage
sudo journalctl --vacuum-time=14d
```

## 5. 로그 회전 / 보관

### 5.1 dev 환경

`logs/` 디렉토리 — `.gitignore` 로 git 제외. 회전 없음. 운영자가 가끔 수동 정리 :
```bash
# 14일 이상 된 로그 정리
find logs -name '*.log' -mtime +14 -delete
```

### 5.2 운영 환경 (Linux)

#### journald (권장)
- `/etc/systemd/journald.conf` 의 `SystemMaxUse=` / `MaxRetentionSec=` 로 자동 회전
- 권장 : `SystemMaxUse=10G`, `MaxRetentionSec=14d`

#### 또는 logrotate (별도 파일 로그)
```
# /etc/logrotate.d/wtg
/var/log/wtg/*.log {
    daily
    rotate 14
    compress
    missingok
    notifempty
    copytruncate
}
```

### 5.3 Loki 통합 (선택)

운영 시 추천 — 분산 환경에서 모든 노드의 로그를 한 곳에 :

```yaml
# Promtail config 일부
- job_name: wtg-systemd
  journal:
    path: /var/log/journal
    matches: _SYSTEMD_UNIT=wtg-mci-price.service|_SYSTEMD_UNIT=wtg-mci-edge-price.service|...
  relabel_configs:
    - source_labels: ['__journal__systemd_unit']
      target_label: 'service'
```

→ Grafana → Explore → `{service="wtg-mci-price"} | json | level="WARN"` 로 빠르게.

## 6. 자주 보는 로그 메시지 — 의미와 대응

### 6.1 정상 (걱정 X)

| msg | 의미 |
|---|---|
| `gRPC listen 시작 / HTTP listen 시작` | 서비스 부팅 완료 |
| `BestConsumer 활성` | 다중 feed best 산정 활성 |
| `PricingConsumer 활성` | 마진 계산 활성 |
| `Swap quote-lock endpoint 활성` | swap-lock RPC 등록됨 |
| `PriceService Subscribe 시작 / SubscribeQuote 시작` | mci-edge-price 가 mci-price 에 붙음 |
| `Aggregator 활성` | 봉 누적 활성 |
| `EtcdRegistry 초기 로드 count=N` | etcd 카탈로그 N건 로드 |
| `dev-seed 라우팅 룰` | DevMode 초기 seed |
| `http status=200` | 정상 요청 |
| `mci-* 정상 종료` | graceful shutdown 완료 |

### 6.2 WARN (모니터링)

| msg | 의미 | 대응 |
|---|---|---|
| `backpressure 감지 — 큐 80% 도달` | ws / gRPC subscriber 큐가 가득 차려 함 | 부하 ↓ 또는 queue_cap 늘림 |
| `SymbolMap 비어있음` | 통화쌍 카탈로그 없음 | etc/symbols.json 또는 etcd 등록 |
| `ProfileSource 비어있음` | Profile 카탈로그 없음 | etcd 또는 정적 파일 등록 |
| `EnableSwapLock=true 인데 Registry 가 SwapIndex 미구현` | swap-lock endpoint 비활성 | Registry 종류 변경 (Memory/Redis) |
| `cluster promote 중` | broker active-standby 전환 중 | 자동 회복 — 1초 대기 |
| `crossed_fallback=true` | 두 source 호가가 역전 | 일시면 OK, 지속이면 source 점검 |

### 6.3 ERROR (즉시 대응)

| msg | 의미 | 대응 |
|---|---|---|
| `broker disconnected` | broker 끊김 | supervisor 가 자동 재연결. spike 없는지 모니터링 |
| `connection refused` | upstream endpoint 죽음 | 해당 서비스 상태 확인 |
| `tls: certificate signed by unknown authority` | CA 체인 문제 | PKI 경로 / chain 확인 |
| `flag provided but not defined: -X` | unit / env 오설정 | 본 가이드 §2 매트릭스 재확인 |
| `policy_blocked` | Kill switch / 정비창 활성 | 의도된 거면 무시, 아니면 admin UI 정책 엔진 |
| `near leg 등록 실패: context deadline exceeded` | Redis Registry Put timeout | Redis Sentinel master 상태 확인 |
| `EXPIRED` / `ALREADY_CONSUMED` | quote_id 만료 / 소진 | 클라이언트 retry 정책 점검 |
| `invalid_quote` (forwarder) | FIX 파싱 실패 | feed schema mismatch — feed 측 점검 |

## 7. 한 줄 명령 모음 (인쇄용)

```bash
# dev 환경 모든 로그 한꺼번에 실시간
tail -f logs/*.log | jq -RC 'try fromjson catch .'

# 운영 환경 — 모든 WTG 서비스 + level WARN 이상
sudo journalctl -u 'wtg-*' -p warning -f -o cat | jq -C .

# 5xx 응답이 발생한 endpoint top 10 (오늘)
sudo journalctl -u wtg-mci-edge-price --since today -o cat \
  | jq -r 'select(.status >= 500) | .path' | sort | uniq -c | sort -rn | head -10

# backpressure WARN 카운트 (지난 1시간)
sudo journalctl -u wtg-mci-edge-price --since "1 hour ago" -o cat \
  | jq 'select(.msg == "backpressure 감지 — 큐 80% 도달")' | wc -l

# 특정 사용자의 모든 활동 (HTTP access + 매매)
sudo journalctl -u 'wtg-*' --since today -o cat \
  | jq 'select(.customer_id == "customer-1234@partner.co.kr")'

# 특정 trace_id 의 전체 흐름 (DMZ → Internal → broker)
sudo journalctl -u 'wtg-*' -o cat \
  | jq 'select(.trace_id == "abc123...")'

# 운영 service 상태 + 마지막 100줄
sudo systemctl status wtg-mci-price --no-pager -n 100
```

## 8. 로그 형식 통일 검토 (참고)

현재 3 형식 혼재 — 운영 시 통합이 가능하지만 비용/이득 :

| 옵션 | 효과 | 비용 |
|---|---|---|
| 그대로 둠 | jq 가 JSON 잘 처리. logfmt 도 grep / awk 충분 | 0 |
| quote-forwarder 도 slog JSON 으로 통일 | 모든 WTG 로그를 jq 단일로 | 코드 변경 (forwarder 의 logger 교체) |
| zap (embedded etcd) 출력 억제 | mci-admin 로그가 깔끔 | etcd 라이브러리 의존 — 어려움 |

→ 현재는 **그대로 두고 jq 명령 모음 (§7) 으로 흡수** 권장. 코드 변경 0.

## 9. 자주 받는 질문

- **Q. 로그 디스크가 가득 찼다.**
  `sudo journalctl --vacuum-time=7d` 또는 `journald.conf` 의 `SystemMaxUse` 줄임.

- **Q. mci-admin 로그가 이상한 zap 형식이 섞여 있다.**
  embedded etcd (etcd 라이브러리) 가 자체 로깅. 무시해도 OK. 정상.

- **Q. logs/*.log 가 git 에 포함되나?**
  ❌ `.gitignore` 의 `/logs/` 로 제외.

- **Q. JSON 안에서 한글 보이는 게 깨진다.**
  `jq -C .` 사용 — UTF-8 출력. 터미널 LANG=ko_KR.UTF-8 또는 en_US.UTF-8 확인.

- **Q. 운영 환경에서 dev 처럼 logs/ 에 쓰고 싶다.**
  systemd unit 의 `StandardOutput=` 을 `append:/var/log/wtg/<service>.log` 로 변경. 단 journald 권장 (회전 자동).

## 10. 참고 문서

- `docs/operations-routine.md` — 운영자 일일 / 주간 / 사고 SOP
- `docs/admin-ui-manual.md` — admin UI 37 페이지 매뉴얼 (각 페이지의 endpoint / 메시지 의미)
- `docs/observability.md` — Prometheus / Grafana 통합 가이드
- `docs/monitoring.md` — 메트릭 명세
- `deploy/systemd/wtg.env.sample` — 환경 변수 카탈로그
- `deploy/systemd/DEPLOY-CHECKLIST.md` — 운영 배포 단계별 명령
