# Phase 2.7 — broker subscribe 제거 rollout 계획

WTG mci-push 의 broker 의존을 **완전히 제거**하는 마지막 단계. Phase 2.1~2.6
의 누적 작업 (HTTP path / multi-instance / 인증 / 모니터링 / C SDK) 위에
운영 검증을 거친 후 실시.

## 진입 조건 (Gate)

다음 4개 조건 **모두** 만족 시 진행:

| 조건 | 측정 | 임계 |
|---|---|---|
| HTTP 비중 30일 평균 | `wtg:push:http_ratio:30d` | > 99% |
| broker 잔여 rate 30일 평균 | `wtg:push:recv_broker:rate30d` | < 0.01/s |
| Grafana alert | `PushReadyForBrokerRemoval` | firing for 7d |
| 운영 svc dual-write 검증 | manually | 모든 svc 가 HTTP-only 전환 완료 |

→ 자동 alert (`PushReadyForBrokerRemoval`, severity=info) 가 trigger 역할.

## Rollout 단계

### Stage 0 — 사전 준비 (1주)

- [ ] 운영 svc 목록 추출 (`mci_push_http_inject_total` 의 `cn` 라벨 unique values)
- [ ] 각 svc 의 broker publish 코드 위치 confirm (마이그레이션 대상)
- [ ] 운영 wiki 에 작업 일정 + 영향 범위 명시
- [ ] rollback 계획 검토 (Stage 5 참조)
- [ ] 야간 작업창 확보 (시장 휴장 시간대 권장)

### Stage 1 — staging 검증 (1~2주)

- [ ] staging 의 mci-push 를 `--no-broker` 로 부팅
- [ ] staging 운영 svc 가 HTTP push 만으로 정상 동작 검증
- [ ] 1주 이상 운영 — 운영 시나리오 (장중 부하 / 야간 batch / 시장 휴장) 모두 통과
- [ ] dispatcher 통계 검증 (recv_http 일정, drop_inject_full = 0)
- [ ] ws 사용자 영향 없음 확인 (E2E)

### Stage 2 — 운영 mci-push 단계적 전환 (3일)

운영 mci-push 가 다중 인스턴스 (Phase 2.2 MultiClient) 라는 전제:

- [ ] **Day 1**: 인스턴스 1개 (canary) 를 `--no-broker` 로 재시작
- [ ] Grafana 24h 모니터링 — canary 인스턴스의 ws 사용자에게 push 정상 도달
- [ ] **Day 2**: 인스턴스 절반 전환
- [ ] **Day 3**: 나머지 전환 → 운영 100% standalone

전환 중에도 broker 의존 코드는 mci-push 측에 남아있음 — `--no-broker` flag
로 비활성만. 문제 시 flag 제거 + 재시작으로 즉시 복구 가능.

### Stage 3 — broker 제거 PR (코드 변경, 1주)

운영 100% `--no-broker` 안정 운영 1~2개월 후 실시:

- [ ] PR 브랜치 작성 (`feat/phase-2.7-remove-broker`)
- [ ] `internal/push/server.go` — `mymq.Open` 호출 + Reconnect / MetricsHook 제거
- [ ] `internal/push/dispatcher.go` — `Subscriber` interface + `sub` field 제거
- [ ] `internal/push/config.go` — `BrokerHost` / `BrokerPort` / `QueueName` /
  `BrokerTLS*` / `NoBroker` 등 broker 관련 옵션 제거
- [ ] `cmd/mci-push/main.go` — broker 부팅 로그 정리
- [ ] dispatcher_metrics.go — `recv_broker_total` 등 broker 관련 metric 제거
- [ ] 관련 test 제거 / refactor
- [ ] Grafana dashboard / recording rules / alerts — broker 관련 항목 제거
- [ ] docs: `docs/conventions.md` / `docs/operations.md` / `docs/push-*.md` 갱신
- [ ] CHANGELOG / migration note 작성

### Stage 4 — 배포 + 관찰 (1주)

- [ ] 새 binary 배포 (코드상 broker 의존 제거)
- [ ] 1주 모니터링 — recv_http rate / drop 비율 / send_failed 변동 없음 확인
- [ ] 운영 svc 측 broker publish 코드도 정리 (PR 별도)
- [ ] Phase 2 완료 선언

### Stage 5 — Rollback (비상 시)

