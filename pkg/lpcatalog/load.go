package lpcatalog

import (
	"encoding/json"
	"fmt"
	"os"
)

// LoadFile — JSON 배열 파일에서 LP 목록을 읽는다 (config 파일 모드, etc/lp.json).
// etcd 미사용 dev/PoC 또는 etcd 초기 seed 원본. 형식:
//
//	[
//	  {"code":"SMB","category":"broker","group":"227.10.40.11","port":45010,"active":true},
//	  ...
//	]
func LoadFile(path string) ([]LP, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("lpcatalog: 파일 읽기 (%s): %w", path, err)
	}
	var items []LP
	if err := json.Unmarshal(b, &items); err != nil {
		return nil, fmt.Errorf("lpcatalog: JSON 파싱 (%s): %w", path, err)
	}
	return items, nil
}
