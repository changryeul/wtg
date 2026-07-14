package tcp

import "testing"

func TestSanitizeTrxc(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		want string
		ok   bool
	}{
		{"공백 패딩", []byte("W3200A01        "), "W3200A01", true},
		{"NUL 패딩 (cs 전문)", append([]byte("W3200A01"), make([]byte, 8)...), "W3200A01", true},
		{"NUL 뒤 쓰레기", append(append([]byte("WECHO"), 0x00), []byte("\xb0\xa1junk")...), "WECHO", true},
		{"선행 공백 + NUL", append([]byte("  W1101S01"), 0x00, 0x00, 0x00, 0x00, 0x00, 0x00), "W1101S01", true},
		{"전부 NUL", make([]byte, 16), "", false},
		{"비ASCII 시작 (한글 CP949)", []byte("\xb0\xa1\xb0\xa1trxc padding!"), "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := sanitizeTrxc(tc.in)
			if tc.ok != (err == nil) {
				t.Fatalf("err=%v, want ok=%v", err, tc.ok)
			}
			if tc.ok && got != tc.want {
				t.Fatalf("got=%q want=%q", got, tc.want)
			}
		})
	}
}
