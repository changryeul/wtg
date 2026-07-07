#!/usr/bin/env bash
# WTG EC2 초기 세팅 — Rocky Linux 9.7 (x86_64) 에서 1회 실행.
#
# 전제:
#   - SSH: ssh -i winway-nh-fxallone-dev.pem rocky@<host>
#   - rocky (sudoer, AMI default) 로 접속
#   - 실제 앱 실행 유저: winway (덜 특권한 앱 유저)
#   - Oracle Client + sqlplus + Pro-C 이미 설치됨 (기존 nh-fxallone-server 스택)
#   - broker (mymqd) 는 winway 계정에서 running (127.0.0.1:11217)
#
# 사용:
#   ssh -i ~/winway-nh-fxallone-dev.pem rocky@<host>
#   curl -O https://raw.githubusercontent.com/changryeul/wtg/master/deploy/setup-ec2.sh
#   bash setup-ec2.sh
#
# 이 스크립트가 하는 일:
#   1. Docker + docker compose plugin 설치 (Rocky 9)
#   2. winway 를 docker 그룹에 추가 (sudo 없이 docker 명령)
#   3. /home/winway/wtg/ 디렉토리 + 권한 세팅
#   4. GHCR public image pull 테스트
#   5. broker 상태 확인
#
# 완료 후:
#   - GitHub Actions "Deploy to EC2" workflow 실행 준비 완료

set -euo pipefail

BLUE='\033[1;34m'
GREEN='\033[1;32m'
YELLOW='\033[1;33m'
RESET='\033[0m'

step()  { printf "\n${BLUE}==> %s${RESET}\n" "$1"; }
ok()    { printf "  ${GREEN}✓${RESET} %s\n" "$1"; }
warn()  { printf "  ${YELLOW}⚠${RESET} %s\n" "$1"; }

CURRENT_USER=$(whoami)
if [ "$CURRENT_USER" != "rocky" ]; then
  warn "이 스크립트는 rocky 로 실행 권장 (현재 유저: ${CURRENT_USER})"
  warn "sudo 권한만 있으면 다른 유저에서 실행해도 무방."
fi

step "1. Docker 저장소 등록 + 설치"
if ! command -v docker >/dev/null 2>&1; then
  sudo dnf install -y dnf-plugins-core
  sudo dnf config-manager --add-repo https://download.docker.com/linux/centos/docker-ce.repo
  sudo dnf install -y docker-ce docker-ce-cli containerd.io docker-compose-plugin
  ok "Docker + docker compose plugin 설치 완료"
else
  ok "Docker 이미 설치됨: $(docker --version)"
fi

step "2. Docker 서비스 시작"
sudo systemctl enable --now docker
ok "docker.service 활성"

step "3. winway 계정 확인 + docker 그룹 등록"
if ! id winway >/dev/null 2>&1; then
  warn "winway 계정이 없음 — 먼저 계정 생성 필요 (sudo useradd -m winway 등)"
  exit 1
fi
if ! id -nG winway | grep -qw docker; then
  sudo usermod -aG docker winway
  ok "winway 를 docker 그룹에 추가 — winway 세션 재로그인 필요"
else
  ok "winway 이미 docker 그룹에 있음"
fi

step "4. /home/winway/wtg/ 디렉토리 생성"
sudo -u winway -H mkdir -p /home/winway/wtg/{deploy,etc,logs,tmp}
ok "$(sudo -u winway ls -la /home/winway/wtg | head -6)"

step "5. rocky 홈에도 staging 디렉토리 (GitHub Actions scp 목적지)"
mkdir -p /home/rocky/wtg-staging
ok "/home/rocky/wtg-staging (CI 가 파일 전송할 위치)"

step "6. GHCR public image pull 테스트 (winway 로 실행)"
if sudo -u winway -H docker pull ghcr.io/changryeul/wtg:latest 2>&1; then
  ok "이미지 pull 성공"
else
  warn "아직 이미지 없음 — 첫 GitHub Actions 배포 후 pull 가능"
fi

step "7. broker (mymqd) :11217 확인"
if ss -tln 2>/dev/null | grep -q ":11217 "; then
  ok "broker :11217 running"
else
  warn "broker :11217 안 떠 있음 — WTG 는 broker 필수 (mymqd 부팅 후 재확인)"
fi

step "8. 방화벽 (firewalld) 사내 CIDR 만 열기 — 샘플"
# 실제 사내 대역으로 조정 필요. 예시는 주석 처리.
# sudo firewall-cmd --permanent --zone=trusted --add-source=10.0.0.0/16
# sudo firewall-cmd --permanent --zone=trusted --add-port=9090/tcp   # mci-admin
# sudo firewall-cmd --permanent --zone=trusted --add-port=8080/tcp   # mci-api
# sudo firewall-cmd --permanent --zone=trusted --add-port=8083/tcp   # mci-edge-price ws
# sudo firewall-cmd --permanent --zone=trusted --add-port=5001/tcp   # mci-edge-fix
# sudo firewall-cmd --permanent --zone=trusted --add-port=5011/tcp   # mci-edge-md
# sudo firewall-cmd --reload
warn "firewalld rule 은 스크립트에 주석 처리 — 사내 CIDR 확정 후 활성"

printf "\n${GREEN}=== 세팅 완료 ===${RESET}\n"
cat <<EOF

다음 단계:
  1. winway 세션 재로그인 (docker 그룹 활성):
       exit
       ssh -i ~/winway-nh-fxallone-dev.pem rocky@<host>
       sudo su - winway
       docker ps  # 권한 없이 실행 확인

  2. GitHub 저장소 → Settings → Secrets and variables → Actions:
       SSH_HOST         = <이 EC2 의 private IP>
       SSH_KEY          = winway-nh-fxallone-dev.pem 전체 내용
       SSH_PORT         = 22 (default 사용 시 생략)
       FIX_PUSH_SECRET  = openssl rand -hex 32
     (SSH_USER=rocky, DEPLOY_USER=winway 는 workflow 에 하드코드)

  3. GHCR image 를 public 으로:
       https://github.com/changryeul?tab=packages → wtg → Settings → Public

  4. GitHub Actions → "Deploy to EC2" → Run workflow (master)

  5. 완료 후 사내 브라우저:
       http://<private-ip>:9090   (mci-admin)
       http://<private-ip>:8082/metrics  (Prometheus scrape)
EOF
