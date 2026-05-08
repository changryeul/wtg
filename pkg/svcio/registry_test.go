package svcio

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

func TestRegistryLoadDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "X1S.h"), []byte(sampleW1104), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "X2S.h"), []byte(sampleW3382), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "broken.h"), []byte("not a header"), 0o644); err != nil {
		t.Fatal(err)
	}

	r := NewRegistry()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	loaded, failed, err := r.LoadDir(dir, logger)
	if err != nil {
		t.Fatal(err)
	}
	if loaded != 2 || failed != 1 {
		t.Errorf("loaded=%d failed=%d want 2/1", loaded, failed)
	}
	if r.Count() != 2 {
		t.Errorf("count=%d want 2", r.Count())
	}

	if s, ok := r.Get("W1104S01"); !ok || s == nil || len(s.Input) == 0 {
		t.Errorf("Get W1104S01 fail: %v %v", s, ok)
	}

	all := r.List()
	if len(all) != 2 {
		t.Errorf("List len=%d want 2", len(all))
	}
	if all[0].Code > all[1].Code {
		t.Error("List 가 정렬 안 됨")
	}

	hits := r.Search("3382", 10)
	if len(hits) != 1 || hits[0].Code != "W3382S01" {
		t.Errorf("Search '3382' = %+v", hits)
	}

	emptyDir := t.TempDir()
	loaded, failed, err = r.LoadDir(emptyDir, logger)
	if err != nil || loaded != 0 || failed != 0 {
		t.Errorf("empty dir: loaded=%d failed=%d err=%v", loaded, failed, err)
	}

	loaded, failed, err = r.LoadDir("/path/that/does/not/exist", logger)
	if err != nil || loaded != 0 || failed != 0 {
		t.Errorf("missing dir should noop: %v", err)
	}
}
