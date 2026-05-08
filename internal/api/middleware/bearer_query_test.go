package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestBearerFromQueryInjects(t *testing.T) {
	mw := BearerFromQuery()
	var seenAuth, seenQuery string
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth = r.Header.Get("Authorization")
		seenQuery = r.URL.RawQuery
	}))
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/subscribe?access_token=abc&room=fx", nil)
	h.ServeHTTP(rr, req)

	if seenAuth != "Bearer abc" {
		t.Errorf("Authorization=%q", seenAuth)
	}
	if seenQuery != "room=fx" {
		t.Errorf("쿼리에서 access_token 제거 실패: %q", seenQuery)
	}
}

// 헤더가 이미 있으면 쿼리 무시.
func TestBearerFromQueryHeaderWins(t *testing.T) {
	mw := BearerFromQuery()
	var seen string
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("Authorization")
	}))
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/x?access_token=fromquery", nil)
	req.Header.Set("Authorization", "Bearer fromheader")
	h.ServeHTTP(rr, req)
	if seen != "Bearer fromheader" {
		t.Errorf("헤더 우선 실패: %q", seen)
	}
}

// 쿼리/헤더 둘 다 비어있으면 그대로 통과 (다음 미들웨어가 401 결정).
func TestBearerFromQueryNoToken(t *testing.T) {
	mw := BearerFromQuery()
	called := false
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		if r.Header.Get("Authorization") != "" {
			t.Error("토큰 없는데 헤더 주입됨")
		}
	}))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/v1/x", nil))
	if !called {
		t.Error("핸들러 호출 안됨")
	}
}
