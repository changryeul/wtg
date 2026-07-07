# WTG EC2 배포 가이드

로컬 mac 개발환경 → AWS EC2 (nh-fxallone-dev, Rocky Linux 9.7, t3.xlarge) 배포.
GitHub Actions 기반 CI/CD.

## EC2 환경 전제

- Rocky Linux 9.7 (x86_64)
- Oracle Client + sqlplus + Pro-C (+ ORACLE_HOME) 이미 세팅
- broker (mymqd) 는 winway 계정에서 running (127.0.0.1:11217)
- SSH 접속 유저: **rocky** (Rocky AMI default sudoer)
- 앱 실행 유저: **winway** (덜 특권한 앱 유저)
- pem key: `winway-nh-fxallone-dev.pem`

## 흐름 전체 그림

```
로컬 mac                GitHub Actions             EC2 (Rocky Linux)
────────                ──────────────             ─────────────────
git push master    →    test (make lint/test-race)
                        ↓
                        docker build (linux/amd64)
                        ↓
                        GHCR push  ────────────→   [SSH: rocky]
                                                   scp → /home/rocky/wtg-staging/
                                                   sudo mv → /home/winway/wtg/
                                                   sudo -u winway docker compose pull
                                                   sudo -u winway docker compose up -d
                                                   ↓
                                                   health check (rocky 로 curl)
```

## 최초 1회 설정

### 1. GitHub Secrets (Settings → Secrets and variables → Actions)

| Secret | 값 | 설명 |
|---|---|---|
| `SSH_HOST` | `<EC2 private IP>` | 사내 IP |
| `SSH_KEY` | pem 전체 내용 | `-----BEGIN...-----END-----` 포함 |
| `SSH_PORT` | `22` | (옵션) |
| `FIX_PUSH_SECRET` | `openssl rand -hex 32` | mci-edge-fix drop copy 인증 |

**SSH_USER 는 secret 아님** — workflow 에 `rocky` 하드코드.
**DEPLOY_USER 도 secret 아님** — workflow 에 `winway` 하드코드.

### 2. EC2 초기 세팅

로컬 mac 에서:
```bash
ssh -i /Users/winwaysystems/mywork/cert/winway-nh-fxallone-dev.pem \
    rocky@<private-ip>
```

EC2 rocky 세션에서:
```bash
curl -O https://raw.githubusercontent.com/changryeul/wtg/master/deploy/setup-ec2.sh
bash setup-ec2.sh
```

스크립트가 하는 일:
- Docker + docker compose 설치 (dnf)
- winway 를 docker 그룹에 추가
- `/home/winway/wtg/{deploy,etc,logs,tmp}` 생성
- `/home/rocky/wtg-staging/` (CI 파일 전송 위치)
- GHCR image pull 테스트
- broker 상태 확인

세팅 후 winway 세션 재로그인:
```bash
exit
ssh -i winway-nh-fxallone-dev.pem rocky@<private-ip>
sudo su - winway
docker ps  # 권한 없이 실행되면 OK
```

### 3. GHCR image 를 public 으로

첫 배포 후:
`https://github.com/changryeul?tab=packages` → **wtg** 패키지
→ Package settings → **Change visibility to Public**

(private 유지하고 싶으면 EC2 에서 `docker login ghcr.io` 추가 필요.)

### 4. 첫 배포

`https://github.com/changryeul/wtg/actions`
→ **Deploy to EC2** → **Run workflow** → **master**

## 이후 배포

`master` push 시 자동. paths-ignore 조건 (`docs/**`, `**.md`, `.gitignore`,
`CLAUDE.md`) 은 스킵.

수동 배포: Actions → **Deploy to EC2** → **Run workflow**.

## 배포된 서비스

같은 EC2 안, network_mode: host 로 동거:

| 서비스 | 포트 | 역할 |
|---|---|---|
| broker (mymqd) | 11217 | **기존 설치, WTG 밖에서 관리** |
| etcd | 2379 | 카탈로그 SoT (bitnami/etcd:3.5) |
| mci-price | 8082 / 50051 | 시세 코어 + AlgoStream |
| mci-edge-price | 8083 | 사용자 ws fan-out |
| mci-admin | 9090 | 운영 콘솔 (사내 브라우저) |
| mci-api | 8080 | /v1/tx (매매 → broker) |
| mci-edge-fix | 5001 / 5002 | FIX 4.4 주문 gateway |
| mci-edge-md | 5011 / 5012 | FIX 4.4 시세 gateway |

## 사내 접근

| 서비스 | URL |
|---|---|
| Admin UI | `http://<private-ip>:9090` |
| 시세 ws | `ws://<private-ip>:8083/v1/subscribe` |
| FIX 주문 | `<private-ip>:5001` (TCP) |
| FIX 시세 | `<private-ip>:5011` (TCP) |
| 매매 REST | `http://<private-ip>:8080/v1/tx` |
| Metrics | `http://<private-ip>:8082/metrics` |

Security Group / firewalld 는 사내 CIDR 만 허용. rule 은 `setup-ec2.sh`
안 firewalld 섹션 참고 (지금은 주석 처리, 사내 CIDR 확정 후 활성).

## 롤백

특정 git sha 로 되돌리기:

```bash
ssh -i winway-nh-fxallone-dev.pem rocky@<private-ip>
sudo su - winway
cd ~/wtg
sed -i 's/^WTG_VERSION=.*/WTG_VERSION=<이전 sha 7자리>/' .env
docker compose -f deploy/docker-compose.prod.yml pull
docker compose -f deploy/docker-compose.prod.yml up -d
```

또는 GitHub Actions → Deploy to EC2 → 이전 커밋 sha 로 Run workflow.

## 트러블슈팅

### 배포 성공했는데 헬스체크 실패

```bash
sudo su - winway
cd ~/wtg
docker compose -f deploy/docker-compose.prod.yml ps
docker compose -f deploy/docker-compose.prod.yml logs --tail 100 mci-price
```

가장 흔한 원인:
- broker 안 뜸 — `ss -tln | grep 11217`
- etcd 컨테이너 crash — `docker logs wtg-etcd`
- 카탈로그 파일 (etc/*.json) 문제 — CI 가 재배포하면 해결

### scp 는 성공했는데 파일이 winway 홈에 없음

Workflow 의 "Move to winway" 단계 실패. GitHub Actions log 확인:
- `sudo` 권한 없음 → rocky 를 sudoers 에 추가
- winway 계정 없음 → `sudo useradd -m winway`

### 이미지 pull 실패

GHCR public 확인. private 이면:
```bash
sudo su - winway
docker login ghcr.io -u <github-user> -p <PAT>
```

## 관측 (선택)

Prometheus + Grafana + Jaeger 관측 스택은 별도:
```bash
sudo su - winway
cd /path/to/wtg
docker compose -f deploy/observability/docker-compose.yml up -d
```