**Stage 2 중 문제 발생:**
```bash
# canary 인스턴스의 --no-broker flag 제거 + 재시작
sudo systemctl edit mci-push   # --no-broker 라인 삭제
sudo systemctl restart mci-push
# → broker 재연결, subscribe 정상화. HTTP path 도 같이 동작 (dual source)
```

**Stage 4 (코드 제거) 후 문제 발생:**
- broker 의존 코드가 이미 git history 에서 사라짐 (단순 revert 어려움)
- 따라서 **Stage 3 PR 의 revert 도 미리 준비** — `git revert <merge-commit>` 검증
- 또는 git tag (`pre-phase-2.7`) 로 직전 commit 보관 → 비상 시 binary 재배포

## 운영 svc 측 dual-write 패턴

검증 기간 (Phase 2.5~2.7 사이) 의 권장 패턴 — broker publish + HTTP push **둘 다** 발사:

```c
#include "wtgpush.h"

static wtg_push_client_t g_push_cli;

void unsolicited_send(const char *user, const char *data) {
    /* 1. HTTP push (primary) */
    int http_rc = wtg_push_send(&g_push_cli, user, data);

    /* 2. broker publish (backup) — 항상 호출 */
    PublishRequest pub = { ... };
    int broker_rc = mq_publish(&pub);

    /* 3. 두 결과 비교 — 한쪽만 실패면 metric 카운트 (운영자 알림용) */
    if (http_rc != WTGPUSH_OK && broker_rc == 0) {
        /* HTTP 만 실패 — Grafana 의 PushAuthFailureSurge / inject_full alert 가 잡음 */
        log_warn("dual-write: HTTP %d, broker OK", http_rc);
    } else if (http_rc == WTGPUSH_OK && broker_rc != 0) {
        log_warn("dual-write: HTTP OK, broker %d — broker 의존 svc 영향", broker_rc);
    } else if (http_rc != WTGPUSH_OK && broker_rc != 0) {
        log_error("dual-write 양쪽 실패: HTTP %d, broker %d", http_rc, broker_rc);
    }
    /* 둘 다 OK 가 정상 — log 없음 */
}
```

### 전환 시점

| 시점 | 호출 패턴 |
|---|---|
| **Phase 2.6** (지금) | broker publish (기존) — HTTP 미사용 |
| **검증 시작** | dual-write (broker + HTTP) — 양쪽 metric 비교 |
| **검증 완료 후 svc 단위** | HTTP only (broker publish 코드 제거) |
| **모든 svc 전환 완료** | mci-push `--no-broker` 가능 |
| **Phase 2.7 Stage 3** | mci-push 코드의 broker subscribe 제거 |

각 svc 의 전환 진척은 Grafana 의 **`Inject rate by CN`** panel 에서 자동
가시화 — 새 cn 라벨이 등장하고 rate 가 올라가는 추세를 추적.

## 통제 측정 (Phase 2.7 진행 중 모니터링)

```promql
# 1. HTTP 비중 - 99% 유지하는가
wtg:push:http_ratio:5m

# 2. 어느 svc 가 아직 broker 만 사용하는가 (cn 라벨 없는 broker 트래픽)
rate(mci_push_dispatcher_recv_broker_total[5m])

# 3. dual-write 중인 svc 의 양쪽 일치율
#   (운영 svc 측에서 dual_write_match_total / dual_write_mismatch_total 노출 가정)

# 4. drop 비율 정상 유지
rate(mci_push_dispatcher_dropped_total[5m])
  / clamp_min(rate(mci_push_dispatcher_received_total[5m]), 0.01)

# 5. send_failed (slow consumer) 정상
rate(mci_push_dispatcher_send_failed_total[5m])
```

## 결정 권한

| 단계 | 결정자 |
|---|---|
| Stage 0~1 (staging) | 개발 팀 |
| Stage 2 (운영 canary) | 운영 팀 + 개발 팀 합의 |
| Stage 3 (코드 제거 PR) | 개발 팀 lead review + 운영 팀 sign-off |
| Stage 4 배포 | 운영 팀 |
| Rollback | on-call 즉시 (사후 보고) |

## 관련 문서

- `docs/push-monitoring.md` — Grafana / Prometheus 가시화
- `docs/push-secret-rotation.md` — 인증 모드 결정 (secret-only)
- `cside/wtgpush/README.md` — C SDK 사용 가이드
- `etc/grafana/mci-push-alerts.yml` — `PushReadyForBrokerRemoval` alert 정의
