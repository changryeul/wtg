# 운영 패키지 — 신규 운영자 인수인계용 7권

> 운영자 / SRE / 사고 대응자 / 시현자가 매일 참조하는 단일 문서 패키지.
> 본 디렉토리만 통째로 인쇄해도 운영의 90% 가 커버됩니다.

## 읽는 순서 (신규 운영자 1주)

| Day | 문서 | 분량 | 학습 목표 |
|---|---|---|---|
| 1 | [admin-ui-manual.md](admin-ui-manual.md) | 1,930 줄 | mci-admin UI 의 37 페이지가 무엇을 보여주는지 |
| 2 | [operator-guide.md](operator-guide.md) | 352 줄 | 설정 4 layer / 로그 위치·형식 / 자주 보는 메시지 |
| 3 | [operations-routine.md](operations-routine.md) | 180 줄 | 매일 5분 / 매주 30분 / 사고 시 3단계 SOP |
| 4 | [deployment-software.md](deployment-software.md) | 514 줄 | 설치할 모든 소프트웨어 (필수/권장/선택) |
| 5 | [deployment-scenario-ha-channel.md](deployment-scenario-ha-channel.md) | 2,204 줄 | 단일 사이트 HA + 5 단계 wire 멘탈모델 (가장 분량 큼) |
| 6 | [deployment-scenario-multi-site.md](deployment-scenario-multi-site.md) | 542 줄 | 다중 사이트 Active-Active + GSLB |
| 7 | [demo-scenario.md](demo-scenario.md) | 609 줄 | 시현 / 데모 60분 풀 코스 |

## 빠른 참조 — 상황 별 어디부터

| 상황 | 먼저 읽기 |
|---|---|
| 출근 직후 5분 점검 | operations-routine.md §매일 5분 |
| 사고 발생 | operations-routine.md §사고 대응 → admin-ui-manual.md §12.2 |
| "이 설정 어디서 바꿔?" | operator-guide.md §2 매트릭스 |
| "이 로그 메시지 뭐지?" | operator-guide.md §6 |
| 운영 배포 준비 | deployment-software.md → deployment-scenario-ha-channel.md §6 |
| 신규 매매 transaction | admin-ui-manual.md §5.1 (라우팅 룰) |
| 마진 정책 변경 | admin-ui-manual.md §7 + deployment-scenario-ha-channel.md §5.7 |
| swap 거래 디버깅 | admin-ui-manual.md §10.12 + deployment-scenario-ha-channel.md §4.7 |
| 시현 / 데모 진행 | demo-scenario.md |
| 다중 사이트 배포 검토 | deployment-scenario-multi-site.md |

## 본 패키지 외의 참고 문서 (`../`)

본 7권은 운영 직접 관련. 그 외 영역 :

- `../directory-structure.md` — 소스 레이아웃 + 설정 파일 카탈로그 (개발자용)
- `../simplification-guide.md` — 단순화 의사결정 도구
- `../operations.md` — 서비스 flag/env 카탈로그
- `../auth.md` — JWT + cookie_t 인증 명세
- `../conventions.md` — ApplName / Channel / Exchange 카탈로그
- `../monitoring.md` / `../observability.md` — Prometheus / Grafana
- `../mci-architecture.md` — 컴포넌트 흐름도

## 인쇄해서 모니터 옆에 붙이기 권장

| 문서 | 인쇄 절 |
|---|---|
| operations-routine.md | §매일 5분 + §사고 대응 3단계 |
| operator-guide.md | §2 매트릭스 + §7 한 줄 명령 |
| demo-scenario.md | §8 시현용 명령 모음 (시현 직전만) |
