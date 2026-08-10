package svcio

import (
	"fmt"
	"testing"
)

// W1422S01 출력 레이아웃 진단 — svcio 가 .h 를 파싱해 산출한 각 출력 필드의
// 오프셋/크기/총길이를 덤프하고, BuyTrnAblYn(마지막) 이 451B 전문의 offset 450 에
// 정확히 놓이는지 확인한다. (item 1: BuyTrnAblYn 빈값 = 오프셋 정합 진단)
func TestW1422S01Layout(t *testing.T) {
	spec, err := ParseFile("../../../nh/win/src/inc/trn/W1422S01.h")
	if err != nil {
		t.Skipf("nh 미러 헤더 없음 (CI/외부 환경) — skip: %v", err)
	}
	off := 0
	var buyTrnOff = -1
	for _, f := range spec.Output {
		if len(f.Children) > 0 {
			t.Logf("  [nested] %-20s repeat=%d", f.Name, f.Repeat)
			continue
		}
		fmt.Printf("off=%3d size=%2d  %s\n", off, f.Size, f.Name)
		if f.Name == "BuyTrnAblYn" {
			buyTrnOff = off
		}
		off += f.Size
	}
	fmt.Printf("== output total = %d B ==\n", off)
	if off != 451 {
		t.Errorf("output 총길이 %d, 엔진 .h 기준 451 아님 — 오프셋 정합 깨짐", off)
	}
	if buyTrnOff != 450 {
		t.Errorf("BuyTrnAblYn offset=%d, 기대 450 — 마지막 필드 정렬 깨짐", buyTrnOff)
	}
}
