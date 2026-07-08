# WTG EC2 배포 가이드 (native binary + systemd, docker 미사용)

로컬 mac 개발환경 → AWS EC2 (nh-fxallone-dev, Rocky Linux 9.7, t3.xlarge)
배포. **GitHub Actions self-hosted runner** + **native 바이너리 + systemd** 기반.

## 설계 요약

- **EC2 에는 docker 를 쓰지 않는다** (운영 결정). Go 는 cgo 없는 static
  binary 라 GitHub-hosted 에서 크로스 빌드 (`CGO_ENABLED=0 GOOS=linux`) →
  artifact 로 전달. EC2 에는 Go toolchain 도 불필요.
- 서비스는 **systemd unit** (`deploy/ec2/*.service`, `User=winway`) 으로 상주.
- etcd 도 **native 바이너리** — `setup-ec2.sh` 가 `/usr/local/bin` 에 1회 설치.
- Test / build 는 GitHub-hosted (ubuntu-latest). Deploy 만 self-hosted.

## 왜 self-hosted runner

- EC2 는 사내 CIDR 만 접근 (private IP). GitHub-hosted runner 는 수천 개
  public IP 대역이라 SG 관리 불가.
- Self-hosted runner 는 사내 EC2 안에서 workflow 실행 → private IP 접근 OK,
  SSH secrets 불필요, 방화벽 rule 그대로 유지.

## EC2 환경 전제

- Rocky Linux 9.7 (x86_64), t3.xlarge
- Oracle Client + sqlplus + Pro-C (+ ORACLE_HOME) 이미 세팅
- broker (mymqd) 는 winway 계정에서 running (127.0.0.1:11217)
- SSH 접속 유저: **rocky** (Rocky AMI default sudoer)
- 앱 실행 유저: **winway** (덜 특권한 앱 유저)
- Runner 실행 유저: **rocky** (sudoer 필요 — deploy job 이 sudo 사용)

## 배포 위치 (`/home/winway/nh-fxallone-server/wtg/`)

```
/home/winway/nh-fxallone-server/wtg/
├── bin/          # 서비스 바이너리 (CI 가 매 배포 갱신)
├── bin.prev/     # 직전 배포 바이너리 (롤백용)
├── etc/          # 카탈로그 (symbols/pricing/profiles.json — CI 가 갱신)
├── src/          # 전체 소스 미러 (.git 포함 — CI 가 매 배포 동기화, 열람/diff 용)
├── data/
│   ├── etcd/     # etcd data dir (운영 SoT — 배포와 무관하게 보존)
│   └── fix/      # mci-edge-fix message store (FIX seq 보존)
├── wtg.env       # BROKER_HOST/PORT + FIX_PUSH_SECRET (CI 가 Secret 으로 갱신, 600)
└── VERSION       # 배포된 git sha (7자리)
```

## 흐름 전체 그림

```
로컬 mac              GitHub Actions               EC2 (self-hosted runner, rocky)
────────              ──────────────               ──────────────────────────
git push master  →    test (ubuntu-latest)
                      ↓
                      make build (linux/amd64,
                      CGO_ENABLED=0, ubuntu-latest)
                      ↓
                      artifact upload ──────→     deploy job (self-hosted):
                                                  · checkout + artifact download
                                                  · systemctl stop 'wtg-mci-*'
                                                  · bin/etc/unit 설치 (sudo)
                                                  · systemctl enable --now wtg-*
                                                  · is-active + curl healthcheck
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
- etcd native 바이너리 설치 (`/usr/local/bin/etcd`, `etcdctl`)
- `/home/winway/nh-fxallone-server/wtg/{bin,etc,data/etcd,data/fix}` 생성
- broker 상태 확인

### 2. GitHub Actions self-hosted runner 설치

**GitHub 저장소에서 registration token 발급**:
- `https://github.com/changryeul/wtg/settings/actions/runners/new`
- **Linux / x64** 선택 → 화면에 나오는 명령 복사

**EC2 rocky 세션에서**:
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

| Secret | 값 |
|---|---|
| `FIX_PUSH_SECRET` | `openssl rand -hex 32` 결과 |

(이전 docker/SSH 흐름의 `SSH_HOST` / `SSH_KEY` / `SSH_PORT` 는 삭제해도 됨.)

### 4. 첫 배포

`https://github.com/changryeul/wtg/actions`
→ **Deploy to EC2** → **Run workflow** → **master**

systemd unit 설치 / 서비스 기동은 workflow 가 수행 — EC2 에서 수동 작업 없음.

## 이후 배포

`master` push 마다 자동. paths-ignore 조건 (`docs/**`, `**.md` 등) 은 스킵.

## 배포된 서비스

