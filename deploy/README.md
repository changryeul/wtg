# WTG EC2 배포 가이드 (Self-hosted GitHub Actions Runner)

로컬 mac 개발환경 → AWS EC2 (nh-fxallone-dev, Rocky Linux 9.7, t3.xlarge)
배포. **GitHub Actions self-hosted runner** 기반.

## 왜 self-hosted runner

- EC2 는 사내 CIDR 만 접근 (private IP). GitHub-hosted runner 는 수천 개
  public IP 대역이라 SG 관리 불가.
- Self-hosted runner 는 사내 EC2 안에서 workflow 실행 → private IP 접근 OK,
  SSH secrets 불필요, 방화벽 rule 그대로 유지.
- Test / build 는 여전히 GitHub-hosted (ubuntu-latest). Deploy 만 self-hosted.

## EC2 환경 전제

- Rocky Linux 9.7 (x86_64), t3.xlarge
- Oracle Client + sqlplus + Pro-C (+ ORACLE_HOME) 이미 세팅
- broker (mymqd) 는 winway 계정에서 running (127.0.0.1:11217)
- SSH 접속 유저: **rocky** (Rocky AMI default sudoer)
- 앱 실행 유저: **winway** (덜 특권한 앱 유저)
- Runner 실행 유저: **rocky** (sudoer 필요 — deploy job 이 sudo 사용)

## 흐름 전체 그림

```
로컬 mac              GitHub Actions               EC2 (self-hosted runner, rocky)
────────              ──────────────               ──────────────────────────
git push master  →    test (ubuntu-latest)
                      ↓
                      docker build (linux/amd64, ubuntu-latest)
                      ↓
                      GHCR push  ────────────→    deploy job (self-hosted):
                                                  · actions/checkout
                                                  · sudo cp → /home/winway/wtg/
                                                  · sudo -u winway docker compose pull + up
                                                  · curl 127.0.0.1 healthcheck
```

## 최초 1회 설정

### 1. EC2 초기 세팅

로컬 mac 에서:
```bash
ssh -i /Users/winwaysystems/mywork/cert/winway-nh-fxallone-dev.pem rocky@<private-ip>
```

EC2 rocky 세션에서:
```bash
curl -O https://raw.githubusercontent.com/changryeul/wtg/master/deploy/setup-ec2.sh
bash setup-ec2.sh
```

setup-ec2.sh 가 하는 일:
- Docker + docker compose plugin 설치 (dnf)
- winway 를 docker 그룹에 추가
- `/home/winway/wtg/{deploy,etc,logs,tmp}` 디렉토리 생성
- broker 상태 확인

winway 세션 재로그인 (docker 그룹 활성):
```bash
exit
ssh -i winway-nh-fxallone-dev.pem rocky@<private-ip>
sudo su - winway
docker ps  # 권한 없이 실행되면 OK
```

### 2. GitHub Actions self-hosted runner 설치

**GitHub 저장소에서 registration token 발급**:
- `https://github.com/changryeul/wtg/settings/actions/runners/new`
- **Linux / x64** 선택 → 화면에 나오는 명령 복사

**EC2 rocky 세션에서 (아직 로그인 상태)**:
```bash
mkdir -p ~/actions-runner && cd ~/actions-runner

# GitHub UI 가 알려주는 정확한 버전/URL 사용 (예시).
curl -o actions-runner.tar.gz -L \
  https://github.com/actions/runner/releases/download/v2.319.1/actions-runner-linux-x64-2.319.1.tar.gz
tar xzf actions-runner.tar.gz

# UI 가 알려주는 token 삽입.
./config.sh --url https://github.com/changryeul/wtg --token <TOKEN_FROM_UI>
# 나머지 프롬프트는 default 로 Enter (labels: self-hosted, Linux, X64)
```

**systemd service 로 등록** (24/7 실행):
```bash
sudo ./svc.sh install rocky
sudo ./svc.sh start
sudo ./svc.sh status
# → Active: active (running) 확인
```

**확인**: `https://github.com/changryeul/wtg/settings/actions/runners`
→ runner 가 **Idle** 상태로 보이면 완료.

### 3. GitHub Secrets

이제 SSH 관련 secret 은 **불필요**. 남는 것 1개:

