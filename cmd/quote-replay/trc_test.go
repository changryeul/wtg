package main

import (
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fixLine 은 mds .trc 형식의 로그 라인을 만든다:
// [YYYY-MM-DD HH:MM:SS.ffffff] ... (8=FIX.4.4^A...^A)
func fixLine(stamp, fixBody string) string {
	return "[2026-01-02 " + stamp + "] RECV 128 bytes (" + fixBody + ")"
}

// wFix 는 35=W 스냅샷 FIX 원문 (SOH 구분) 을 만든다.
func wFix(msgType, symbol string) string {
	fields := []byte("8=FIX.4.4" + soh + "9=80" + soh + "35=" + msgType + soh +
		"49=SMB" + soh + "56=SUB" + soh)
	if symbol != "" {
		fields = append(fields, []byte("55="+symbol+soh)...)
	}
	fields = append(fields, []byte("268=1"+soh+"269=0"+soh+"270=1385.5000"+soh+"10=000"+soh)...)
	return string(fields)
}

func TestExtractFIX(t *testing.T) {
	cases := []struct {
		name string
		line string
		want string // "" = 추출 대상 아님 (nil)
	}{
		{
			name: "35=W 스냅샷 — 로그 prefix 와 닫는 괄호 제거",
			line: fixLine("09:00:01.123456", wFix("W", "USD/KRW")),
			want: wFix("W", "USD/KRW"),
		},
		{
			name: "35=X 증분도 재생 대상",
			line: fixLine("09:00:01.123456", wFix("X", "USD/KRW")),
			want: wFix("X", "USD/KRW"),
		},
		{
			name: "FIX 아닌 로그 라인은 skip",
			line: "[2026-01-02 09:00:01.123456] heartbeat ok",
			want: "",
		},
		{
			name: "55(symbol) 없는 FIX 는 skip — mds 원형과 동일",
			line: fixLine("09:00:01.123456", wFix("W", "")),
			want: "",
		},
		{
			name: "35=A (Logon) 등 시세 외 메시지는 skip",
			line: fixLine("09:00:01.123456", wFix("A", "USD/KRW")),
			want: "",
		},
		{
			name: "닫는 괄호 없는 라인은 라인 끝까지 (CRLF 제거)",
			line: "[2026-01-02 09:00:01.123456] " + wFix("W", "EUR/USD") + "\r\n",
			want: wFix("W", "EUR/USD"),
		},
		{
			name: "빈 라인",
			line: "",
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := extractFIX([]byte(tc.line))
			if tc.want == "" {
				if got != nil {
					t.Fatalf("skip 대상인데 추출됨: %q", got)
				}
				return
			}
			if string(got) != tc.want {
				t.Fatalf("추출 불일치\n got=%q\nwant=%q", got, tc.want)
			}
		})
	}
}

func TestParseStamp(t *testing.T) {
	cases := []struct {
		name string
		line string
		want time.Duration
		ok   bool
	}{
		{
			name: "표준 prefix [YYYY-MM-DD HH:MM:SS.ffffff]",
			line: fixLine("09:00:01.123456", wFix("W", "USD/KRW")),
			want: 9*time.Hour + 1*time.Second + 123456*time.Microsecond,
			ok:   true,
		},
		{
			name: "자정 직후",
			line: fixLine("00:00:00.000001", wFix("W", "USD/KRW")),
			want: 1 * time.Microsecond,
			ok:   true,
		},
		{
			name: "타임스탬프 없는 라인",
			line: "no stamp here 8=FIX.4.4",
			ok:   false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseStamp([]byte(tc.line))
			if ok != tc.ok {
				t.Fatalf("ok=%v, want %v", ok, tc.ok)
			}
			if ok && got != tc.want {
				t.Fatalf("stamp=%v, want %v", got, tc.want)
			}
		})
	}
}

func TestPaceDelay(t *testing.T) {
	cases := []struct {
		name  string
		prev  time.Duration
		cur   time.Duration
		speed float64
		want  time.Duration
	}{
		{"원본 간격 유지 (x1)", 10 * time.Second, 10*time.Second + 500*time.Millisecond, 1.0, 500 * time.Millisecond},
		{"배속 x2 는 절반 대기", 10 * time.Second, 11 * time.Second, 2.0, 500 * time.Millisecond},
		{"역행 (자정 wrap 등) 은 대기 없음", 11 * time.Second, 10 * time.Second, 1.0, 0},
		{"speed 0 = 페이싱 없이 최고속", 10 * time.Second, 20 * time.Second, 0, 0},
		{"동일 시각", 10 * time.Second, 10 * time.Second, 1.0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := paceDelay(tc.prev, tc.cur, tc.speed)
			if got != tc.want {
				t.Fatalf("delay=%v, want %v", got, tc.want)
			}
		})
	}
}

// TestReplayFile 은 임시 .trc 를 로컬 UDP 리스너 2곳으로 재생해
// 원문 바이트 무변형 도달 + 통계를 검증한다 (mds/WTG 동시 송신 시나리오).
func TestReplayFile(t *testing.T) {
	// 수신측 (mds cooker 역 + forwarder 역) 2개
	listeners := make([]*net.UDPConn, 2)
	dests := make([]string, 2)
	for i := range listeners {
		conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		defer conn.Close()
		listeners[i] = conn
		dests[i] = conn.LocalAddr().String()
	}

	msg1 := wFix("W", "USD/KRW")
	msg2 := wFix("X", "EUR/USD")
	trc := fixLine("09:00:01.000001", msg1) + "\n" +
		"[2026-01-02 09:00:01.000002] heartbeat ok\n" + // skip 대상
		fixLine("09:00:01.000003", msg2) + "\n"

	path := filepath.Join(t.TempDir(), "SMB.trc")
	if err := os.WriteFile(path, []byte(trc), 0o644); err != nil {
		t.Fatalf("fixture write: %v", err)
	}

	stats, err := replayFile(replayConfig{
		FilePath: path,
		Dests:    dests,
		Speed:    0, // 테스트는 최고속
	})
	if err != nil {
		t.Fatalf("replayFile: %v", err)
	}
	if stats.Lines != 3 || stats.Sent != 2 || stats.Skipped != 1 {
		t.Fatalf("stats 불일치: %+v (want Lines=3 Sent=2 Skipped=1)", stats)
	}

	// 두 목적지 모두 msg1, msg2 를 원문 그대로 수신해야 한다
	for i, conn := range listeners {
		for _, want := range []string{msg1, msg2} {
			conn.SetReadDeadline(time.Now().Add(2 * time.Second))
			buf := make([]byte, 4096)
			n, _, err := conn.ReadFromUDP(buf)
			if err != nil {
				t.Fatalf("dest[%d] 수신 실패 (want %.20q...): %v", i, want, err)
			}
			if string(buf[:n]) != want {
				t.Fatalf("dest[%d] wire 불일치\n got=%q\nwant=%q", i, buf[:n], want)
			}
		}
	}
}