같은 EC2, 전부 host 프로세스 (systemd):

| systemd unit | 포트 | 역할 |
|---|---|---|
| (broker, mymqd) | 11217 | 기존 (WTG 외부, winway 수동 관리) |
| `wtg-etcd` | 2379 | 카탈로그 SoT (native etcd) |
| `wtg-mci-price` | 8082 / 50051 | 시세 코어 + AlgoStream |
| `wtg-mci-edge-price` | 8083 | 사용자 ws fan-out |
| `wtg-mci-admin` | 9090 | 운영 콘솔 |
| `wtg-mci-api` | 8080 | /v1/tx |
| `wtg-mci-edge-fix` | 5001 / 5002 | FIX 4.4 주문 |
| `wtg-mci-edge-md` | 5011 / 5012 | FIX 4.4 시세 |
| `wtg-quote-forwarder` | UDP 30044/30045, 9091 (stats) | UDP 시세 → broker publish (SMB/KMB 2-feed) |
| `wtg-dev-feed` | — | **개발용** load-gen 데모 시세 60 tick/s. 실 cooker 전환 시 unit 파일 삭제 후 재배포 |

## 사내 접근

| 서비스 | URL |
|---|---|
| Admin UI | `http://<private-ip>:9090` |
| 시세 ws | `ws://<private-ip>:8083/v1/subscribe` |
| FIX 주문 | `<private-ip>:5001` |
| FIX 시세 | `<private-ip>:5011` |
| 매매 REST | `http://<private-ip>:8080/v1/tx` |
| Metrics | `http://<private-ip>:8082/metrics` |

## 운영 명령 (EC2)

```bash
# 상태 / 로그
sudo systemctl status 'wtg-*'
sudo journalctl -u wtg-mci-price -f

# 재시작 (전체 / 개별)
sudo systemctl restart 'wtg-mci-*'
sudo systemctl restart wtg-mci-price

# 배포된 버전
cat /home/winway/nh-fxallone-server/wtg/VERSION
```

## 롤백

직전 배포로 (bin.prev):
```bash
ssh -i winway-nh-fxallone-dev.pem rocky@<private-ip>
sudo systemctl stop 'wtg-mci-*'
sudo rm -rf /home/winway/nh-fxallone-server/wtg/bin
sudo mv /home/winway/nh-fxallone-server/wtg/bin.prev /home/winway/nh-fxallone-server/wtg/bin
sudo systemctl start wtg-mci-price wtg-mci-edge-price wtg-mci-admin \
  wtg-mci-api wtg-mci-edge-fix wtg-mci-edge-md
```

특정 커밋으로: GitHub Actions → Run workflow → 해당 커밋 sha 선택
(또는 revert commit push).

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

deploy job 이 inactive 서비스의 journal 마지막 40줄을 자동 출력. 직접 보려면:
```bash
sudo systemctl status 'wtg-*'
sudo journalctl -u wtg-mci-price -n 100 --no-pager
```

가장 흔한 원인:
- broker 안 뜸 — `ss -tln | grep 11217`. mci-price / mci-admin / mci-api 는
  broker 연결 실패 시 종료 → systemd 가 재시도 (`connection refused` 로그).
  broker 기동되면 자동 복구.
- etcd 안 뜸 — `sudo journalctl -u wtg-etcd -n 50`
- 카탈로그 파일 (etc/*.json) 문제 — 재배포로 갱신
- `wtg.env` 누락/권한 — CI 가 매 배포 갱신 (600, winway)

### SELinux 로 서비스 기동 실패

`Failed to run 'start' task: Permission denied` / `Failed to load environment
files: Permission denied` (result 'resources') 는 SELinux — 홈 디렉토리 안
파일이 `user_home_t` 라벨이라 systemd 가 exec/read 거부.

setup-ec2.sh §5 가 fcontext 룰 (bin→`bin_t`, etc/wtg.env→`etc_t`,
data→`var_lib_t`) 을 등록하고, deploy job 이 매 배포 후 `restorecon -R` 로
재적용한다. 수동 확인:
```bash
sudo ls -Z /home/winway/nh-fxallone-server/wtg/bin/mci-price  # bin_t 여야 정상
sudo restorecon -R /home/winway/nh-fxallone-server/wtg        # 라벨 재적용
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

## 참고

- `deploy/ec2/` — EC2 용 systemd unit (이 배포의 실체)
- `deploy/systemd/` — 대형 HA/TLS 시나리오용 범용 템플릿 (docs/operations 참조용, 본 EC2 배포와 별개)
- `deploy/observability/` — Prometheus/Grafana/Jaeger 스택 (선택, docker compose — EC2 가 아닌 별도 관측 호스트에서 실행)
