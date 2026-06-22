# WTG 운영자 루틴 SOP

> 운영자가 매일 / 매주 / 분기 / 사고 시 무엇을 하는지 한 장으로.
> **인쇄해서 모니터 옆에 붙여놓는 용도**.
> 자세한 설명은 `admin-ui-manual.md` §12 참조.

---

## 매일 5분 — 출근 직후

브라우저 `http://<admin-host>:9090/` 접속 후 :

```
□ 1. 📊 대시보드
     - broker 카드 ● 연결됨
     - 시세 rate (tick/s) 평소 범위 (±20%)
     - subscribers / connections 평소 범위

□ 2. 📊 운영 모니터링
     - HTTP 5xx rate         = 0
     - Broker disconnects    = 0
     - RateLimit denied      ≈ 0
     - QuoteID denied        = 0
     모든 카드 sparkline 에 어제 밤 spike 없는지 확인

□ 3. 📜 감사 로그
     - 어제 18:00 이후 변경 이력 확인
     - 의도하지 않은 kill_switch / pricing 변경 없는지
```

이 3개 페이지 모두 평소면 **퇴근까지 다시 안 봐도 됨**. 어느 하나라도 어긋나면 해당 페이지의 디테일로.

---

## 매주 30분 — 주 1회 (월요일 오전 권장)

```
□ 1. 📈 매매 통계 (alias × tier)
     - 지난 7일 alias 별 호출 수 + 평균 latency
     - 에러율 1% 초과 alias 있으면 원인 분석

□ 2. 🔁 FX swap 잠금 통계 (swap-lock 활성 시)
     - 부분실패 합 = 0
     - revoke_fail = 0
     - 둘 중 하나라도 누적되면 Registry / SwapIndex 점검

□ 3. 🔍 Customer 검색 (VIP 모니터링)
     - 활동 적은 VIP 확인 (영업 신호)
     - 최근 매매 latency / 5L quote 캐시 정상

□ 4. 💰 마진 테이블 — Swap 탭
     - swap_point 갱신 (운영 시장 결정 반영)

□ 5. /tmp 또는 logs/ 정리
     ./scripts/wtg-status.sh > docs/snapshots/$(date +%Y-%W).log
```

---

## 분기 1회 — 정책 검토

```
□ 1. 마진 정책 검토
     - 🔬 마진 계산기 — 샘플 시나리오 (VIP / GOLD / STD × 3 통화쌍) 시뮬레이션
     - 🪄 마진 변경 미리보기 — 다음 분기 변경안 영향 측정

□ 2. Profile / 사용자 카탈로그 정리
     - 🧩 프로파일 페이지 — 비활성 Profile 정리
     - 👥 사용자 프로파일 — 비활성 customer (90일+ 미사용) 정리

□ 3. retention 점검
     - TimescaleDB quote_bars 압축 / 삭제 정책 동작
     - etcd snapshot 보관 일수 (7일 권장)
     - Redis AOF 파일 크기

□ 4. 보안 점검
     - TLS cert 만료까지 60일 남았는지 (broker / etcd / Redis / DMZ)
     - push-secret 회전 (분기 1회 권장, ../push-secret-rotation.md)
     - 운영자 계정 권한 (roles=admin) 의 정당성

□ 5. 단순화 검토
     - ../simplification-guide.md §11 Week 4 (큰 자르기) 도입 후보 점검
     - 사용 안 하는 feature flag off 결정
```

---

## 사고 대응 — 3단계 (모니터 옆에 인쇄)

### 1단계 : 즉시 거리 두기 (30초 안)

```
admin UI → 🛡 정책 엔진 → Kill switch ON
```

- 사고 범위가 좁으면 채널별 (WEB 만 / CS 만)
- 사고 범위가 넓으면 전체

→ 모든 매매 즉시 503 reject. **분쟁 확대 차단**.

### 2단계 : 정보 수집 (10분)

```
□ 📊 운영 모니터링 → 어느 카드가 spike 인지
□ 📜 감사 로그   → 사고 시작 시각 변경 이력
□ 📜 매매 감사   → 마지막 transaction 들의 alias / status / errn
□ ⚠️ Backpressure 이력 → 부하 spike 시각 일치 여부
□ 🔌 구독자 (gRPC) → mci-edge-* 살아있는지
□ 🔗 연결 (ws)   → 외부 ws 클라이언트 수
□ 운영 모니터링 sparkline 의 시각대 + Slack alert 시각 cross-check
```

### 3단계 : 회복 (10~30분)

```
□ 원인 파악 (위 2단계 데이터로)
□ 정책 변경 또는 서비스 재시작 (필요 시)
□ Kill switch OFF (정책 변경 검증 완료 후)
□ 사용자 안내 — 메인 페이지 banner / SMS / 이메일
□ 사후 분석 시작 — docs/postmortem-YYYY-MM-DD.md (timeline + 원인 + 재발 방지)
```

---

## 자동화된 것들 (운영자가 손 댈 일 없음)

| 자동화 | 트리거 |
|---|---|
| systemd 자동 재시작 | 서비스 죽으면 5초 backoff 후 (운영) |
| broker reconnect | broker 끊김 감지 시 supervisor goroutine (모든 인스턴스) |
| logrotate | 일별, 14일 보관 (운영) |
| etcd snapshot | 매일 03:00 (운영) |
| Redis AOF | everysec (운영) |
| Slack alert | Prometheus Alertmanager (5xx spike / broker disconnect / queue 80% 등) |
| 마진 hot reload | admin UI 저장 즉시 (etcd watch) |
| Profile / 카탈로그 hot reload | 같음 |

→ 알림이 와야 운영자가 움직임. 안 오면 평소 운영.

---

## 명령 한 줄 모음

```bash
# 상태 확인
./scripts/wtg-status.sh
watch -tcn 2 ./scripts/wtg-status.sh         # 2초 갱신

# dev 스택 일괄 부팅 / 종료
./scripts/wtg-stack-up.sh                    # 기본 (단순화 v3)
./scripts/wtg-stack-up.sh --with-prom        # + Prometheus
./scripts/wtg-stack-up.sh --with-chart       # + mci-chart
./scripts/wtg-stack-up.sh --with-forwarder   # + UDP forwarder
./scripts/wtg-stack-down.sh                  # 전체 종료

# 부하 테스트
./scripts/load-test.sh low                   # 640 tick/s
./scripts/load-test.sh mid                   # 6.4k tick/s
./scripts/load-test.sh high                  # 64k tick/s

# 사고 진단
./scripts/chaos-broker.sh                    # broker 강제 kill → reconnect 검증
./scripts/broker-loss-diag.sh                # subscribe drop 진단

# 운영 (Linux 서버)
sudo systemctl status wtg-*                  # 모든 WTG unit
sudo journalctl -u wtg-mci-price -f          # mci-price 로그 실시간
sudo etcdctl endpoint health                 # etcd quorum
redis-cli -p 26379 sentinel masters          # Sentinel 상태
```

---

## 참고 문서

- `admin-ui-manual.md` §12 — 운영 시나리오 7가지 자세히
- `deployment-scenario-ha-channel.md` §11 — 단일 사이트 운영 SOP
- `deployment-scenario-multi-site.md` §7.2 — 다중 사이트 SOP
- `../simplification-guide.md` — 단순화 의사결정
- `../operations.md` — 서비스 flag/env 카탈로그
