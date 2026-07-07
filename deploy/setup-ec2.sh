#!/usr/bin/env bash
# WTG EC2 초기 세팅 — Rocky Linux 9.7 (x86_64) 에서 1회 실행.
#
# 사용:
#   ssh winway@10.0.1.106
#   curl -O https://raw.githubusercontent.com/changryeul/wtg/master/deploy/setup-ec2.sh
#   bash setup-ec2.sh
#
# 이 스크립트가 하는 일:
#   1. Docker + docker compose plugin 설치 (Rocky Linux 9)
#   2. sudo 그룹에 사용자 추가 (docker 명령을 sudo 없이)
#   3. ~/wtg/ 디렉토리 + 볼륨 마운트 위치
#   4. GHCR public image pull 확인
#
# 완료 후:
#   - GitHub Actions "Deploy to EC2" 워크플로우 실행 준비 완료
#   - 처음엔 GitHub Actions → Run workflow → master 배포
#   - 이후엔 master push 마다 자동 배포

set -euo pipefail

BLUE='\033[1;34m'
RESET='\033[0m'

step() { printf "\n${BLUE}==> %s${RESET}\n" "$1"; }

step "1. Docker 저장소 등록 (docker-ce.repo)"
if ! command -v docker >/dev/null 2>&1; then
  sudo dnf install -y dnf-plugins-core
  sudo dnf config-manager --add-repo https://download.docker.com/linux/centos/docker-ce.repo
  sudo dnf install -y docker-ce docker-ce-cli containerd.io docker-compose-plugin
else
  echo "  docker 이미 설치됨: $(docker --version)"
fi

step "2. Docker 서비스 시작 + user 등록"
sudo systemctl enable --now docker
if ! id -nG "$USER" | grep -qw docker; then
  sudo usermod -aG docker "$USER"
  echo "  ${USER} 를 docker 그룹에 추가 — 로그아웃 후 재로그인 필요"
fi

step "3. 디렉토리 구조"
mkdir -p "$HOME/wtg/deploy" "$HOME/wtg/etc" "$HOME/wtg/logs" "$HOME/wtg/tmp"
echo "  ${HOME}/wtg/"
ls -la "$HOME/wtg"

step "4. GHCR public image pull 테스트"
if docker pull ghcr.io/changryeul/wtg:latest 2>&1; then
  echo "  이미지 pull 성공"
else
  echo "  아직 이미지 없음 — 첫 GitHub Actions 배포 후 pull 가능"
fi

step "5. 기존 broker (mymqd) 확인"
if ss -tln 2>/dev/null | grep -q ":11217 "; then
  echo "  broker :11217 running 확인"
else
  echo "  ⚠ broker :11217 가 안 떠 있음 — WTG 는 broker 필수 (mymqd 부팅 후 재시도)"
fi

step "6. 방화벽 (firewalld) — 사내 CIDR 만 열기 (샘플)"
# 아래는 예시. 실제 사내 대역으로 조정 필요.
# sudo firewall-cmd --permanent --zone=trusted --add-source=10.0.0.0/16
# sudo firewall-cmd --permanent --zone=trusted --add-port=9090/tcp   # mci-admin
# sudo firewall-cmd --permanent --zone=trusted --add-port=8083/tcp   # mci-edge-price ws
# sudo firewall-cmd --permanent --zone=trusted --add-port=5001/tcp   # mci-edge-fix
# sudo firewall-cmd --permanent --zone=trusted --add-port=5011/tcp   # mci-edge-md
# sudo firewall-cmd --reload
echo "  방화벽 rule 은 setup-ec2.sh 안에 주석 처리 — 사내 CIDR 확정 후 열기"

printf "\n${BLUE}=== 세팅 완료 ===${RESET}\n"
cat <<EOF

다음 단계:
  1. 로그아웃 후 재로그인 (docker 그룹 활성)
  2. GitHub 저장소 → Settings → Secrets and variables → Actions:
     - SSH_HOST         = ${HOSTNAME} 의 private IP (예: 10.0.1.106)
     - SSH_USER         = ${USER}
     - SSH_KEY          = pem 파일 전체 내용
     - SSH_PORT         = 22 (필요시)
     - FIX_PUSH_SECRET  = mci-edge-fix drop copy 인증 값 (임의 랜덤)
  3. GitHub Actions → "Deploy to EC2" → Run workflow (master)
  4. 배포 완료 후 사내 브라우저에서 http://${HOSTNAME}:9090 접속
EOF
