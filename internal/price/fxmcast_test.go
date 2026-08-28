package price

import "testing"

func TestNormalizeFXSymbol(t *testing.T) {
	cases := map[string]string{
		"USD/KRW": "USDKRW",
		"EUR/USD": "EURUSD",
		"USDKRW":  "USDKRW", // 이미 concat — 무변경
		"USD KRW": "USDKRW",
		"USD_KRW": "USDKRW",
	}
	for in, want := range cases {
		if got := normalizeFXSymbol(in); got != want {
			t.Errorf("normalizeFXSymbol(%q)=%q, want %q", in, got, want)
		}
	}
}
