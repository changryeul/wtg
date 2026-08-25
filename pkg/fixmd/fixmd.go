// Package fixmd — FIX 4.4 MarketDataSnapshotFullRefresh(35=W) top-of-book 파서.
//
// quote-forwarder(UDP 수신) 와 mci-price 의 FX multicast 수신부가 공유한다. hot-path
// 최적화(alloc 최소 — Sym 문자열 1개, struct/slice 미할당). 269(MDEntryType)로 bid/ask/
// trade 를 구분하고 270(px)/271(size)을 추출한다. avail(가용수량)=bid/ask entry 의 271.
package fixmd

import "strconv"

// SOH — FIX field separator.
const SOH = 0x01

// Snapshot — 35=W 에서 뽑은 top-of-book + 체결.
type Snapshot struct {
	Sym     string
	Bid     float64
	Ask     float64
	Last    float64 // 시장 체결가 (269=2 Trade)
	LastQty float64 // 체결 수량 (Trade 의 271)
	BidSize float64 // bid 가용수량 avail (269=0 의 271)
	AskSize float64 // ask 가용수량 avail (269=1 의 271)
}

// ParseSnapshot — 35=W wire 에서 top-of-book 추출. ok=false 면 유효 호가 아님
// (Sym 있고 bid>0 && ask>=bid). 269 순서로 270/271 을 entry 에 귀속.
func ParseSnapshot(buf []byte) (s Snapshot, ok bool) {
	var entryType byte
	start := 0
	for i := 0; i <= len(buf); i++ {
		if i < len(buf) && buf[i] != SOH {
			continue
		}
		field := buf[start:i]
		start = i + 1
		eq := indexByte(field, '=')
		if eq < 1 {
			continue
		}
		tag := field[:eq]
		val := field[eq+1:]
		switch {
		case len(tag) == 2 && tag[0] == '5' && tag[1] == '5': // 55 Symbol
			if s.Sym == "" {
				s.Sym = string(val)
			}
		case len(tag) == 3 && tag[0] == '2' && tag[1] == '6' && tag[2] == '9': // 269 MDEntryType
			if len(val) >= 1 {
				entryType = val[0]
			}
		case len(tag) == 3 && tag[0] == '2' && tag[1] == '7' && tag[2] == '0': // 270 MDEntryPx
			f, err := strconv.ParseFloat(string(val), 64)
			if err != nil {
				continue
			}
			switch entryType {
			case '0':
				if s.Bid == 0 {
					s.Bid = f
				}
			case '1':
				if s.Ask == 0 {
					s.Ask = f
				}
			case '2':
				if s.Last == 0 {
					s.Last = f
				}
			}
		case len(tag) == 3 && tag[0] == '2' && tag[1] == '7' && tag[2] == '1': // 271 MDEntrySize
			f, err := strconv.ParseFloat(string(val), 64)
			if err != nil {
				continue
			}
			switch entryType {
			case '0': // bid avail
				if s.BidSize == 0 {
					s.BidSize = f
				}
			case '1': // ask avail
				if s.AskSize == 0 {
					s.AskSize = f
				}
			case '2': // trade qty
				if s.LastQty == 0 {
					s.LastQty = f
				}
			}
		}
	}
	if s.Sym != "" && s.Bid > 0 && s.Ask >= s.Bid {
		ok = true
	}
	return
}

// indexByte — bytes.IndexByte 인라인 (표준 라이브러리 의존 회피, 컴파일러 inline).
func indexByte(b []byte, c byte) int {
	for i := 0; i < len(b); i++ {
		if b[i] == c {
			return i
		}
	}
	return -1
}
