package quote

import (
	"errors"
	"testing"
)

func TestSymbolEntry_Validate(t *testing.T) {
	tests := []struct {
		name    string
		entry   SymbolEntry
		wantErr error // exact sentinel match (errors.Is)
	}{
		{"정상", SymbolEntry{Symbol: "USDKRW", Pair: "USD/KRW", Active: true}, nil},
		{"비활성도 OK", SymbolEntry{Symbol: "JPYKRW", Pair: "JPY/KRW", Active: false}, nil},
		{"비표준 심볼 OK", SymbolEntry{Symbol: "BTCUSDT", Pair: "BTC/USDT", Active: true}, nil},

		{"symbol 빈 값", SymbolEntry{Pair: "USD/KRW"}, ErrSymbolEmpty},
		{"symbol whitespace", SymbolEntry{Symbol: "   ", Pair: "USD/KRW"}, ErrSymbolEmpty},
		{"pair 빈 값", SymbolEntry{Symbol: "USDKRW"}, ErrPairEmpty},
		{"pair 슬래시 없음", SymbolEntry{Symbol: "USDKRW", Pair: "USDKRW"}, ErrPairFormat},
		{"pair 슬래시 양 끝", SymbolEntry{Symbol: "USDKRW", Pair: "/USD/KRW/"}, ErrPairFormat},
		{"pair 슬래시 2개", SymbolEntry{Symbol: "USDKRW", Pair: "USD/KR/W"}, ErrPairFormat},
		{"pair base 빈 값", SymbolEntry{Symbol: "X", Pair: "/KRW"}, ErrPairFormat},
		{"pair quote 빈 값", SymbolEntry{Symbol: "X", Pair: "USD/"}, ErrPairFormat},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.entry.Validate()
			if tc.wantErr == nil {
				if err != nil {
					t.Errorf("정상 entry 인데 err = %v", err)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("err = %v, want %v", err, tc.wantErr)
			}
		})
	}
}
