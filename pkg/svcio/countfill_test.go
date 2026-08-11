package svcio

import (
	"strings"
	"testing"
)

// 가변 grid(orec[]) 바로 앞의 count 필드(nrec 등)가 입력 JSON 에 없어도
// grid 행 수로 자동 채워지는지 — 클라가 orec 배열만 보내는 실제 케이스.
// (미채움 시 count 공백 → 수신 AP 건수 파싱 실패 → 전체 유실. W9501S03 실장애 회귀)
func TestSerializeAutoFillCount(t *testing.T) {
	spec := &SvcSpec{
		Input: []Field{
			{Name: "nrec", CType: "char", Size: 6},
			{Name: "orec", CType: "struct", Repeat: -1, Children: []Field{
				{Name: "exnm", CType: "char", Size: 16},
				{Name: "symb", CType: "char", Size: 16},
				{Name: "pay_ymd", CType: "char", Size: 16},
				{Name: "exp_ymd", CType: "char", Size: 16},
			}},
		},
	}
	in := map[string]interface{}{
		"orec": []interface{}{
			map[string]interface{}{"exnm": "BEST", "symb": "USD/KRW"},
			map[string]interface{}{"exnm": "BEST", "symb": "EUR/KRW"},
		},
	}
	b, err := Serialize(spec.Input, in)
	if err != nil {
		t.Fatalf("Serialize: %v", err)
	}
	if len(b) != 6+2*64 {
		t.Fatalf("len=%d, want 134 (6 + 2*64)", len(b))
	}
	if nrec := strings.TrimSpace(string(b[:6])); nrec != "2" {
		t.Errorf("nrec 자동채움=%q, want \"2\"", nrec)
	}
	if symb1 := strings.TrimSpace(string(b[6+64+16 : 6+64+32])); symb1 != "EUR/KRW" {
		t.Errorf("row1 symb=%q, want EUR/KRW", symb1)
	}

	// 명시적 nrec 이 있으면 그대로 존중 (덮어쓰지 않음).
	in["nrec"] = "9"
	b2, _ := Serialize(spec.Input, in)
	if nrec := strings.TrimSpace(string(b2[:6])); nrec != "9" {
		t.Errorf("명시 nrec=%q, want \"9\" (자동채움이 덮으면 안됨)", nrec)
	}
}
