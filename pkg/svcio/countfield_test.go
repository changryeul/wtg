package svcio

import "testing"

// 선언된 count 필드(rec1)는 위치추측(isCountFieldName)이 놓치는데,
// applyDeclaredCount 로 정확히 가변 grid + CountField 지정되고 디코드가 N행을 읽는다.
func TestDeclaredCount_NonHeuristicName(t *testing.T) {
	mkFields := func() []Field {
		return []Field{
			{Name: "rec1", CType: "char", Size: 4},
			{Name: "orec", Repeat: 1, Children: []Field{{Name: "a", CType: "char", Size: 2}}},
		}
	}
	// rec1 은 count-like 이름이 아니므로 휴리스틱은 가변으로 안 봄.
	if isCountFieldName("rec1") {
		t.Fatal("전제 오류: rec1 이 휴리스틱에 잡히면 이 테스트 의미 없음")
	}

	// 3행 wire: rec1="0003" + "AA"+"BB"+"CC".
	buf := []byte("0003" + "AABBCC")

	// (1) 선언 미적용 — orec.Repeat=1 → 1행만 (N-1 손실 = 버그 재현).
	base := mkFields()
	out, err := Deserialize(base, buf)
	if err != nil {
		t.Fatal(err)
	}
	if rows, _ := out["orec"].([]map[string]interface{}); len(rows) != 1 {
		t.Errorf("선언 미적용: orec %d행, want 1 (위치추측이 rec1 못 잡음)", len(rows))
	}

	// (2) 선언 적용 — applyDeclaredCount("rec1") → 가변 + CountField=rec1 → 3행.
	decl := mkFields()
	if !applyDeclaredCount(decl, "rec1") {
		t.Fatal("applyDeclaredCount 가 rec1 적용 실패")
	}
	if decl[1].Repeat != -1 || decl[1].CountField != "rec1" {
		t.Fatalf("orec Repeat=%d CountField=%q, want -1/rec1", decl[1].Repeat, decl[1].CountField)
	}
	out2, err := Deserialize(decl, buf)
	if err != nil {
		t.Fatal(err)
	}
	rows, _ := out2["orec"].([]map[string]interface{})
	if len(rows) != 3 {
		t.Fatalf("선언 적용: orec %d행, want 3", len(rows))
	}
	if rows[0]["a"] != "AA" || rows[1]["a"] != "BB" || rows[2]["a"] != "CC" {
		t.Errorf("행 내용 오류: %+v", rows)
	}
}

// CountField 가 "직전 필드"가 아니어도(사이에 다른 필드) 이름으로 정확히 읽는다.
func TestDeclaredCount_NotImmediatelyBefore(t *testing.T) {
	fields := []Field{
		{Name: "grid01_cnt", CType: "char", Size: 4},
		{Name: "filler", CType: "char", Size: 3}, // count 와 orec 사이 끼어듦
		{Name: "orec", Repeat: 1, Children: []Field{{Name: "a", CType: "char", Size: 2}}},
	}
	if !applyDeclaredCount(fields, "grid01_cnt") {
		t.Fatal("적용 실패")
	}
	// wire: cnt="0002" + filler="XYZ" + 2행.
	buf := []byte("0002" + "XYZ" + "AABB")
	out, err := Deserialize(fields, buf)
	if err != nil {
		t.Fatal(err)
	}
	rows, _ := out["orec"].([]map[string]interface{})
	if len(rows) != 2 {
		t.Fatalf("orec %d행, want 2 (위치추측이면 filler=XYZ→0행)", len(rows))
	}
}