| Secret | 값 |
|---|---|
| `FIX_PUSH_SECRET` | `openssl rand -hex 32` 결과 |

이전에 등록한 `SSH_HOST` / `SSH_KEY` / `SSH_PORT` 는 삭제해도 됨 (안 참조).

### 4. GHCR image 를 public 으로

첫 배포 후:
`https://github.com/changryeul?tab=packages` → **wtg** 패키지
→ Package settings → **Change visibility to Public**

(private 유지하고 싶으면 EC2 에서 `docker login ghcr.io` 추가.)

### 5. 첫 배포

`https://github.com/changryeul/wtg/actions`
→ **Deploy to EC2** → **Run workflow** → **master**

## 이후 배포

`master` push 마다 자동. paths-ignore 조건 (`docs/**`, `**.md` 등) 은 스킵.

## 배포된 서비스

같은 EC2, network_mode: host:

| 서비스 | 포트 | 역할 |
|---|---|---|
| broker (mymqd) | 11217 | 기존 (WTG 외부) |
| etcd | 2379 | 카탈로그 SoT |
| mci-price | 8082 / 50051 | 시세 코어 + AlgoStream |
| mci-edge-price | 8083 | 사용자 ws fan-out |
| mci-admin | 9090 | 운영 콘솔 |
| mci-api | 8080 | /v1/tx |
| mci-edge-fix | 5001 / 5002 | FIX 4.4 주문 |
| mci-edge-md | 5011 / 5012 | FIX 4.4 시세 |

## 사내 접근

| 서비스 | URL |
|---|---|
| Admin UI | `http://<private-ip>:9090` |
| 시세 ws | `ws://<private-ip>:8083/v1/subscribe` |
| FIX 주문 | `<private-ip>:5001` |
| FIX 시세 | `<private-ip>:5011` |
| 매매 REST | `http://<private-ip>:8080/v1/tx` |
| Metrics | `http://<private-ip>:8082/metrics` |

## 롤백

특정 git sha 로:
```bash
ssh -i winway-nh-fxallone-dev.pem rocky@<private-ip>
sudo su - winway
cd ~/wtg
sed -i 's/^WTG_VERSION=.*/WTG_VERSION=<이전 sha 7자리>/' .env
docker compose -f deploy/docker-compose.prod.yml pull
docker compose -f deploy/docker-compose.prod.yml up -d
```

또는 GitHub Actions → Run workflow → 이전 커밋 sha.

## 트러블슈팅

### 배포 job 이 계속 pending

Runner service 상태 확인:
```bash
ssh rocky@<private-ip>
cd ~/actions-runner
sudo ./svc.sh status
sudo ./svc.sh start   # 안 떠 있으면
```

### sudo 권한 없음

rocky 가 sudoer 인지 확인:
```bash
sudo -v   # password prompt 없이 통과되어야
```

### 헬스체크 실패

```bash
sudo su - winway
cd ~/wtg
docker compose -f deploy/docker-compose.prod.yml logs --tail 100 mci-price
docker compose -f deploy/docker-compose.prod.yml ps
```

가장 흔한 원인:
- broker 안 뜸 — `ss -tln | grep 11217`
- etcd 컨테이너 crash — `docker logs wtg-etcd`
- 카탈로그 파일 (etc/*.json) 문제 — 재배포로 갱신

### GHCR image pull 실패

Public 확인. private 이면:
```bash
sudo su - winway
docker login ghcr.io -u <github-user> -p <PAT>
```

## Runner 관리

### 재시작
```bash
cd ~/actions-runner
sudo ./svc.sh stop
sudo ./svc.sh start
```

### 업그레이드
```bash
sudo ./svc.sh stop
cd ~/actions-runner
./config.sh remove --token <REMOVAL_TOKEN>  # GitHub UI 에서 발급
# 새 버전 다운로드 + config + install + start
```

### 로그
```bash
journalctl -u actions.runner.changryeul-wtg.* -f
# 또는
tail -f ~/actions-runner/_diag/Runner_*.log
```

## 관측 (선택)

Prometheus + Grafana + Jaeger:
```bash
sudo su - winway
cd /path/to/wtg
docker compose -f deploy/observability/docker-compose.yml up -d
```
