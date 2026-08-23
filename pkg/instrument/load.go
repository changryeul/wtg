package instrument

import (
	"encoding/json"
	"fmt"
	"os"
)

// LoadFile — JSON 배열 파일에서 Instrument 목록을 읽는다 (정적 파일 모드,
// etc/instruments.json 등). etcd 미사용 dev/PoC 용. 형식:
//
//	[
//	  {"symbol":"USD/KRW","asset_class":"FX","market":"OTC","upstream":"fx","active":true},
//	  {"symbol":"101V6000","asset_class":"FUTURE","market":"KRX","upstream":"krx","active":true}
//	]
func LoadFile(path string) ([]Instrument, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("instrument: 파일 읽기 (%s): %w", path, err)
	}
	var items []Instrument
	if err := json.Unmarshal(b, &items); err != nil {
		return nil, fmt.Errorf("instrument: JSON 파싱 (%s): %w", path, err)
	}
	return items, nil
}
