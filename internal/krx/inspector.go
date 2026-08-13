package krx

import (
	_ "embed"
	"net/http"
)

// inspector.html — mci-edge-krx 내장 시세 확인 화면 (GET /). 정식 web 프론트 전,
// 브라우저로 http://<host>:8085/ 를 열어 fut/bond trade·book fan-out 을 실시간
// 표로 확인 (같은 서버의 /v1/subscribe 에 붙음). 외부 의존 0 단일 페이지.
//
//go:embed inspector.html
var inspectorHTML []byte

// ServeInspector — GET / 로 내장 인스펙터 HTML 서빙. 정확히 "/" 만 처리하고
// 그 외 경로는 404 (ws /v1/subscribe · /healthz 는 별도 등록이 우선).
func (srv *Server) ServeInspector(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(inspectorHTML)
}
