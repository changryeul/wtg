package ratelimit

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// makeReq — RemoteAddr + 헤더를 채운 테스트용 *http.Request.
func makeReq(method, path, remoteAddr string, headers map[string]string) *http.Request {
	r := httptest.NewRequest(method, path, nil)
	r.RemoteAddr = remoteAddr
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	return r
}

func TestRuleSet_FirstMatchWins(t *testing.T) {
	rs, err := NewRuleSet([]Rule{
		{Pattern: "POST /v1/tx", Rate: 10, Burst: 1},
		{Pattern: "POST /v1/*", Rate: 100, Burst: 1}, // 더 넓은 매칭은 뒤에
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer rs.Stop()

	// POST /v1/tx — 첫 룰 매칭, burst=1 이라 두번째 거부.
	rule, ok := rs.Allow("POST", "/v1/tx", "ip-1")
	if !ok || rule != "POST /v1/tx" {
		t.Errorf("첫 요청: rule=%q ok=%v", rule, ok)
	}
	rule, ok = rs.Allow("POST", "/v1/tx", "ip-1")
	if ok {
		t.Errorf("두번째: 거부되어야 하는데 통과. rule=%q", rule)
	}

	// POST /v1/login — 두번째 룰 (POST /v1/*) 매칭.
	rule, ok = rs.Allow("POST", "/v1/login", "ip-1")
	if !ok || rule != "POST /v1/*" {
		t.Errorf("login: rule=%q ok=%v", rule, ok)
	}
}

func TestRuleSet_MethodFilter(t *testing.T) {
	rs, err := NewRuleSet([]Rule{
		{Pattern: "POST /v1/tx", Rate: 1, Burst: 1},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer rs.Stop()

	// GET /v1/tx — POST 룰 매칭 X, fallback 없으므로 통과.
	rule, ok := rs.Allow("GET", "/v1/tx", "ip-1")
	if !ok || rule != "" {
		t.Errorf("GET /v1/tx: rule=%q ok=%v (통과 + 빈 rule 기대)", rule, ok)
	}
}

func TestRuleSet_FallbackApplied(t *testing.T) {
	rs, err := NewRuleSet([]Rule{
		{Pattern: "POST /v1/tx", Rate: 100, Burst: 1},
	}, &Config{RatePerSec: 1, Burst: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer rs.Stop()

	// 첫 GET 은 fallback (burst=1) 으로 통과.
	rule, ok := rs.Allow("GET", "/v1/x", "ip-1")
	if !ok || rule != "default" {
		t.Errorf("first: rule=%q ok=%v", rule, ok)
	}
	// 두번째 GET — fallback 한도 초과 → 거부.
	rule, ok = rs.Allow("GET", "/v1/x", "ip-1")
	if ok {
		t.Errorf("second: 거부 기대인데 통과. rule=%q", rule)
	}
}

func TestRuleSet_PerRuleIndependentBuckets(t *testing.T) {
	rs, err := NewRuleSet([]Rule{
		{Pattern: "POST /v1/tx", Rate: 1, Burst: 1},
		{Pattern: "GET /v1/ping", Rate: 1, Burst: 1},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer rs.Stop()

	// 같은 IP 라도 룰 별로 독립 버킷.
	if _, ok := rs.Allow("POST", "/v1/tx", "ip-1"); !ok {
		t.Error("tx 첫 토큰 거부")
	}
	if _, ok := rs.Allow("GET", "/v1/ping", "ip-1"); !ok {
		t.Error("ping 첫 토큰 거부 (tx 와 별개 버킷이어야)")
	}
	if _, ok := rs.Allow("POST", "/v1/tx", "ip-1"); ok {
		t.Error("tx 두번째: 거부 기대")
	}
	if _, ok := rs.Allow("GET", "/v1/ping", "ip-1"); ok {
		t.Error("ping 두번째: 거부 기대")
	}
}

func TestRuleSet_GlobMatch(t *testing.T) {
	rs, err := NewRuleSet([]Rule{
		{Pattern: "GET /v1/chart/*", Rate: 1, Burst: 1},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer rs.Stop()

	if _, ok := rs.Allow("GET", "/v1/chart/USD-KRW", "ip-1"); !ok {
		t.Error("glob 매칭 실패: /v1/chart/USD-KRW")
	}
	// path.Match 의 `*` 는 `/` cross 안 함.
	rule, ok := rs.Allow("GET", "/v1/chart/USD/KRW", "ip-2")
	if !ok || rule != "" {
		t.Errorf("nested path: glob 매칭 안 되어야: rule=%q ok=%v", rule, ok)
	}
}

func TestRuleSet_StarPattern(t *testing.T) {
	rs, err := NewRuleSet([]Rule{
		{Pattern: "POST /v1/login", Rate: 1, Burst: 1},
		{Pattern: "*", Rate: 100, Burst: 1},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer rs.Stop()

	// /v1/login — 첫 룰.
	if _, ok := rs.Allow("POST", "/v1/login", "ip"); !ok {
		t.Error("login 첫번째 거부")
	}
	if _, ok := rs.Allow("POST", "/v1/login", "ip"); ok {
		t.Error("login 두번째: 거부 기대")
	}
	// /v1/x — * 매칭.
	if _, ok := rs.Allow("GET", "/v1/x", "ip"); !ok {
		t.Error("* 매칭 통과해야")
	}
}

func TestRuleSet_BadPatternErrors(t *testing.T) {
	cases := []string{"", "POST [invalid"}
	for _, c := range cases {
		_, err := NewRuleSet([]Rule{{Pattern: c, Rate: 1, Burst: 1}}, nil)
		if err == nil {
			t.Errorf("패턴 %q: 에러 기대", c)
		}
	}
}

func TestRuleSet_Rules(t *testing.T) {
	in := []Rule{
		{Pattern: "POST /v1/tx", Rate: 50, Burst: 100},
		{Pattern: "GET /v1/ping", Rate: 1000, Burst: 2000},
	}
	rs, err := NewRuleSet(in, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer rs.Stop()
	out := rs.Rules()
	if len(out) != len(in) {
		t.Fatalf("len=%d, want %d", len(out), len(in))
	}
	for i := range in {
		if out[i] != in[i] {
			t.Errorf("rule[%d] = %+v, want %+v", i, out[i], in[i])
		}
	}
}

func TestUserOrIPKey_PrefersHeader(t *testing.T) {
	keyFn := UserOrIPKey("X-WTG-User")
	r := makeReq("POST", "/v1/tx", "1.2.3.4:5000", map[string]string{
		"X-WTG-User": "alice",
	})
	if got := keyFn(r); got != "user:alice" {
		t.Errorf("got %q, want user:alice", got)
	}
}

func TestUserOrIPKey_FallsBackToIP(t *testing.T) {
	keyFn := UserOrIPKey("X-WTG-User")
	r := makeReq("POST", "/v1/tx", "1.2.3.4:5000", nil)
	if got := keyFn(r); got != "ip:1.2.3.4" {
		t.Errorf("got %q, want ip:1.2.3.4", got)
	}
}

func TestSplitKey(t *testing.T) {
	cases := []struct {
		in        string
		wantKind  string
		wantValue string
	}{
		{"user:alice", "user", "alice"},
		{"ip:1.2.3.4", "ip", "1.2.3.4"},
		{"unprefixed", "ip", "unprefixed"}, // prefix 없으면 보수적으로 ip 취급
		{"unknown:x", "ip", "unknown:x"},   // 알려지지 않은 prefix 도 ip
	}
	for _, c := range cases {
		k, v := SplitKey(c.in)
		if k != c.wantKind || v != c.wantValue {
			t.Errorf("SplitKey(%q) = (%q, %q), want (%q, %q)",
				c.in, k, v, c.wantKind, c.wantValue)
		}
	}
}

func TestMiddlewareRules_DeniesAfterBurstAndCallsMetric(t *testing.T) {
	rs, err := NewRuleSet([]Rule{
		{Pattern: "POST /v1/tx", Rate: 1, Burst: 1},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer rs.Stop()

	var allowedCalls, deniedCalls int
	var lastRule, lastKind string
	mw := MiddlewareRules(rs, UserOrIPKey("X-WTG-User"), MetricsHook{
		OnAllowed: func(rule, kind string) {
			allowedCalls++
			lastRule, lastKind = rule, kind
		},
		OnDenied: func(rule, kind string) { deniedCalls++; lastRule, lastKind = rule, kind },
	})
	hit := 0
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hit++ }))

	r1 := makeReq("POST", "/v1/tx", "1.2.3.4:1", map[string]string{"X-WTG-User": "alice"})
	w1 := httptest.NewRecorder()
	h.ServeHTTP(w1, r1)
	if w1.Code != 200 {
		t.Errorf("첫 요청 status=%d", w1.Code)
	}
	if allowedCalls != 1 || hit != 1 || lastRule != "POST /v1/tx" || lastKind != "user" {
		t.Errorf("첫 요청 metric: allowed=%d hit=%d rule=%q kind=%q",
			allowedCalls, hit, lastRule, lastKind)
	}

	r2 := makeReq("POST", "/v1/tx", "1.2.3.4:1", map[string]string{"X-WTG-User": "alice"})
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, r2)
	if w2.Code != 429 {
		t.Errorf("두번째 status=%d, want 429", w2.Code)
	}
	if deniedCalls != 1 || hit != 1 {
		t.Errorf("거부 metric: denied=%d hit=%d", deniedCalls, hit)
	}
	if got := w2.Body.String(); got == "" || !strings.Contains(got, `"rule":"POST /v1/tx"`) {
		t.Errorf("응답에 룰 정보 없음: %s", got)
	}
}
