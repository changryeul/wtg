package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// up / down 혼합 fan-out — 살아있는 대상은 up+latency, 죽은 대상은 error.
func TestMciHealthMixed(t *testing.T) {
	alive := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer alive.Close()
	dead := httptest.NewServer(http.HandlerFunc(nil))
	dead.Close() // 즉시 닫음 — connection refused 유도

	h := MciHealth("api=" + alive.URL + ",price=" + dead.URL)
	rr := httptest.NewRecorder()
	h(rr, httptest.NewRequest(http.MethodGet, "/v1/admin/mci-health", nil))

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d", rr.Code)
	}
	var body struct {
		Services []MciHealthEntry `json:"services"`
		Up       int              `json:"up"`
		Total    int              `json:"total"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Total != 2 || body.Up != 1 {
		t.Errorf("up/total: %d/%d, want 1/2", body.Up, body.Total)
	}
	for _, s := range body.Services {
		switch s.Name {
		case "api":
			if !s.Up {
				t.Errorf("api down: %+v", s)
			}
		case "price":
			if s.Up || s.Error == "" {
				t.Errorf("price 가 up 이거나 error 비어있음: %+v", s)
			}
		}
	}
}

// 5xx 응답은 down 으로 판정.
func TestMciHealth5xxIsDown(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer bad.Close()

	h := MciHealth("svc=" + bad.URL)
	rr := httptest.NewRecorder()
	h(rr, httptest.NewRequest(http.MethodGet, "/v1/admin/mci-health", nil))
	var body struct {
		Up int `json:"up"`
	}
	_ = json.NewDecoder(rr.Body).Decode(&body)
	if body.Up != 0 {
		t.Errorf("5xx 인데 up=%d", body.Up)
	}
}

// 형식 오류 spec 은 skip, 전부 오류면 default 목록 fallback.
func TestParseMciTargets(t *testing.T) {
	got := parseMciTargets("a=http://x, ,broken,b=http://y")
	if len(got) != 2 || got[0].Name != "a" || got[1].Name != "b" {
		t.Errorf("parse: %+v", got)
	}
	if len(parseMciTargets("")) != 0 {
		t.Error("빈 spec 은 빈 slice")
	}
}
