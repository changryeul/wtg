package policy

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// admin 측 stub 이 State JSON 을 반환하면 mci-api 측 Engine 이 ApplyRemote 로
// kill switch 를 받아 Check 가 차단하는지.
func TestHTTPPollAppliesAdminState(t *testing.T) {
	var hits atomic.Int32
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		st := State{KillSwitch: true, BlockedSymbols: []string{"USDKRW"}}
		_ = json.NewEncoder(w).Encode(st)
	}))
	defer stub.Close()

	eng := NewEngine(nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	err := StartHTTPPoll(ctx, eng, HTTPPollOptions{
		URL:      stub.URL,
		Interval: 50 * time.Millisecond,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		st := eng.State()
		if st.KillSwitch {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !eng.State().KillSwitch {
		t.Fatalf("kill switch 가 ApplyRemote 로 전파되지 않음 (hits=%d)", hits.Load())
	}

	// Check 도 차단해야.
	d := eng.Check(Request{Usid: "u", Channel: "Web", RoutingKey: "ANY"})
	if d.Allowed {
		t.Errorf("kill switch 활성인데 통과")
	}
	if d.Reason != ReasonKillSwitch {
		t.Errorf("Reason=%q want=%q", d.Reason, ReasonKillSwitch)
	}
}

func TestHTTPPollHandles500(t *testing.T) {
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer stub.Close()

	eng := NewEngine(nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := StartHTTPPoll(ctx, eng, HTTPPollOptions{
		URL: stub.URL, Interval: 30 * time.Millisecond,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)
	// 500 이 와도 Engine 은 default 상태 유지 — 일시 장애로 정책이 풀리지 않아야.
	if eng.State().KillSwitch {
		t.Error("500 응답인데 임의 변경됨")
	}
}

func TestSanitizePollURL(t *testing.T) {
	cases := map[string]string{
		"":                            "",
		"http://x/":                   "http://x",
		"http://x///":                 "http://x",
		"  http://x/v1/admin/policy ": "http://x/v1/admin/policy",
		"http://x/v1/admin/policy":    "http://x/v1/admin/policy",
	}
	for in, want := range cases {
		if got := SanitizePollURL(in); got != want {
			t.Errorf("SanitizePollURL(%q)=%q want %q", in, got, want)
		}
	}
}
