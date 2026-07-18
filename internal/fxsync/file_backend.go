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
//
//	<Dir>/currency.json          ← Currencies (JSON 배열)
//	<Dir>/pair.json              ← (Step 4 에서 추가)
//	<Dir>/pair_product.json      ← (Step 5 에서 추가, cross 산식)
//	<Dir>/swap_point.json        ← (Step 6)
//	<Dir>/margin.json            ← (Step 7)
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
	var out Currencies
	if err := b.readJSON("currency.json", &out); err != nil {
		return nil, err
	}
	return out, nil
}

// LoadPairs — pair.json 읽어 Pairs 반환.
func (b *FileBackend) LoadPairs(_ context.Context) (Pairs, error) {
	var out Pairs
	if err := b.readJSON("pair.json", &out); err != nil {
		return nil, err
	}
	return out, nil
}

// LoadSwapPoints — swap_point.json.
func (b *FileBackend) LoadSwapPoints(_ context.Context) (SwapPoints, error) {
	var out SwapPoints
	if err := b.readJSON("swap_point.json", &out); err != nil {
		return nil, err
	}
	return out, nil
}

// LoadHQMargins — hq_margin.json.
func (b *FileBackend) LoadHQMargins(_ context.Context) (HQMargins, error) {
	var out HQMargins
	if err := b.readJSON("hq_margin.json", &out); err != nil {
		return nil, err
	}
	return out, nil
}

// LoadSiteMargins — site_margin.json.
func (b *FileBackend) LoadSiteMargins(_ context.Context) (SiteMargins, error) {
	var out SiteMargins
	if err := b.readJSON("site_margin.json", &out); err != nil {
		return nil, err
	}
	return out, nil
}

// LoadUserProfiles — user_profile.json (이미 enum 형태 {usid,site,tier,active}) 읽기.
// Oracle backend 는 raw 등급코드를 GradeMapper 로 변환하지만, dev file 은 변환 완료본.
func (b *FileBackend) LoadUserProfiles(_ context.Context) (UserProfiles, error) {
	var out UserProfiles
	if err := b.readJSON("user_profile.json", &out); err != nil {
		return nil, err
	}
	return out, nil
}

// readJSON — 파일 read + JSON unmarshal. 누락 파일은 빈 결과 + nil err (호출자
// 가 v 의 zero value 유지).
func (b *FileBackend) readJSON(filename string, v any) error {
	path := filepath.Join(b.Dir, filename)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("fxsync: read %s: %w", path, err)
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("fxsync: parse %s: %w", path, err)
	}
	return nil
}
