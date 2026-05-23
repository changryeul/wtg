package quote

import (
	"errors"
	"strings"
)

// ErrSymbolEmpty, ErrPairEmpty, ErrPairFormat — SymbolEntry 검증 에러 sentinel.
var (
	ErrSymbolEmpty = errors.New("quote: symbol 비어있음")
	ErrPairEmpty   = errors.New("quote: pair 비어있음")
	ErrPairFormat  = errors.New("quote: pair 는 'BASE/QUOTE' 형식이어야 함")
)

// Validate 는 SymbolEntry 의 sanity 검사.
//
// 검사:
//   - Symbol 비어있지 않음
//   - Pair 비어있지 않음
//   - Pair 가 "X/Y" 형식 ("/" 정확히 1개, 양쪽이 비어있지 않음)
//
// 통화 코드의 ISO 4217 검증은 의도적으로 안 함 — 신규 통화나 비표준 심볼
// (USDT 등 가상자산) 가능성 있음.
func (e SymbolEntry) Validate() error {
	if strings.TrimSpace(e.Symbol) == "" {
		return ErrSymbolEmpty
	}
	if e.Pair == "" {
		return ErrPairEmpty
	}
	if !validPair(string(e.Pair)) {
		return ErrPairFormat
	}
	return nil
}

// validPair — "X/Y" 형식 검증 (양쪽 비어있지 않고 / 정확히 1개).
func validPair(s string) bool {
	slash := strings.Index(s, "/")
	if slash <= 0 || slash >= len(s)-1 {
		return false
	}
	// "/" 가 2개 이상이면 거부.
	if strings.Count(s, "/") != 1 {
		return false
	}
	return true
}
