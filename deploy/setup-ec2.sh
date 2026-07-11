#!/usr/bin/env bash
# WTG EC2 초기 세팅 — Rocky Linux 9.7 (x86_64) 에서 1회 실행. docker 미사용.
#
# 전제:
#   - SSH: ssh -i winway-nh-fxallone-dev.pem rocky@<host>
#   - rocky (sudoer, AMI default) 로 접속
#   - 실제 앱 실행 유저: winway (덜 특권한 앱 유저)
#   - broker (mymqd) 는 winway 계정에서 running (127.0.0.1:11217)
#
# 사용:
#   ssh -i ~/winway-nh-fxallone-dev.pem rocky@<host>
#   curl -O https://raw.githubusercontent.com/changryeul/wtg/master/deploy/setup-ec2.sh
#   bash setup-ec2.sh
#
# 이 스크립트가 하는 일:
#   1. etcd native 바이너리 설치 (/usr/local/bin — systemd 로 실행)
#   2. /home/winway/nh-fxallone-server/wtg/ 디렉토리 + 권한 세팅
#   3. broker 상태 확인
#   4. firewalld 샘플 안내 (주석)
#
# 완료 후:
#   - self-hosted runner 등록 (deploy/README.md §2)
#   - GitHub Actions "Deploy to EC2" workflow 실행 준비 완료

set -euo pipefail

ETCD_VER=v3.5.21
WTG_HOME=/home/winway/nh-fxallone-server/wtg

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

step "1. winway 계정 확인"
if ! id winway >/dev/null 2>&1; then
  warn "winway 계정이 없음 — 먼저 계정 생성 필요 (sudo useradd -m winway 등)"
  exit 1
fi
ok "winway 계정 존재"

step "2. rsync 확인 (소스 미러 동기화용)"
if command -v rsync >/dev/null 2>&1; then
  ok "rsync 설치됨"
else
  sudo dnf install -y rsync
  ok "rsync 설치 완료"
fi

step "3. etcd native 바이너리 설치 (${ETCD_VER})"
if command -v etcd >/dev/null 2>&1; then
  ok "etcd 이미 설치됨: $(etcd --version | head -1)"
else
  curl -fsSL -o /tmp/etcd.tar.gz \
    "https://github.com/etcd-io/etcd/releases/download/${ETCD_VER}/etcd-${ETCD_VER}-linux-amd64.tar.gz"
  tar xzf /tmp/etcd.tar.gz -C /tmp
  sudo install -m 0755 "/tmp/etcd-${ETCD_VER}-linux-amd64/etcd" \
                       "/tmp/etcd-${ETCD_VER}-linux-amd64/etcdctl" /usr/local/bin/
  rm -rf /tmp/etcd.tar.gz "/tmp/etcd-${ETCD_VER}-linux-amd64"
  ok "etcd + etcdctl → /usr/local/bin"
fi

step "4. ${WTG_HOME} 디렉토리 생성"
sudo install -d -o winway -g winway \
  "$WTG_HOME" "$WTG_HOME/bin" "$WTG_HOME/etc" \
  "$WTG_HOME/data/etcd" "$WTG_HOME/data/fix"
ok "$(sudo ls -la "$WTG_HOME" | head -8)"

step "5. JWT RSA 키쌍 생성 (mci-api 발급 / edge 검증 — 1회)"
if sudo test -f "$WTG_HOME/etc/jwt-private.pem"; then
  ok "jwt-private.pem 이미 존재"
else
  sudo -u winway bash -c "
    umask 077
    openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:2048 \
      -out '$WTG_HOME/etc/jwt-private.pem' 2>/dev/null
    openssl pkey -in '$WTG_HOME/etc/jwt-private.pem' -pubout \
      -out '$WTG_HOME/etc/jwt-public.pem' 2>/dev/null
    chmod 600 '$WTG_HOME/etc/jwt-private.pem'
    chmod 644 '$WTG_HOME/etc/jwt-public.pem'"
  ok "jwt-private.pem (600) + jwt-public.pem 생성"
fi

step "5b. edge TLS 자가서명 cert 생성 (mci-edge-api 8090 — dev 전용, 1회)"
if sudo test -f "$WTG_HOME/etc/edge-tls.crt"; then
  ok "edge-tls.crt 이미 존재"
else
  sudo -u winway bash -c "
    umask 077
    openssl req -x509 -newkey rsa:2048 -nodes -days 3650 \
      -subj '/CN=wtg-edge-dev' \
      -addext 'subjectAltName=DNS:localhost,IP:127.0.0.1' \
      -keyout '$WTG_HOME/etc/edge-tls.key' \
      -out '$WTG_HOME/etc/edge-tls.crt' 2>/dev/null
    chmod 600 '$WTG_HOME/etc/edge-tls.key'
    chmod 644 '$WTG_HOME/etc/edge-tls.crt'"
  ok "edge-tls.crt + edge-tls.key 생성 (자가서명 — 운영 전환 시 정식 인증서로 교체)"
fi

step "6. SELinux fcontext 등록 (systemd 가 홈 안 바이너리/설정 접근 허용)"
if command -v getenforce >/dev/null 2>&1 && [ "$(getenforce)" != "Disabled" ]; then
  command -v semanage >/dev/null 2>&1 || sudo dnf install -y -q policycoreutils-python-utils
  sudo semanage fcontext -a -t bin_t     "$WTG_HOME/bin(/.*)?"    2>/dev/null || true
  sudo semanage fcontext -a -t etc_t     "$WTG_HOME/wtg\\.env"    2>/dev/null || true
  sudo semanage fcontext -a -t etc_t     "$WTG_HOME/etc(/.*)?"    2>/dev/null || true
  sudo semanage fcontext -a -t var_lib_t "$WTG_HOME/data(/.*)?"   2>/dev/null || true
  sudo restorecon -R "$WTG_HOME"
  ok "bin_t / etc_t / var_lib_t fcontext 등록 + restorecon"
else
  ok "SELinux 비활성 — skip"
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
# sudo firewall-cmd --permanent --zone=trusted --add-port=5001/tcp   # mci-edge-fix-ord
# sudo firewall-cmd --permanent --zone=trusted --add-port=5011/tcp   # mci-edge-fix-md
# sudo firewall-cmd --reload
warn "firewalld rule 은 스크립트에 주석 처리 — 사내 CIDR 확정 후 활성"

printf "\n${GREEN}=== 세팅 완료 ===${RESET}\n"
cat <<EOF

다음 단계:
  1. GitHub self-hosted runner 등록 (아직 안 했으면):
       https://github.com/changryeul/wtg/settings/actions/runners/new
       → Linux/x64 → UI 명령대로 config.sh + svc.sh install rocky
       (상세: deploy/README.md §2)

  2. GitHub 저장소 → Settings → Secrets and variables → Actions:
       FIX_PUSH_SECRET  = openssl rand -hex 32

  3. GitHub Actions → "Deploy to EC2" → Run workflow (master)
     → 바이너리 + systemd unit 설치 + 서비스 기동은 workflow 가 수행

  4. 완료 후 사내 브라우저:
       http://<private-ip>:9090            (mci-admin)
       http://<private-ip>:8082/metrics    (Prometheus scrape)
EOF
