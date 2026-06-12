package price

import (
	"testing"
	"strings"
)

func TestSplitPairsCSV(t *testing.T) {
	cases := []struct{ in, want string }{
		{"USD/KRW", "USD/KRW"},
		{"USD/KRW,JPY/KRW", "USD/KRW|JPY/KRW"},
		{" USD/KRW , JPY/KRW ", "USD/KRW|JPY/KRW"},
		{"USD/KRW,USD/KRW,JPY/KRW", "USD/KRW|JPY/KRW"}, // dedup
		{"", ""},
		{",,USD/KRW,,", "USD/KRW"},
	}
	for _, c := range cases {
		got := splitPairsCSV(c.in)
		joined := strings.Join(got, "|")
		if joined != c.want {
			t.Errorf("splitPairsCSV(%q) = %v, want %q", c.in, got, c.want)
		}
	}
}
