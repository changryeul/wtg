# WTG EC2 배포 가이드

로컬 mac 개발환경 → AWS EC2 (Rocky Linux 9, t3.xlarge) 배포.
GitHub Actions 기반 CI/CD.

## 흐름 전체 그림

```
로컬 mac                GitHub Actions            EC2 (10.0.1.106)
────────                ──────────────           ─────────────────
git push master    →    test (make lint/test)
                        ↓
                        docker build (linux/amd64)
                        ↓
                        GHCR push  ────────────→  docker pull ghcr.io/changryeul/wtg:<sha>
                        (public)                  ↓
                                                  docker compose up -d
                                                  ↓
                                                  health check
```

## 최초 1회 설정

### GitHub Secrets (Settings → Secrets and variables → Actions)

| Secret | 값 | 설명 |
|---|---|---|
| `SSH_HOST` | `10.0.1.106` | EC2 private IP |
| `SSH_USER` | `winway` | SSH 사용자 |
| `SSH_KEY` | pem 전체 내용 | `-----BEGIN...-----END-----` 포함 |
| `SSH_PORT` | `22` | (옵션, default 22) |
| `FIX_PUSH_SECRET` | 랜덤 문자열 | mci-edge-fix drop copy 인증 |

### EC2 초기 세팅

```bash
ssh winway@10.0.1.106
curl -O https://raw.githubusercontent.com/changryeul/wtg/master/deploy/setup-ec2.sh
bash setup-ec2.sh
# 로그아웃 후 재로그인 (docker 그룹 활성)
```

### 첫 배포

GitHub → Actions → **Deploy to EC2** → **Run workflow** → master

## 이후 배포

master 브랜치 push 마다 자동 배포 (paths-ignore 조건 제외).

수동 배포 필요 시: Actions → **Deploy to EC2** → **Run workflow**.

## 배포된 서비스

같은 EC2 안에 network_mode: host 로 동거:

| 서비스 | 포트 | 역할 |
|---|---|---|
| broker (mymqd) | 11217 | 기존 설치 유지 |
| **etcd** | 2379 | 카탈로그 SoT (bitnami/etcd:3.5) |
| **mci-price** | 8082 / 50051 | 시세 코어 + AlgoStream |
| **mci-edge-price** | 8083 | 사용자 ws fan-out |
| **mci-admin** | 9090 | 운영 콘솔 (사내 브라우저) |
| **mci-api** | 8080 | /v1/tx (매매 → broker) |
| **mci-edge-fix** | 5001 (FIX) / 5002 (stats) | FIX 4.4 주문 gateway |
| **mci-edge-md** | 5011 (FIX) / 5012 (stats) | FIX 4.4 시세 gateway |

## 사내 접근

- Admin UI: http://10.0.1.106:9090
- 시세 ws: ws://10.0.1.106:8083/v1/subscribe
- FIX 주문: 10.0.1.106:5001
- FIX 시세: 10.0.1.106:5011
- 매매 REST: http://10.0.1.106:8080/v1/tx

Security Group 은 사내 CIDR 만 허용 (10.0.0.0/16 등). 상세 rule 은
`setup-ec2.sh` 안 firewalld 섹션 참고.

## 롤백

특정 git sha 로 되돌리기:

```bash
ssh winway@10.0.1.106
cd ~/wtg
# .env 편집 → WTG_VERSION=<이전 sha 7자리>
sed -i 's/^WTG_VERSION=.*/WTG_VERSION=abc1234/' .env
docker compose -f deploy/docker-compose.prod.yml pull
docker compose -f deploy/docker-compose.prod.yml up -d
```

또는 GitHub Actions → Deploy to EC2 → 이전 커밋 sha 로 Run workflow.

## 트러블슈팅

### 배포는 성공했는데 헬스체크 실패

```bash
ssh winway@10.0.1.106
cd ~/wtg
docker compose -f deploy/docker-compose.prod.yml logs --tail 100 mci-price
docker compose -f deploy/docker-compose.prod.yml ps
```

가장 흔한 원인:
- broker (mymqd) 안 뜸 — `ss -tln | grep 11217` 확인
- etcd 컨테이너 crash — `docker logs wtg-etcd`
- 카탈로그 파일 (etc/*.json) 손상 — CI 가 새로 push 하므로 재배포

### 이미지 pull 실패

GHCR 이 public 인지 확인 (Settings → Packages → Change visibility → Public).
private 이면 EC2 에서 `docker login ghcr.io -u <user> -p <PAT>` 필요.

### t3.xlarge 리소스 부족

`docker stats` 로 확인. 16GB / 4vCPU 이므로 여유 있어야 정상. 폭주 시:
- 로그 disk 가 찼는지 (`df -h`)
- 특정 서비스가 memory leak (재시작으로 임시 대응)

## 관측

- Prometheus scrape: mci-price `/metrics`, mci-edge-fix `:5002/metrics`, mci-edge-md `:5012/metrics`
- 관측 스택 (Prometheus + Grafana + Jaeger) 은 `deploy/observability/docker-compose.yml` — 원하면 별도로 up.
