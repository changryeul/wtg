#!/usr/bin/env bash
# krx-verify.sh — KRX 원 TR 파서 런타임 대조 (C 오라클 ↔ WTG Go 디코더).
#
# 결정적 원 TR 캡처를 생성 → (1) C 오라클(실제 sise 구조체 캐스팅)과
# (2) WTG Go 디코더(pkg/krx)로 각각 CSV 를 뽑아 diff. 오프셋/파싱이 C 구조체
# 레이아웃과 어긋나면 diff 로 드러난다. docs/krx-sise-design.md §11.7.
#
# 사용:  scripts/krx-verify.sh [sise_inc_dir]
#   기본값: 리포 내 vendored 헤더 cside/krxverify/inc (self-contained — 외부 sise 폴더 불요).
#   원 sise 폴더로 대조하려면 그 inc 경로를 인자로 전달.
# cc 만 있으면 동작(없으면 skip). vendored .h 는 순수 char[] struct (무의존).
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
INC="${1:-$ROOT/cside/krxverify/inc}"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

CC="${CC:-cc}"
if ! command -v "$CC" >/dev/null 2>&1; then
	echo "[skip] C 컴파일러($CC) 없음 — 런타임 대조 생략"; exit 0
fi
if [ ! -f "$INC/A306F.h" ]; then
	echo "[skip] sise 헤더 없음: $INC (인자로 경로 지정 가능) — 생략"; exit 0
fi

echo "== 1) C 오라클 빌드 (inc=$INC)"
"$CC" -O2 -I "$INC" "$ROOT/cside/krxverify/oracle.c" -o "$WORK/oracle"

echo "== 2) 결정적 캡처 생성"
( cd "$ROOT" && go run ./cmd/krx-verify gen "$WORK/cap.dat" )

echo "== 3) C 오라클 CSV"
"$WORK/oracle" "$WORK/cap.dat" | sort > "$WORK/c.csv"

echo "== 4) Go 디코더 CSV"
( cd "$ROOT" && go run ./cmd/krx-verify decode "$WORK/cap.dat" ) | sort > "$WORK/go.csv"

echo "== 5) diff (C ↔ Go)"
if diff -u "$WORK/c.csv" "$WORK/go.csv"; then
	echo "[OK] C 오라클 ↔ Go 디코더 값 일치 ($(wc -l < "$WORK/c.csv") 레코드)"
else
	echo "[FAIL] 불일치 — 위 diff 확인 (오프셋/파싱 어긋남)"; exit 1
fi
