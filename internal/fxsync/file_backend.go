package fxsync

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// FileBackend — JSON 파일에서 마스터 데이터 read. dev / 테스트용.
//
// 디렉토리 구조:
//   <Dir>/currency.json          ← Currencies (JSON 배열)
//   <Dir>/pair.json              ← (Step 4 에서 추가)
//   <Dir>/pair_product.json      ← (Step 5 에서 추가, cross 산식)
//   <Dir>/swap_point.json        ← (Step 6)
//   <Dir>/margin.json            ← (Step 7)
//
// 누락된 파일은 빈 슬라이스 반환 (sync 가 그 항목 무시).
type FileBackend struct {
	Dir string
}

// NewFileBackend — dir 안의 *.json 을 source 로.
func NewFileBackend(dir string) *FileBackend {
	return &FileBackend{Dir: dir}
}

// LoadCurrencies — currency.json 읽어 Currencies 반환.
func (b *FileBackend) LoadCurrencies(_ context.Context) (Currencies, error) {
	path := filepath.Join(b.Dir, "currency.json")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Currencies{}, nil
		}
		return nil, fmt.Errorf("fxsync: read %s: %w", path, err)
	}
	var out Currencies
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("fxsync: parse %s: %w", path, err)
	}
	return out, nil
}
