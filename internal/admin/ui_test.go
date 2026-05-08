package admin

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// UI 정적 파일이 embed 에 포함되어 / 로 서빙되는지.
func TestUIHandlerServesIndex(t *testing.T) {
	rr := httptest.NewRecorder()
	UIHandler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "WTG Control") {
		t.Errorf("index.html 내용에 'WTG Control' 없음 — embed 누락 의심")
	}
	if !strings.Contains(body, "API 테스터") {
		t.Errorf("API 테스터 메뉴 누락")
	}
	if !strings.Contains(body, "WS 모니터") {
		t.Errorf("WS 모니터 메뉴 누락")
	}
	if !strings.Contains(body, "spark-routes") {
		t.Errorf("sparkline 마크업 누락")
	}
	if !strings.Contains(body, "dash-chart") {
		t.Errorf("Chart.js 캔버스 마크업 누락")
	}
	if !strings.Contains(body, "audit-timeline") {
		t.Errorf("Audit timeline 마크업 누락")
	}
	if !strings.Contains(body, "page-policy") {
		t.Errorf("정책 화면 마크업 누락")
	}
	if !strings.Contains(body, "stream-state") {
		t.Errorf("ws stream 상태 표시 마크업 누락")
	}
	if !strings.Contains(body, "/v1/admin/stream") {
		t.Errorf("stream 클라이언트 코드 누락")
	}
	if !strings.Contains(body, "data-theme-set") {
		t.Errorf("테마 토글 마크업 누락")
	}
	if !strings.Contains(body, "data-theme=\"light\"") || !strings.Contains(body, "--bg-base") {
		t.Errorf("테마 CSS 변수 누락")
	}
	ct := rr.Header().Get("Content-Type")
	if !strings.Contains(ct, "text/html") {
		t.Errorf("Content-Type=%q", ct)
	}
}
