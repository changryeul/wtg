#!/usr/bin/env bash
# krxshm-verify.sh — pkg/krxshm 레이아웃 상수가 실제 sise SHM 구조체(MFSISE_T)와
# byte-exact 일치하는지 검증. cside/krxshm/shmlayout.c 를 vendored 헤더로 컴파일해
# sizeof/offsetof 를 pkg/krxshm/layout.go 상수와 대조한다.
#
# ⚠️ long double(ldouble)이 플랫폼 의존(linux x86-64 16B / mac arm64 8B)이라
#    **반드시 배포 타깃(linux x86-64 = EC2)에서** 실행해야 유효. 비-linux 는 skip.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

if [ "$(uname -s)" != "Linux" ] || [ "$(uname -m)" != "x86_64" ]; then
	echo "[skip] linux/x86_64 아님 ($(uname -sm)) — SHM 레이아웃은 타깃(EC2)에서 검증"; exit 0
fi
CC="${CC:-cc}"; command -v "$CC" >/dev/null || { echo "[skip] cc 없음"; exit 0; }

OUT="$("$CC" -I "$ROOT/cside/krxshm/inc" "$ROOT/cside/krxshm/shmlayout.c" -o /tmp/shmlayout && /tmp/shmlayout)"
echo "$OUT"

# pkg/krxshm/layout.go 상수 (linux 기대값)
declare -A EXP=(
  [ShmSize]=44957040 [KBFUT_T]=112112 [kbfut]=128
  [futCd]=0 [shrtCd]=12 [fsise]=2048
  [ePrc]=32 [bPrc]=0 [yPrc]=64 [diff]=72 [sPrc]=80 [rate]=96 [sign]=103 [halt]=105 [maxN]=32 [useN]=36
)
fail=0
check(){ # key  actual
  local k="$1" a="$2"
  if [ "$a" != "${EXP[$k]}" ]; then echo "  ✗ $k: oracle=$a  go=${EXP[$k]}"; fail=1; else echo "  ✓ $k=$a"; fi
}
g(){ echo "$OUT" | grep -oE "$1[= ]+[0-9]+" | grep -oE "[0-9]+" | head -1; }
check ShmSize "$(echo "$OUT" | grep -oE 'MFSISE_SZ\(total\)=[0-9]+' | grep -oE '[0-9]+')"
check KBFUT_T "$(echo "$OUT" | grep -oE 'KBFUT_T=[0-9]+' | grep -oE '[0-9]+' | head -1)"
check kbfut   "$(echo "$OUT" | grep -oE 'kbfut@[0-9]+' | grep -oE '[0-9]+')"
check fsise   "$(echo "$OUT" | grep -oE 'fsise@[0-9]+' | grep -oE '[0-9]+')"
check ePrc    "$(echo "$OUT" | grep -oE 'ePrc@[0-9]+' | grep -oE '[0-9]+')"
check yPrc    "$(echo "$OUT" | grep -oE 'yPrc@[0-9]+' | grep -oE '[0-9]+')"
check diff    "$(echo "$OUT" | grep -oE 'diff@[0-9]+' | grep -oE '[0-9]+')"
check sPrc    "$(echo "$OUT" | grep -oE 'sPrc@[0-9]+' | grep -oE '[0-9]+')"
check rate    "$(echo "$OUT" | grep -oE 'rate@[0-9]+' | grep -oE '[0-9]+')"
check sign    "$(echo "$OUT" | grep -oE 'sign@[0-9]+' | grep -oE '[0-9]+')"
[ "$fail" = 0 ] && echo "[OK] pkg/krxshm 상수 ↔ 실제 MFSISE_T 레이아웃 일치" || { echo "[FAIL] 불일치 — layout.go 재확정 필요"; exit 1; }
