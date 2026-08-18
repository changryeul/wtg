package svcio

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

// TestPerFirmComhdr — 회사(디렉터리)별로 레이아웃이 다른 COMHDR 를 각각 로드하고,
// 각 서비스가 자기 회사 COMHDR 로 조립되는지 검증한다. NH usid[30] / yuanta usid[16].
func TestPerFirmComhdr(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	root := t.TempDir()

	// 회사별 inc-dir(.../win/src/inc/trn) + 형제 comhdr(.../inc/com/comhdr.h) 구성.
	mk := func(firm, usidWidth, code string) string {
		base := filepath.Join(root, firm, "win", "src", "inc")
		if err := os.MkdirAll(filepath.Join(base, "com"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(base, "trn"), 0o755); err != nil {
			t.Fatal(err)
		}
		comhdr := "typedef struct {\n char usid[" + usidWidth + "];\n char rcod[5];\n} COMHDR;\n"
		if err := os.WriteFile(filepath.Join(base, "com", "comhdr.h"), []byte(comhdr), 0o644); err != nil {
			t.Fatal(err)
		}
		svc := "typedef struct {  // Input\n char a[4];\n} " + code + "_I;\ntypedef struct {  // Output\n char b[4];\n} " + code + "_O;\n"
		if err := os.WriteFile(filepath.Join(base, "trn", code+".h"), []byte(svc), 0o644); err != nil {
			t.Fatal(err)
		}
		return filepath.Join(base, "trn")
	}
	nhDir := mk("nh", "30", "W1200S01")
	yuDir := mk("yuanta", "16", "T4201S03")

	r := NewRegistry()
	// server.go 배선과 동일: 각 inc-dir 의 형제 comhdr 를 회사 네임스페이스로 로드 + dir default 연결.
	if err := r.LoadHeaderFileAs(filepath.Join(filepath.Dir(nhDir), "com", "comhdr.h"), "_NH", logger); err != nil {
		t.Fatal(err)
	}
	if err := r.LoadHeaderFileAs(filepath.Join(filepath.Dir(yuDir), "com", "comhdr.h"), "_YUANTA", logger); err != nil {
		t.Fatal(err)
	}
	r.SetDirHeaderDefault(nhDir, "COMHDR_NH")
	r.SetDirHeaderDefault(yuDir, "COMHDR_YUANTA")
	if _, _, err := r.LoadDir(nhDir, logger); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.LoadDir(yuDir, logger); err != nil {
		t.Fatal(err)
	}

	usidWidth := func(code string) int {
		s, ok := r.Get(code)
		if !ok {
			t.Fatalf("spec %s 미등록", code)
		}
		for _, f := range s.HeaderFields {
			if f.Name == "usid" {
				return f.Size
			}
		}
		t.Fatalf("spec %s HeaderFields 에 usid 없음 (HeaderType=%q, fields=%d)", code, s.HeaderType, len(s.HeaderFields))
		return 0
	}
	if w := usidWidth("W1200S01"); w != 30 {
		t.Errorf("NH W1200S01 usid width=%d want 30 (NH COMHDR)", w)
	}
	if w := usidWidth("T4201S03"); w != 16 {
		t.Errorf("yuanta T4201S03 usid width=%d want 16 (yuanta COMHDR)", w)
	}
}

// TestDefaultHeaderForLongestPrefix — 일반 prefix 와 구체 prefix 가 둘 다 매칭될 때
// 더 긴(구체) prefix 가 이긴다.
func TestDefaultHeaderForLongestPrefix(t *testing.T) {
	r := NewRegistry()
	r.SetDirHeaderDefault("win/src/inc/trn", "COMHDR")
	r.SetDirHeaderDefault("yuanta/win/src/inc/trn", "COMHDR_YUANTA")
	r.mu.RLock()
	defer r.mu.RUnlock()
	if got := r.defaultHeaderFor("/home/x/projects/yuanta/win/src/inc/trn/T1.h"); got != "COMHDR_YUANTA" {
		t.Errorf("yuanta 경로 defaultHeaderFor=%q want COMHDR_YUANTA", got)
	}
	if got := r.defaultHeaderFor("/home/x/projects/nh/win/src/inc/trn/W1.h"); got != "COMHDR" {
		t.Errorf("nh 경로 defaultHeaderFor=%q want COMHDR", got)
	}
}
